package agent

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
)

func TestPreparationImagePinCurrentVersionAndReplay(t *testing.T) {
	p, key := newPreparationPublicationTest(t)
	root := p.capture(t, key)
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "certified"); err != nil {
		t.Fatal(err)
	}
	computer := p.f.waiter(t)
	got, err := PinComputerImage(t.Context(), p.f.pool, p.f.env, computer)
	if err != nil || got.PreparationID != p.ref.PreparationID {
		t.Fatalf("pin: %v %v", got, err)
	}
	if _, err = p.f.secrets.Rotate(t.Context(), p.f.env, p.f.secretID, []byte("v2"), "rotate"); err != nil {
		t.Fatal(err)
	}
	replay, err := PinComputerImage(t.Context(), p.f.pool, p.f.env, computer)
	if err != nil || replay.ID != got.ID {
		t.Fatalf("replay: %v %v", replay, err)
	}
	fresh := p.f.waiter(t)
	if _, err = PinComputerImage(t.Context(), p.f.pool, p.f.env, fresh); !errors.Is(err, ErrNotReady) {
		t.Fatalf("stale image: %v", err)
	}
	var unchanged bool
	if err = p.f.pool.QueryRow(t.Context(), `SELECT image_id IS NULL AND initial_root_id IS NULL FROM computers WHERE environment_id=$1 AND id=$2`, p.f.env, fresh).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("partial pin: %v", err)
	}
}

func TestPreparationImageAgeAndDeadline(t *testing.T) {
	p, key := newPreparationPublicationTest(t)
	root := p.capture(t, key)
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "certified"); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), p.f.pool, `UPDATE computer_images SET published_at=clock_timestamp()-interval '2 days' WHERE environment_id=$1`, p.f.env)
	for _, tc := range []struct {
		name    string
		age     bool
		expired bool
		want    bool
	}{{"default", false, false, true}, {"max age", true, false, false}, {"deadline", false, true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			c := p.f.waiter(t)
			if tc.age {
				dbtest.MustExec(t, t.Context(), p.f.pool, `UPDATE computers SET preparation_max_age_ms=86400000 WHERE environment_id=$1 AND id=$2`, p.f.env, c)
			}
			if tc.expired {
				dbtest.MustExec(t, t.Context(), p.f.pool, `UPDATE computers SET preparation_deadline_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND id=$2`, p.f.env, c)
			}
			_, err := PinComputerImage(t.Context(), p.f.pool, p.f.env, c)
			if tc.want && err != nil || !tc.want && !errors.Is(err, ErrNotReady) {
				t.Fatalf("pin: %v", err)
			}
		})
	}
}

