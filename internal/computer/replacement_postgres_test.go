package computer_test

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// replacementFixture is a captured Computer's live Instance with resident
// members, and a new deployment whose program the Computer admits.
func replacementFixture(t *testing.T) (runtest.Fixture, computer.Capture, computer.ReplacementRef) {
	t.Helper()
	f, old, _, capture := computertest.Capture(t)
	next := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO deployments(id,org_id,project_id,environment_id,version,bundle_digest,runtime_artifact_digest,program_artifact_id,program_index_digest,queue_config) SELECT $2,org_id,project_id,environment_id,'next-program','sha256:'||repeat('a',64),runtime_artifact_digest,program_artifact_id,program_index_digest,queue_config FROM deployments WHERE id=$1`, f.DeploymentID, next)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO deployment_definitions(id,environment_id,deployment_id,kind,declared_id,manifest_version,manifest,manifest_digest,computer_spec_id) SELECT gen_random_uuid(),environment_id,$2,kind,declared_id,manifest_version,manifest,manifest_digest,computer_spec_id FROM deployment_definitions WHERE deployment_id=$1`, f.DeploymentID, next)
	var computerID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM runs WHERE id=$1`, old.RunID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	return f, capture, computer.ReplacementRef{EnvironmentID: f.EnvironmentID, ComputerID: computerID, DeploymentID: next}
}

// releaseMembers ends the resident members so nothing holds the Computer.
func releaseMembers(t *testing.T, f runtest.Fixture, capture computer.Capture, ref computer.ReplacementRef) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='cancelled',terminal_at=now(),terminal_reason_code='cancelled',process_reconciled_at=now() WHERE computer_instance_id=$1`, capture.InstanceID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='cancelled',terminal_at=now(),failure='{"code":"cancelled","message":"Cancelled","details":{}}',current_run_lease_id=NULL,active_started_at=NULL WHERE computer_id=$1`, ref.ComputerID)
}

// lockReplacement runs LockReplacement and then step in one transaction,
// and commits when both succeed.
func lockReplacement(t *testing.T, ctx context.Context, f runtest.Fixture, ref computer.ReplacementRef, step func(computer.Replacement) error) (computer.ReplacementKind, error) {
	t.Helper()
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	replacement, err := computer.LockReplacement(ctx, tx, ref)
	if err != nil {
		return 0, err
	}
	if step != nil {
		if err = step(replacement); err != nil {
			return replacement.Kind(), err
		}
	}
	return replacement.Kind(), tx.Commit(ctx)
}

func captureStep(r computer.Replacement) error {
	_, err := r.Capture(context.Background())
	return err
}

// The capture's supply check ranks after its program and member checks: a
// replacement on a lost host that members still hold is blocked, not
// changed, and only a clear Computer reports the lost host.
func TestReplacementRanksLostHostAfterMemberChecks(t *testing.T) {
	f, request, ref := replacementFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET status='lost',lost_at=now() WHERE id=$1`, f.WorkerID)
	if _, err := lockReplacement(t, t.Context(), f, ref, nil); !errors.Is(err, computer.ErrReplacementBlocked) {
		t.Fatalf("held replacement on lost host=%v", err)
	}
	releaseMembers(t, f, request, ref)
	if _, err := lockReplacement(t, t.Context(), f, ref, nil); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("clear replacement on lost host=%v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET status='active',lost_at=NULL WHERE id=$1`, f.WorkerID)
	kind, err := lockReplacement(t, t.Context(), f, ref, captureStep)
	if err != nil || kind != computer.ReplacementCapture {
		t.Fatalf("capture replacement=%v %v", kind, err)
	}
	var captured bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT capture_checkpoint_id IS NOT NULL FROM computer_instances WHERE id=$1`, request.InstanceID).Scan(&captured); err != nil || !captured {
		t.Fatalf("replacement did not capture: %v %v", captured, err)
	}
	if _, err = lockReplacement(t, t.Context(), f, ref, func(r computer.Replacement) error { return r.Promote(t.Context()) }); err == nil {
		t.Fatal("capture replacement promoted")
	}
}

// A promotion locks the Computer and the checkpoint's source Instance before
// the checkpoint, then commits the checkpoint's disk as the Computer's head.
func TestReplacementPromotionLocksSourceInstanceBeforeCheckpoint(t *testing.T) {
	f, request, ref := replacementFixture(t)
	releaseMembers(t, f, request, ref)
	if _, err := lockReplacement(t, t.Context(), f, ref, captureStep); err != nil {
		t.Fatal(err)
	}
	var checkpointID pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT capture_checkpoint_id FROM computer_instances WHERE id=$1`, request.InstanceID).Scan(&checkpointID); err != nil {
		t.Fatal(err)
	}
	cp, err := db.New(f.Pool).LockComputerCheckpoint(t.Context(), db.LockComputerCheckpointParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ComputerID: pgvalue.UUID(ref.ComputerID), CheckpointID: checkpointID})
	if err != nil {
		t.Fatal(err)
	}
	checkpointRef, manifest := computertest.CaptureRequest(t, f, cp)
	cp = computertest.Complete(t, f, checkpointRef, manifest, computertest.PrepareCapture(t, f, checkpointRef, manifest))
	if _, err = lockReplacement(t, t.Context(), f, ref, captureStep); err == nil {
		t.Fatal("replacement before source exclusion")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='closed',observed_desired_version=desired_version,mount_state='unmounted',unmounted_at=now(),terminal_at=now(),reclaimed_at=now(),reclaim_evidence='{"method":"machine_closed"}',terminal_reason_code='checkpointed' WHERE id=$1`, request.InstanceID)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	hold, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(context.Background())
	dbtest.MustExec(t, ctx, hold, `SELECT id FROM computer_instances WHERE id=$1 FOR UPDATE`, request.InstanceID)
	holder := hold.Conn().PgConn().PID()
	type result struct {
		kind computer.ReplacementKind
		err  error
	}
	done := make(chan result, 1)
	go func() {
		kind, err := lockReplacement(t, ctx, f, ref, func(r computer.Replacement) error { return r.Promote(ctx) })
		done <- result{kind, err}
	}()
	for blocked := false; !blocked; {
		if err = f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, holder).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		select {
		case r := <-done:
			t.Fatalf("promotion did not wait for its source Instance: %v %v", r.kind, r.err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	lockedNow := func(query string, id any) bool {
		t.Helper()
		probe, err := f.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer probe.Rollback(context.Background())
		_, err = probe.Exec(ctx, query, id)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			return true
		}
		if err != nil {
			t.Fatal(err)
		}
		return false
	}
	if !lockedNow(`SELECT id FROM computers WHERE id=$1 FOR UPDATE NOWAIT`, ref.ComputerID) {
		t.Fatal("promotion waited for its source Instance before locking the Computer")
	}
	if lockedNow(`SELECT id FROM computer_checkpoints WHERE id=$1 FOR UPDATE NOWAIT`, cp.ID) {
		t.Fatal("promotion locked the checkpoint before its source Instance")
	}
	if err = hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.kind != computer.ReplacementPromotion {
		t.Fatalf("promotion=%v %v", r.kind, r.err)
	}
	var promoted bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT c.head_disk_version_id=cp.private_computer_disk_version_id AND v.status='committed' AND cp.status='invalid' AND cp.invalidation_reason_code='program_replaced' FROM computers c JOIN computer_checkpoints cp ON cp.computer_id=c.id JOIN computer_disk_versions v ON v.id=c.head_disk_version_id WHERE cp.id=$1`, cp.ID).Scan(&promoted); err != nil || !promoted {
		t.Fatalf("checkpoint disk not promoted: %v %v", promoted, err)
	}
	if kind, err := lockReplacement(t, t.Context(), f, ref, nil); err != nil || kind != computer.ReplacementNone {
		t.Fatalf("replacement after promotion=%v %v", kind, err)
	}
}
