package session_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/jackc/pgx/v5"
)

func dueImageRefresh(t *testing.T, f preparationFixture) time.Time {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_specs SET refresh_every_ms=60000,next_refresh_at=clock_timestamp()-interval '1 day' WHERE environment_id=$1 AND id=$2`, f.env, f.deployment)
	var due time.Time
	if err := f.pool.QueryRow(t.Context(), `SELECT next_refresh_at FROM image_specs WHERE environment_id=$1 AND id=$2`, f.env, f.deployment).Scan(&due); err != nil {
		t.Fatal(err)
	}
	return due
}

func assertRefreshUnchanged(t *testing.T, f preparationFixture, due time.Time, builds int) {
	t.Helper()
	var unchanged bool
	if err := f.pool.QueryRow(t.Context(), `SELECT next_refresh_at=$3 AND (SELECT count(*) FROM image_builds WHERE environment_id=$1)=$4 FROM image_specs WHERE environment_id=$1 AND id=$2`, f.env, f.deployment, due, builds).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("refresh consumed blocked interval or changed attempts: %v %v", unchanged, err)
	}
}

func TestImageRefreshConcurrentAdmissionAndCadenceCoalescing(t *testing.T) {
	f := newPreparationFixture(t)
	dueImageRefresh(t, f)
	results := make(chan computer.ImageBuild, 8)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			build, err := computer.RefreshImageSpec(t.Context(), f.pool, f.env, f.deployment)
			results <- build
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	admitted := 0
	for err := range errs {
		if err == nil {
			admitted++
		} else if !errors.Is(err, computer.ErrNotReady) {
			t.Fatal(err)
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted %d concurrent refreshes", admitted)
	}
	var first uuid.UUID
	for result := range results {
		if result.ID != uuid.Nil() {
			first = result.ID
		}
	}
	var valid bool
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(p.status='queued' AND p.worker_host_id IS NULL
 AND p.deadline_at>clock_timestamp() AND p.deadline_at<=clock_timestamp()+e.preparation_timeout_ms*interval '1 millisecond'
 AND s.next_refresh_at>clock_timestamp()+interval '55 seconds' AND s.next_refresh_at<=clock_timestamp()+interval '60 seconds')
 FROM image_builds p JOIN environments e ON e.id=p.environment_id JOIN image_specs s ON (s.environment_id,s.id)=(p.environment_id,p.image_spec_id)
 WHERE p.environment_id=$1`, f.env).Scan(&valid); err != nil || !valid {
		t.Fatalf("invalid refresh admission: %v %v", valid, err)
	}
	// A later due interval shares a usable attempt without stacking another job.
	dueImageRefresh(t, f)
	shared, err := computer.RefreshImageSpec(t.Context(), f.pool, f.env, f.deployment)
	if err != nil || shared.ID != first {
		t.Fatalf("live build was not coalesced: %+v %v", shared, err)
	}
	if _, err := computer.RefreshImageSpec(t.Context(), f.pool, f.env, f.deployment); !errors.Is(err, computer.ErrNotReady) {
		t.Fatalf("restart/repeat consumed a future interval: %v", err)
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM image_builds WHERE environment_id=$1`, f.env).Scan(&count); err != nil || count != 1 {
		t.Fatalf("coalescing forked: %d %v", count, err)
	}
}

func TestImageRefreshInactivePolicyAndSecretCannotAdmit(t *testing.T) {
	for _, mode := range []string{"future", "unpromoted", "revoked Secret"} {
		t.Run(mode, func(t *testing.T) {
			f := newPreparationFixture(t)
			due := dueImageRefresh(t, f)
			switch mode {
			case "future":
				due = time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_specs SET next_refresh_at=$3 WHERE environment_id=$1 AND id=$2`, f.env, f.deployment, due)
			case "unpromoted":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_specs SET refresh_every_ms=NULL,next_refresh_at=NULL WHERE environment_id=$1 AND id=$2`, f.env, f.deployment)
			case "revoked Secret":
				if _, err := f.secrets.Revoke(t.Context(), audit.System("test-fixture"), f.env, f.secretID, "before-refresh"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := computer.RefreshImageSpec(t.Context(), f.pool, f.env, f.deployment); !errors.Is(err, computer.ErrNotReady) {
				t.Fatalf("inactive refresh admitted: %v", err)
			}
			if mode != "unpromoted" {
				assertRefreshUnchanged(t, f, due, 0)
			}
		})
	}
}

func TestImageRefreshUnusableAttemptKeepsDueUntilPhysicalStop(t *testing.T) {
	for _, mode := range []string{"expired queued", "expired executor", "lost host", "failed unfenced"} {
		t.Run(mode, func(t *testing.T) {
			f := newPreparationFixture(t)
			build := f.attach(t, f.waiter(t))
			var ref computer.ImageBuildExecutor
			if mode != "expired queued" {
				ref = f.claim(t, build)
			}
			switch mode {
			case "expired queued":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds SET deadline_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, f.env, build.ID)
			case "expired executor":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds SET executor_expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, f.env, build.ID)
			case "lost host":
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status='lost',lost_at=clock_timestamp() WHERE id=$1`, f.worker)
			case "failed unfenced":
				if err := computer.FailImageBuild(t.Context(), f.pool, *f.host(), ref, "failed-start"); err != nil {
					t.Fatal(err)
				}
			}
			due := dueImageRefresh(t, f)
			if _, err := computer.RefreshImageSpec(t.Context(), f.pool, f.env, f.deployment); !errors.Is(err, computer.ErrNotReady) {
				t.Fatalf("unusable attempt consumed interval: %v", err)
			}
			assertRefreshUnchanged(t, f, due, 1)
			if mode == "failed unfenced" {
				if err := computer.ObserveImageBuildStopped(t.Context(), f.pool, *f.host(), preparationIdentity(ref)); err != nil {
					t.Fatal(err)
				}
				next, err := computer.RefreshImageSpec(t.Context(), f.pool, f.env, f.deployment)
				if err != nil || next.ID == build.ID {
					t.Fatalf("physical stop did not unblock due refresh: %+v %v", next, err)
				}
			}
		})
	}
}