func TestPreparationSuccessorOnePerChain(t *testing.T) {
	p, key := newPreparationPublicationTest(t)
	waiting := p.f.waiter(t)
	original := p.f.attach(t, waiting)
	root := p.capture(t, key)
	if _, err := p.f.secrets.Rotate(t.Context(), p.f.env, p.f.secretID, []byte("v2"), "rotate"); err != nil {
		t.Fatal(err)
	}
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "certified"); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsurePreparationSuccessor(t.Context(), p.f.pool, p.f.env, original.ID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("unfenced replacement: %v", err)
	}
	if err := ObservePreparationStopped(t.Context(), p.f.pool, *p.f.host(), p.ref.Identity()); err != nil {
		t.Fatal(err)
	}
	const clients = 8
	results := make(chan Preparation, clients)
	errs := make(chan error, clients)
	var wg sync.WaitGroup
	for range clients {
		wg.Go(func() {
			next, err := EnsurePreparationSuccessor(t.Context(), p.f.pool, p.f.env, original.ID)
			results <- next
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var next Preparation
	for got := range results {
		if next.ID != uuid.Nil() && next.ID != got.ID {
			t.Fatal("duplicate successor")
		}
		next = got
	}
	if receipt := p.f.attach(t, waiting); receipt.ID != original.ID {
		t.Fatal("original receipt changed")
	}
	fresh := p.f.waiter(t)
	if receipt := p.f.attach(t, fresh); receipt.ID != next.ID {
		t.Fatal("fresh demand did not join successor")
	}
	p.ref = p.f.claim(t, next)
	broker, _ := NewPreparationKeyBroker(p.f.pool, preparationWrapper(t))
	key, err := broker.WriteKey(t.Context(), *p.f.host(), p.ref)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key.Key)
	root = p.capture(t, key)
	if err = p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "certified successor"); err != nil {
		t.Fatal(err)
	}
	image, err := PinComputerImage(t.Context(), p.f.pool, p.f.env, waiting)
	if err != nil || image.PreparationID != next.ID {
		t.Fatalf("successor image: %v %v", image, err)
	}
	if receipt := p.f.attach(t, waiting); receipt.ID != original.ID {
		t.Fatal("pin changed original receipt")
	}
	if _, err = p.f.secrets.Rotate(t.Context(), p.f.env, p.f.secretID, []byte("v3"), "rotate-again"); err != nil {
		t.Fatal(err)
	}
	if err = ObservePreparationStopped(t.Context(), p.f.pool, *p.f.host(), p.ref.Identity()); err != nil {
		t.Fatal(err)
	}
	if _, err = EnsurePreparationSuccessor(t.Context(), p.f.pool, p.f.env, next.ID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("third execution: %v", err)
	}
	if _, err = PinComputerImage(t.Context(), p.f.pool, p.f.env, fresh); !errors.Is(err, ErrNotReady) {
		t.Fatalf("stale successor: %v", err)
	}
	// Independent demand may start again, but cannot rescue exhausted attachments.
	independent := p.f.waiter(t)
	unrelated := p.f.attach(t, independent)
	p.ref = p.f.claim(t, unrelated)
	key, err = broker.WriteKey(t.Context(), *p.f.host(), p.ref)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key.Key)
	root = p.capture(t, key)
	if err = p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "new demand image"); err != nil {
		t.Fatal(err)
	}
	if _, err = PinComputerImage(t.Context(), p.f.pool, p.f.env, fresh); !errors.Is(err, ErrNotReady) {
		t.Fatalf("exhausted demand crossed chains: %v", err)
	}
	if _, err = PinComputerImage(t.Context(), p.f.pool, p.f.env, independent); err != nil {
		t.Fatalf("independent demand: %v", err)
	}
}

