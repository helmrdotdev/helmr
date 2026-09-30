package scheduler

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lockRaceTimeout bounds every step of a lock race so that a lock cycle
// PostgreSQL does not detect fails the test instead of the package.
const lockRaceTimeout = 30 * time.Second

var protectedSchedulePlacements = []secretbinding.Placement{{
	Name: "API_TOKEN", Kind: "env", Target: "API_TOKEN", Mode: "protected", AllowedOrigins: []string{"https://example.com"},
}}

// caBarrier pauses the first Computer CA generation inside its creating
// transaction, after that transaction has taken its creation locks.
type caBarrier struct {
	generate func(uuid.UUID, uuid.UUID, time.Time) (secret.ProxyTrust, error)
	ctx      context.Context
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func newCABarrier(t *testing.T, pool *pgxpool.Pool) *caBarrier {
	t.Helper()
	return &caBarrier{
		generate: testProxyTrustGenerator(t, pool),
		ctx:      t.Context(),
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
	}
}

func (b *caBarrier) GenerateProxyTrust(environmentID, computerID uuid.UUID, createdAt time.Time) (secret.ProxyTrust, error) {
	first := false
	b.once.Do(func() { first = true })
	if first {
		close(b.entered)
		select {
		case <-b.release:
		case <-b.ctx.Done():
			return secret.ProxyTrust{}, b.ctx.Err()
		case <-time.After(lockRaceTimeout):
			return secret.ProxyTrust{}, errors.New("Computer CA barrier was not released")
		}
	}
	return b.generate(environmentID, computerID, createdAt)
}

func (b *caBarrier) await(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case <-b.entered:
	case err := <-result:
		t.Fatalf("transaction finished before its barrier: %v", err)
	case <-time.After(lockRaceTimeout):
		t.Fatal("transaction did not reach its barrier")
	}
}

// anyTaskAuthority resolves every scheduled task like daily-report with the
// given Secret placements.
type anyTaskAuthority struct {
	fixedAuthority
	placements []secretbinding.Placement
}

func (a anyTaskAuthority) ResolveScheduledTask(v int32, _ string, manifest, digest, queues []byte) (definition.ScheduledTaskAdmission, error) {
	task, err := a.fixedAuthority.ResolveScheduledTask(v, "daily-report", manifest, digest, queues)
	task.SecretPlacements = a.placements
	return task, err
}

func protectScheduleSecrets(t *testing.T, pool *pgxpool.Pool, schedule db.Schedule) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), pool, `
		UPDATE schedule_secrets
		   SET mode = 'protected', allowed_origins = '{https://example.com}'
		 WHERE schedule_id = $1
	`, schedule.ID)
}

// seedSiblingSchedule adds a claimed schedule for another task of the same
// deployment that selects the same Secret placements as schedule.
func seedSiblingSchedule(t *testing.T, pool *pgxpool.Pool, schedule db.Schedule, declaredID string) db.Schedule {
	t.Helper()
	definitionID := uuid.NewV7()
	id := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO deployment_definitions (
			id, environment_id, deployment_id, kind, declared_id, manifest_version, manifest, manifest_digest
		)
		SELECT $1, environment_id, deployment_id, kind, $2, manifest_version, manifest, manifest_digest
		  FROM deployment_definitions
		 WHERE id = $3
	`, definitionID, declaredID, schedule.DeploymentDefinitionID)
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO schedules (
			id, environment_id, task_declared_id, deployment_definition_id, deployment_id,
			cron_pattern, timezone, status, effective_from, next_fire_at, claimed_by, claim_expires_at
		)
		SELECT $1, environment_id, $2, $3, deployment_id,
		       cron_pattern, timezone, status, effective_from, next_fire_at, claimed_by, claim_expires_at
		  FROM schedules
		 WHERE id = $4
	`, id, declaredID, definitionID, schedule.ID)
	dbtest.MustExec(t, t.Context(), pool, `
		INSERT INTO schedule_secrets (
			schedule_id, environment_id, placement_kind, placement_target, secret_id, mode, allowed_origins
		)
		SELECT $1, environment_id, placement_kind, placement_target, secret_id, mode, allowed_origins
		  FROM schedule_secrets
		 WHERE schedule_id = $2
	`, id, schedule.ID)
	sibling, err := db.New(pool).GetSchedule(t.Context(), db.GetScheduleParams{EnvironmentID: schedule.EnvironmentID, ID: pgvalue.UUID(id)})
	if err != nil {
		t.Fatal(err)
	}
	return sibling
}

func startFire(ctx context.Context, admitter *DBAdmitter, schedule db.Schedule) <-chan error {
	result := make(chan error, 1)
	go func() { result <- admitter.AdmitSchedule(ctx, schedule) }()
	return result
}

func awaitResult(t *testing.T, name string, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(lockRaceTimeout):
		t.Fatalf("%s did not finish; lock cycle was not resolved", name)
		return nil
	}
}