func TestImageRefreshAdvanceAndAdmissionRollbackTogether(t *testing.T) {
	f := newPreparationFixture(t)
	due := dueImageRefresh(t, f)
	dbtest.MustExec(t, t.Context(), f.pool, `CREATE FUNCTION reject_refresh_advance() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'reject refresh advance'; END $$;
 CREATE TRIGGER reject_refresh_advance BEFORE UPDATE ON image_specs FOR EACH STATEMENT EXECUTE FUNCTION reject_refresh_advance()`, pgx.QueryExecModeSimpleProtocol)
	if _, err := computer.RefreshImageSpec(t.Context(), f.pool, f.env, f.deployment); err == nil {
		t.Fatal("injected advance failure was ignored")
	}
	assertRefreshUnchanged(t, f, due, 0)
	dbtest.MustExec(t, t.Context(), f.pool, `DROP TRIGGER reject_refresh_advance ON image_specs`)
	if _, err := computer.RefreshImageSpec(t.Context(), f.pool, f.env, f.deployment); err != nil {
		t.Fatal(err)
	}
}

func TestImageRefreshRechecksPolicyAfterEnvironmentWait(t *testing.T) {
	for _, change := range []string{"promotion", "Secret revocation"} {
		t.Run(change, func(t *testing.T) {
			f := newPreparationFixture(t)
			dueImageRefresh(t, f)
			tx, err := f.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			var wg sync.WaitGroup
			t.Cleanup(func() { cancel(); _ = tx.Rollback(context.Background()); wg.Wait() })
			dbtest.MustExec(t, ctx, tx, `SELECT id FROM environments WHERE id=$1 FOR NO KEY UPDATE`, f.env)
			done := make(chan error, 1)
			wg.Go(func() { _, err := computer.RefreshImageSpec(ctx, f.pool, f.env, f.deployment); done <- err })
			waitSessionLifecycleLock(t, f.fixture)
			if change == "promotion" {
				dbtest.MustExec(t, ctx, tx, `UPDATE image_specs SET refresh_every_ms=NULL,next_refresh_at=NULL WHERE environment_id=$1 AND id=$2`, f.env, f.deployment)
			} else if _, err := f.secrets.Revoke(ctx, audit.System("test-fixture"), f.env, f.secretID, "while-refresh-waits"); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, computer.ErrNotReady) {
				t.Fatalf("obsolete policy admitted after lock wait: %v", err)
			}
			var count int
			if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM image_builds WHERE environment_id=$1`, f.env).Scan(&count); err != nil || count != 0 {
				t.Fatalf("obsolete refresh persisted: %d %v", count, err)
			}
		})
	}
}

func TestImageRefreshLifecycleConsumesDueSpec(t *testing.T) {
	f := newPreparationFixture(t)
	dueImageRefresh(t, f)
	for range 2 {
		if _, _, err := dispatch.ReconcileImageBuildLifecycle(t.Context(), f.pool, dispatch.ImageBuildLifecyclePosition{}); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM image_builds WHERE environment_id=$1`, f.env).Scan(&count); err != nil || count != 1 {
		t.Fatalf("lifecycle did not converge on one due build: %d %v", count, err)
	}
}

func TestImageRefreshYieldsToAttachedRotationSuccessor(t *testing.T) {
	p, key := newPreparationPublicationTest(t)
	root := p.capture(t, key)
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "before rotation"); err != nil {
		t.Fatal(err)
	}
	if err := computer.ObserveImageBuildStopped(t.Context(), p.f.pool, *p.f.host(), preparationIdentity(p.ref)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.f.secrets.Rotate(t.Context(), audit.System("test-fixture"), p.f.env, p.f.secretID, []byte("v2"), "before-refresh"); err != nil {
		t.Fatal(err)
	}
	due := dueImageRefresh(t, p.f)
	if _, err := computer.RefreshImageSpec(t.Context(), p.f.pool, p.f.env, p.f.deployment); !errors.Is(err, computer.ErrNotReady) {
		t.Fatalf("refresh preempted attached successor: %v", err)
	}
	assertRefreshUnchanged(t, p.f, due, 1)
	type admission struct {
		build computer.ImageBuild
		err   error
	}
	start := make(chan struct{})
	ensured, refreshed := make(chan admission, 1), make(chan admission, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		<-start
		build, err := computer.EnsureImageBuildSuccessor(t.Context(), p.f.pool, p.f.env, p.ref.ImageBuildID)
		ensured <- admission{build, err}
	})
	wg.Go(func() {
		<-start
		build, err := computer.RefreshImageSpec(t.Context(), p.f.pool, p.f.env, p.f.deployment)
		refreshed <- admission{build, err}
	})
	close(start)
	wg.Wait()
	successor := <-ensured
	racing := <-refreshed
	if successor.err != nil || racing.err != nil && !errors.Is(racing.err, computer.ErrNotReady) || racing.err == nil && racing.build.ID != successor.build.ID {
		t.Fatalf("successor/refresh race: successor=%+v refresh=%+v", successor, racing)
	}
	dueImageRefresh(t, p.f)
	shared, err := computer.RefreshImageSpec(t.Context(), p.f.pool, p.f.env, p.f.deployment)
	if err != nil || shared.ID != successor.build.ID {
		t.Fatalf("refresh did not share admitted successor: %+v %v", shared, err)
	}
}

func TestImageRefreshLifecycleSkipsPermanentlyRevokedSecret(t *testing.T) {
	f := newPreparationFixture(t)
	due := dueImageRefresh(t, f)
	if _, err := f.secrets.Revoke(t.Context(), audit.System("test-fixture"), f.env, f.secretID, "no-refresh"); err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SELECT id FROM environments WHERE id=$1 FOR NO KEY UPDATE`, f.env)
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	if _, _, err := dispatch.ReconcileImageBuildLifecycle(ctx, f.pool, dispatch.ImageBuildLifecyclePosition{}); err != nil {
		t.Fatal(err)
	}
	assertRefreshUnchanged(t, f, due, 0)
}