func TestPreparationSuccessorRequiresOnlyRotationStaleness(t *testing.T) {
	for _, reason := range []string{"current", "age", "rotation-and-age", "revoked", "expired-waiters", "failed"} {
		t.Run(reason, func(t *testing.T) {
			p, key := newPreparationPublicationTest(t)
			root := p.capture(t, key)
			if reason == "failed" {
				if err := FailPreparation(t.Context(), p.f.pool, *p.f.host(), p.ref, "command_failed"); err != nil {
					t.Fatal(err)
				}
			} else if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "certified"); err != nil {
				t.Fatal(err)
			}
			if err := ObservePreparationStopped(t.Context(), p.f.pool, *p.f.host(), p.ref.Identity()); err != nil {
				t.Fatal(err)
			}
			if reason != "current" && reason != "age" {
				if _, err := p.f.secrets.Rotate(t.Context(), p.f.env, p.f.secretID, []byte("v2"), "rotate"); err != nil {
					t.Fatal(err)
				}
			}
			switch reason {
			case "age", "rotation-and-age":
				dbtest.MustExec(t, t.Context(), p.f.pool, `UPDATE computer_images SET published_at=clock_timestamp()-interval '2 days' WHERE environment_id=$1; UPDATE computers SET preparation_max_age_ms=86400000 WHERE environment_id=$1`, pgx.QueryExecModeSimpleProtocol, p.f.env)
			case "revoked":
				if _, err := p.f.secrets.Revoke(t.Context(), p.f.env, p.f.secretID, "revoke"); err != nil {
					t.Fatal(err)
				}
			case "expired-waiters":
				dbtest.MustExec(t, t.Context(), p.f.pool, `UPDATE computers SET preparation_deadline_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND initial_root_id IS NULL`, p.f.env)
			}
			if _, err := EnsurePreparationSuccessor(t.Context(), p.f.pool, p.f.env, p.ref.PreparationID); !errors.Is(err, ErrNotReady) {
				t.Fatalf("replacement for %s: %v", reason, err)
			}
			var count int
			if err := p.f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_preparations WHERE environment_id=$1 AND successor_of=$2`, p.f.env, p.ref.PreparationID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("unexpected successor %d: %v", count, err)
			}
		})
	}
}

func TestPreparationImagePinSerializesSecretMutation(t *testing.T) {
	for _, mutation := range []string{"rotate", "revoke"} {
		t.Run(mutation, func(t *testing.T) {
			p, key := newPreparationPublicationTest(t)
			root := p.capture(t, key)
			if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "certified"); err != nil {
				t.Fatal(err)
			}
			computer := p.f.waiter(t)
			tx, err := p.f.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err = tx.Exec(t.Context(), `SELECT id FROM secrets WHERE environment_id=$1 AND id=$2 FOR UPDATE`, p.f.env, p.f.secretID); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			go func() { _, err := PinComputerImage(ctx, p.f.pool, p.f.env, computer); result <- err }()
			for {
				var waiting bool
				if err = p.f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%SELECT s.id FROM secrets%')`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(time.Millisecond):
				}
			}
			store, err := secret.New(db.New(tx), tx, bytes.Repeat([]byte{17}, 32))
			if err != nil {
				t.Fatal(err)
			}
			if mutation == "rotate" {
				_, err = store.Rotate(ctx, p.f.env, p.f.secretID, []byte("v2"), "rotate")
			} else {
				_, err = store.Revoke(ctx, p.f.env, p.f.secretID, "revoke")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err = <-result; !errors.Is(err, ErrNotReady) {
				t.Fatalf("pin crossed %s: %v", mutation, err)
			}
		})
	}
}