// awaitLockWaiter waits until a backend of this test database whose current
// statement matches the LIKE pattern waits on a heavyweight lock. It fails
// when a contender in results finishes first.
func awaitLockWaiter(t *testing.T, pool *pgxpool.Pool, statement string, results ...<-chan error) {
	t.Helper()
	deadline := time.Now().Add(lockRaceTimeout)
	for time.Now().Before(deadline) {
		var waiters int
		queryCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		err := pool.QueryRow(queryCtx, `
			SELECT count(*)
			  FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND wait_event_type = 'Lock'
			   AND query LIKE $1
		`, statement).Scan(&waiters)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if waiters > 0 {
			return
		}
		for _, result := range results {
			select {
			case err := <-result:
				t.Fatalf("contender finished without waiting on a lock: %v", err)
			default:
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no contender waited on a lock")
}

// pgFailure reports the SQLSTATE and detail of a PostgreSQL error, which for
// a deadlock names the waiting processes.
func pgFailure(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code + ": " + pgErr.Detail
	}
	return ""
}

// assertOneFireComputer checks that the schedule's fires each admitted one
// Run on its own Computer with one disk version and, when protected, one CA.
func assertOneFireComputer(t *testing.T, pool *pgxpool.Pool, schedule db.Schedule, protected bool) {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
		SELECT runs.computer_id,
		       (SELECT count(*) FROM computer_disk_versions WHERE computer_id = runs.computer_id),
		       computers.secret_ca_certificate IS NOT NULL
		  FROM runs
		  JOIN computers ON computers.id = runs.computer_id
		 WHERE runs.schedule_id = $1
	`, schedule.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	fires := 0
	for rows.Next() {
		var computerID uuid.UUID
		var versions int
		var hasCA bool
		if err := rows.Scan(&computerID, &versions, &hasCA); err != nil {
			t.Fatal(err)
		}
		fires++
		if versions != 1 || hasCA != protected {
			t.Fatalf("scheduled Computer %s versions/CA = %d/%t, want 1/%t", computerID, versions, hasCA, protected)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if fires != 1 {
		t.Fatalf("scheduled Runs for %s = %d, want 1", schedule.ID, fires)
	}
}

func environmentComputers(t *testing.T, pool *pgxpool.Pool, schedule db.Schedule) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM computers WHERE environment_id = $1`, schedule.EnvironmentID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// TestScheduleFireAndPublicComputerCreationShareSecretWithoutDeadlock races a
// schedule fire against a public Computer creation without an idempotency
// key that binds the same Secret in the same environment, each pausing while
// it holds its creation locks. Fire first reproduces the deadlock of a fire
// that held its environment FOR UPDATE while it waited for the Secret; public
// first is a regression guard for the reverse order.
func TestScheduleFireAndPublicComputerCreationShareSecretWithoutDeadlock(t *testing.T) {
	for _, first := range []string{"fire", "public"} {
		t.Run(first+" first", func(t *testing.T) {
			pool := openSchedulePostgres(t)
			schedule, digest := seedScheduleAdmission(t, pool)
			protectScheduleSecrets(t, pool, schedule)
			fireCA := newCABarrier(t, pool)
			publicCA := newCABarrier(t, pool)
			if first == "fire" {
				close(publicCA.release)
			} else {
				close(fireCA.release)
			}
			admitter, err := NewDBAdmitter(pool, caScheduleAuthority{fixedAuthority{digest: digest}, protectedSchedulePlacements}, fireCA.GenerateProxyTrust)
			if err != nil {
				t.Fatal(err)
			}
			admitter.now = func() time.Time { return schedule.NextFireAt.Time }
			var scope computer.Scope
			if err := pool.QueryRow(t.Context(), `SELECT org_id, project_id, id FROM environments WHERE id = $1`, schedule.EnvironmentID).
				Scan(&scope.OrgID, &scope.ProjectID, &scope.EnvironmentID); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(t.Context(), 2*lockRaceTimeout)
			defer cancel()
			startPublic := func() <-chan error {
				result := make(chan error, 1)
				go func() {
					_, err := computer.NewCreator(publicCA).Create(ctx, pool, computer.Request{
						Scope:      scope,
						DeclaredID: "scheduler",
						Secrets: []secretbinding.Binding{{
							Name: "API_TOKEN",
							Env:  &secretbinding.Env{Name: "PUBLIC_TOKEN", Mode: "protected", AllowedOrigins: []string{"https://example.com"}},
						}},
					})
					result <- err
				}()
				return result
			}
			var fire, public <-chan error
			if first == "fire" {
				fire = startFire(ctx, admitter, schedule)
				fireCA.await(t, fire)
				public = startPublic()
				awaitLockWaiter(t, pool, "%", public)
				close(fireCA.release)
			} else {
				public = startPublic()
				publicCA.await(t, public)
				fire = startFire(ctx, admitter, schedule)
				awaitLockWaiter(t, pool, "%", fire)
				close(publicCA.release)
			}

			fireErr := awaitResult(t, "schedule fire", fire)
			publicErr := awaitResult(t, "public Computer creation", public)
			if fireErr != nil || publicErr != nil {
				t.Fatalf("schedule fire = %v [%s], public creation = %v [%s]",
					fireErr, pgFailure(fireErr), publicErr, pgFailure(publicErr))
			}
			assertOneFireComputer(t, pool, schedule, true)
			if count := environmentComputers(t, pool, schedule); count != 2 {
				t.Fatalf("Computers = %d, want the scheduled and the public Computer", count)
			}
		})
	}
}

// TestConcurrentScheduleFiresShareSecret races two schedules of one
// environment that select the same Secret. It is a regression guard: fires
// must stay serialized by their environment, or two fires holding binding
// key-share locks would deadlock on each other's Secret lock.
func TestConcurrentScheduleFiresShareSecret(t *testing.T) {
	pool := openSchedulePostgres(t)
	first, digest := seedScheduleAdmission(t, pool)
	protectScheduleSecrets(t, pool, first)
	second := seedSiblingSchedule(t, pool, first, "nightly-report")
	barrier := newCABarrier(t, pool)
	admitter, err := NewDBAdmitter(pool, anyTaskAuthority{fixedAuthority{digest: digest}, protectedSchedulePlacements}, barrier.GenerateProxyTrust)
	if err != nil {
		t.Fatal(err)
	}
	admitter.now = func() time.Time { return first.NextFireAt.Time }

	ctx, cancel := context.WithTimeout(t.Context(), 2*lockRaceTimeout)
	defer cancel()
	firstFire := startFire(ctx, admitter, first)
	barrier.await(t, firstFire)
	secondFire := startFire(ctx, admitter, second)
	awaitLockWaiter(t, pool, "%", secondFire)
	close(barrier.release)

	firstErr := awaitResult(t, "first schedule fire", firstFire)
	secondErr := awaitResult(t, "second schedule fire", secondFire)
	if firstErr != nil || secondErr != nil {
		t.Fatalf("first fire = %v [%s], second fire = %v [%s]",
			firstErr, pgFailure(firstErr), secondErr, pgFailure(secondErr))
	}
	assertOneFireComputer(t, pool, first, true)
	assertOneFireComputer(t, pool, second, true)
	if count := environmentComputers(t, pool, first); count != 2 {
		t.Fatalf("Computers = %d, want one per fire", count)
	}
}

// TestScheduleFireHoldsSecretAuthorityBeforeCreatingComputer is a regression
// guard for the fire's Secret lock order rather than a deadlock
// reproduction. It revokes the fire's Secret while the fire creates its
// Computer CA. The revocation waits on the Secret row the fire already holds,
// and the fire resolves the Secret before its revocation.
func TestScheduleFireHoldsSecretAuthorityBeforeCreatingComputer(t *testing.T) {
	pool := openSchedulePostgres(t)
	schedule, digest := seedScheduleAdmission(t, pool)
	protectScheduleSecrets(t, pool, schedule)
	barrier := newCABarrier(t, pool)
	admitter, err := NewDBAdmitter(pool, caScheduleAuthority{fixedAuthority{digest: digest}, protectedSchedulePlacements}, barrier.GenerateProxyTrust)
	if err != nil {
		t.Fatal(err)
	}
	admitter.now = func() time.Time { return schedule.NextFireAt.Time }
	store, err := secret.New(db.New(pool), pool, bytes.Repeat([]byte{71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	var secretID uuid.UUID
	var generation int64
	if err := pool.QueryRow(t.Context(), `
		SELECT secrets.id, secrets.revocation_generation
		  FROM schedule_secrets JOIN secrets ON secrets.id = schedule_secrets.secret_id
		 WHERE schedule_secrets.schedule_id = $1
	`, schedule.ID).Scan(&secretID, &generation); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*lockRaceTimeout)
	defer cancel()
	fire := startFire(ctx, admitter, schedule)
	barrier.await(t, fire)
	revoke := make(chan error, 1)
	go func() {
		_, err := store.Revoke(ctx, pgvalue.MustUUIDValue(schedule.EnvironmentID), secretID, "revoke-during-fire")
		revoke <- err
	}()
	awaitLockWaiter(t, pool, "%UPDATE secrets%", revoke)
	close(barrier.release)

	fireErr := awaitResult(t, "schedule fire", fire)
	revokeErr := awaitResult(t, "Secret revocation", revoke)
	if fireErr != nil || revokeErr != nil {
		t.Fatalf("schedule fire = %v [%s], revocation = %v [%s]",
			fireErr, pgFailure(fireErr), revokeErr, pgFailure(revokeErr))
	}
	assertOneFireComputer(t, pool, schedule, true)
	assertScheduleAdmissionCounts(t, pool, schedule, 1, 1)
	var resolved int64
	var status string
	if err := pool.QueryRow(t.Context(), `
		SELECT secret_resolutions.revocation_generation, secrets.status
		  FROM secret_resolutions JOIN secrets ON secrets.id = secret_resolutions.secret_id
		 WHERE secret_resolutions.run_id IN (SELECT id FROM runs WHERE schedule_id = $1)
	`, schedule.ID).Scan(&resolved, &status); err != nil {
		t.Fatal(err)
	}
	if resolved != generation || status != "revoked" {
		t.Fatalf("resolved generation/status = %d/%s, want %d/revoked", resolved, status, generation)
	}
}