func TestPreparationLifecycleReplacesThenPinsOriginalDemand(t *testing.T) {
	p, key := newPreparationPublicationTest(t)
	computer := p.f.waiter(t)
	original := p.f.attach(t, computer)
	var deadline time.Time
	if err := p.f.pool.QueryRow(t.Context(), `SELECT preparation_deadline_at FROM computers WHERE environment_id=$1 AND id=$2`, p.f.env, computer).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	root := p.capture(t, key)
	if _, err := p.f.secrets.Rotate(t.Context(), p.f.env, p.f.secretID, []byte("v2"), "rotate"); err != nil {
		t.Fatal(err)
	}
	if err := p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "certified"); err != nil {
		t.Fatal(err)
	}
	if err := ObservePreparationStopped(t.Context(), p.f.pool, *p.f.host(), p.ref.Identity()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reconcilePreparationLifecycle(t.Context(), p.f.pool, preparationLifecyclePosition{}); err != nil {
		t.Fatal(err)
	}
	next, err := EnsurePreparationSuccessor(t.Context(), p.f.pool, p.f.env, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	p.ref = p.f.claim(t, next)
	broker, _ := NewPreparationKeyBroker(p.f.pool, preparationWrapper(t))
	key, err = broker.WriteKey(t.Context(), *p.f.host(), p.ref)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key.Key)
	root = p.capture(t, key)
	if err = p.publisher.Publish(t.Context(), *p.f.host(), p.ref, root, "successor"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = reconcilePreparationLifecycle(t.Context(), p.f.pool, preparationLifecyclePosition{}); err != nil {
		t.Fatal(err)
	}
	var attached, producer uuid.UUID
	var retained time.Time
	if err = p.f.pool.QueryRow(t.Context(), `SELECT c.preparation_id,i.preparation_id,c.preparation_deadline_at FROM computers c JOIN computer_images i ON (i.environment_id,i.id)=(c.environment_id,c.image_id) WHERE c.environment_id=$1 AND c.id=$2`, p.f.env, computer).Scan(&attached, &producer, &retained); err != nil || attached != original.ID || producer != next.ID || !retained.Equal(deadline) {
		t.Fatalf("lineage/deadline: %v %v %v %v", attached, producer, retained, err)
	}
	if _, err = p.f.secrets.Revoke(t.Context(), p.f.env, p.f.secretID, "revoke"); err != nil {
		t.Fatal(err)
	}
	var revoked bool
	if err = p.f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM computer_secret_revocations WHERE environment_id=$1 AND computer_id=$2)`, p.f.env, computer).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("successor lineage lost: %v", err)
	}
}

func TestPreparationLifecycleIsolatesBlockedImage(t *testing.T) {
	p := preparationResidentImage(t)
	var blocked uuid.UUID
	if err := p.f.pool.QueryRow(t.Context(), `SELECT id FROM computers WHERE environment_id=$1 AND preparation_id=$2 AND initial_root_id IS NULL`, p.f.env, p.ref.PreparationID).Scan(&blocked); err != nil {
		t.Fatal(err)
	}
	// Give this pending candidate the first key without changing any other owner.
	first := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	dbtest.MustExec(t, t.Context(), p.f.pool, `UPDATE computers SET id=$3 WHERE environment_id=$1 AND id=$2`, p.f.env, blocked, first)
	// Publish another recipe through the real capture pipeline in the same DB.
	// It has its own Secret and must progress after the first candidate times out.
	independent := p.f.fixture
	independent.deployment = uuid.NewV7()
	dbtest.MustExec(t, t.Context(), p.f.pool, `INSERT INTO computer_preparation_specs(environment_id,id,spec_digest,spec,seed) SELECT environment_id,$3,$4,spec,seed FROM computer_preparation_specs WHERE environment_id=$1 AND id=$2`, p.f.env, p.f.deployment, independent.deployment, "sha256:"+strings.Repeat("9", 64))
	other, key := preparationPublicationTestFor(t, preparationFixtureFor(t, independent))
	root := other.capture(t, key)
	if err := other.publisher.Publish(t.Context(), *other.f.host(), other.ref, root, "independent image"); err != nil {
		t.Fatal(err)
	}
	var otherComputer uuid.UUID
	if err := p.f.pool.QueryRow(t.Context(), `SELECT id FROM computers WHERE environment_id=$1 AND preparation_id=$2`, p.f.env, other.ref.PreparationID).Scan(&otherComputer); err != nil {
		t.Fatal(err)
	}
	if _, err := p.f.secrets.Revoke(t.Context(), p.f.env, p.f.secretID, "revoke"); err != nil {
		t.Fatal(err)
	}
	tx, err := p.f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `SELECT id FROM secrets WHERE environment_id=$1 AND id=$2 FOR UPDATE`, p.f.env, p.f.secretID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// An expired per-candidate context is reported, but later keys still execute.
	if _, _, err = reconcilePreparationLifecycle(ctx, p.f.pool, preparationLifecyclePosition{}); err == nil {
		t.Fatal("blocked image did not report timeout")
	}
	var pinned, stopping bool
	if err = p.f.pool.QueryRow(ctx, `SELECT image_id IS NOT NULL FROM computers WHERE environment_id=$1 AND id=$2`, p.f.env, otherComputer).Scan(&pinned); err != nil || !pinned {
		t.Fatalf("independent image starved: %v", err)
	}
	if err = p.f.pool.QueryRow(ctx, `SELECT status='stopping' AND fenced_at IS NULL FROM session_processes WHERE environment_id=$1 AND session_id=$2`, p.f.env, p.f.session).Scan(&stopping); err != nil || !stopping {
		t.Fatalf("revocation starved: %v", err)
	}
}
