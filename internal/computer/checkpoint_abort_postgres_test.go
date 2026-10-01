package computer_test

import (
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/computertest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"uuid"
)

func captureAbortFixture(t *testing.T, idle bool, setup ...func(runtest.Fixture, runtest.RunLease)) (runtest.Fixture, computer.CheckpointRef, computer.CheckpointManifest, disk.FencingKey) {
	t.Helper()
	f, ref, manifest := computertest.RegisteredCapture(t, idle, setup...)
	key, err := disk.NewFencingKey(make([]byte, disk.FencingKeySize))
	if err != nil {
		t.Fatal(err)
	}
	hash, err := computer.WriterTokenHash(key, ref.InstanceID, uuid.MustParse(manifest.RecoveryPoint.ComputerID), manifest.RecoveryPoint.WriterGeneration)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_token_hash=$2 WHERE id=$1`, ref.InstanceID, hash)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET active_started_at=clock_timestamp(),max_active_duration_ms=3600000 WHERE current_run_lease_id IN (SELECT id FROM run_leases WHERE computer_instance_id=$1)`, ref.InstanceID)
	return f, ref, manifest, key
}

func TestCaptureAbortRetainsSourceUntilPhysicalAcknowledgment(t *testing.T) {
	for _, idle := range []bool{false, true} {
		t.Run(map[bool]string{false: "members", true: "idle"}[idle], func(t *testing.T) {
			f, ref, _, key := captureAbortFixture(t, idle)
			first, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
			if err != nil {
				t.Fatal(err)
			}
			if first.Adopted || first.Acknowledged || first.Checkpoint.Status != "aborted" || first.Instance.AdmissionState != "resuming_capture" || first.Instance.DesiredVersion != ref.DesiredVersion+1 {
				t.Fatalf("abort=%+v", first)
			}
			var held bool
			err = f.Pool.QueryRow(t.Context(), `SELECT i.desired_state='ready' AND i.admission_state='resuming_capture' AND i.reclaimed_at IS NULL AND i.capture_checkpoint_id=c.id AND i.observed_desired_version<i.desired_version AND NOT EXISTS(SELECT 1 FROM computer_checkpoint_runs m JOIN run_waits w ON w.id=m.run_wait_id JOIN run_leases l ON l.id=m.source_run_lease_id WHERE m.checkpoint_id=c.id AND (w.suspension_status<>'checkpointing' OR l.status<>'checkpointing')) FROM computer_checkpoints c JOIN computer_instances i ON i.id=c.source_computer_instance_id WHERE c.id=$1`, ref.CheckpointID).Scan(&held)
			if err != nil || !held {
				t.Fatalf("source held=%v err=%v", held, err)
			}
			second, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
			if err != nil {
				t.Fatal(err)
			}
			if second.Instance.DesiredVersion != first.Instance.DesiredVersion || second.Checkpoint.InvalidatedAt != first.Checkpoint.InvalidatedAt {
				t.Fatal("replay changed receipt")
			}
			for range 2 {
				if _, err := computer.CompleteCaptureAbort(t.Context(), f.Pool, ref, first.Instance.DesiredVersion, nil); err != nil {
					t.Fatal(err)
				}
			}
			var hot bool
			err = f.Pool.QueryRow(t.Context(), `SELECT i.desired_state='ready' AND i.admission_state='open' AND i.capture_checkpoint_id IS NULL AND i.observed_desired_version=i.desired_version AND c.abort_acknowledged_at IS NOT NULL AND NOT EXISTS(SELECT 1 FROM computer_checkpoint_runs m JOIN run_waits w ON w.id=m.run_wait_id JOIN run_leases l ON l.id=m.source_run_lease_id WHERE m.checkpoint_id=c.id AND (w.suspension_status<>'hot' OR w.suspend_checkpoint_id IS NOT NULL OR l.status<>'running')) FROM computer_checkpoints c JOIN computer_instances i ON i.id=c.source_computer_instance_id WHERE c.id=$1`, ref.CheckpointID).Scan(&hot)
			if err != nil || !hot {
				t.Fatalf("same-source hot=%v err=%v", hot, err)
			}
			replay, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
			if err != nil || !replay.Acknowledged || len(replay.Members) != 0 {
				t.Fatalf("ack replay=%+v err=%v", replay, err)
			}
		})
	}
}

func TestCaptureAbortRejectsExpiredSourceAndRollsBack(t *testing.T) {
	for _, query := range []string{`UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, `UPDATE run_leases SET start_deadline_at=created_at,expires_at=clock_timestamp()-interval '1 second' WHERE computer_instance_id=$1`} {
		f, ref, _, key := captureAbortFixture(t, false)
		dbtest.MustExec(t, t.Context(), f.Pool, query, ref.InstanceID)
		if _, err := computer.AbortCapture(t.Context(), f.Pool, key, ref); !errors.Is(err, computer.ErrAuthorityChanged) {
			t.Fatalf("expired ownership=%v", err)
		}
		var status string
		if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM computer_checkpoints WHERE id=$1`, ref.CheckpointID).Scan(&status); err != nil || status != "creating" {
			t.Fatalf("failed abort wrote receipt: %s %v", status, err)
		}
	}
}

func TestCaptureAbortPreservesRenewalAndRequiresSemanticAcknowledgment(t *testing.T) {
	f, ref, _, key := captureAbortFixture(t, false)
	plan, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := computer.ReconcileTargets(t.Context(), db.New(f.Pool), ref.Host, 64)
	if err != nil || len(targets) != 1 || targets[0].Action != computer.ReconcileAbortCapture || targets[0].Capture == nil || targets[0].Capture.Checkpoint.ID != plan.Checkpoint.ID {
		t.Fatalf("abort discovery: %+v %v", targets, err)
	}
	i := plan.Instance
	_, err = computer.RecordInstanceReady(t.Context(), f.Pool, computer.Readiness{Observation: computer.Observation{Instance: computer.InstanceRef{Host: ref.Host, ID: ref.InstanceID, DesiredVersion: i.DesiredVersion}, ExpectedObservedVersion: i.ObservedVersion}, VCPUCount: i.VMVCPUCount, CPUConfigDigest: i.CPUConfigDigest})
	if !errors.Is(err, computer.ErrAuthorityChanged) {
		t.Fatalf("generic ready bypassed abort: %v", err)
	}
	m := plan.Members[0]
	fence := run.ExecutionFence{LeaseID: pgvalue.UUID(uuid.MustParse(m.LeaseID)), WorkerGroupID: pgvalue.UUID(ref.Host.GroupID), WorkerHostID: pgvalue.UUID(ref.Host.HostID), WorkerEpoch: ref.Host.Epoch, LeaseSequence: m.LeaseSequence}
	if err := f.Pool.QueryRow(t.Context(), `SELECT h.claim_version,g.claim_version FROM worker_hosts h JOIN worker_groups g ON g.id=h.worker_group_id WHERE h.id=$1`, ref.Host.HostID).Scan(&fence.HostClaimVersion, &fence.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := run.RenewLease(t.Context(), f.Pool, fence, m.ExpiresAt); err != nil {
		t.Fatalf("existing member cannot renew during abort: %v", err)
	}
	replay, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Members[0].ExpiresAt.Before(m.ExpiresAt) {
		t.Fatal("replay returned older authority")
	}
}

func TestCaptureAbortDoesNotReviveCancelledMember(t *testing.T) {
	f, ref, _, key := captureAbortFixture(t, false)
	plan, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
	if err != nil {
		t.Fatal(err)
	}
	cancelled := plan.Members[0]
	canceler, err := run.NewCanceler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	_, err = canceler.Cancel(t.Context(), run.CancellationRequest{IdempotencyKey: uuid.NewV7().String(), OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RunID: uuid.MustParse(cancelled.RunID)})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !replay.Members[0].Cancelled || replay.Members[1].Cancelled {
		t.Fatalf("wrong cancellation dispositions: %+v", replay.Members)
	}
	if _, err := computer.CompleteCaptureAbort(t.Context(), f.Pool, ref, replay.Instance.DesiredVersion, nil); !errors.Is(err, computer.ErrAuthorityChanged) {
		t.Fatalf("acknowledgment omitted concurrent cancellation: %v", err)
	}
	if _, err := computer.CompleteCaptureAbort(t.Context(), f.Pool, ref, replay.Instance.DesiredVersion, []uuid.UUID{uuid.MustParse(cancelled.LeaseID)}); err != nil {
		t.Fatal(err)
	}
	var terminal bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.terminal_at IS NOT NULL AND l.terminal_at IS NOT NULL FROM runs r JOIN run_leases l ON l.id=$2 WHERE r.id=$1`, cancelled.RunID, cancelled.LeaseID).Scan(&terminal); err != nil || !terminal {
		t.Fatalf("cancelled member revived: %v %v", terminal, err)
	}
}

func TestCaptureAbortReadyWinnerRequiresExactVersion(t *testing.T) {
	f, ref, manifest, objects := computertest.ReadyCapture(t, false)
	computertest.Complete(t, f, ref, manifest, objects)
	key, err := disk.NewFencingKey(make([]byte, disk.FencingKeySize))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
	if err != nil || !plan.Adopted || len(plan.Members) != 0 {
		t.Fatalf("ready winner: %+v %v", plan, err)
	}
	ref.DesiredVersion++
	if _, err := computer.AbortCapture(t.Context(), f.Pool, key, ref); !errors.Is(err, computer.ErrAuthorityChanged) {
		t.Fatalf("foreign capture version accepted: %v", err)
	}
}

func TestCaptureAbortRejectsLeaseExpiredBeforeAcknowledgment(t *testing.T) {
	f, ref, _, key := captureAbortFixture(t, false)
	plan, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=created_at,expires_at=$2 WHERE id=$1`, plan.Members[0].LeaseID, time.Now().Add(-time.Second))
	if _, err := computer.CompleteCaptureAbort(t.Context(), f.Pool, ref, plan.Instance.DesiredVersion, nil); !errors.Is(err, computer.ErrAuthorityChanged) {
		t.Fatalf("expired lease acknowledged: %v", err)
	}
}

func TestCaptureAbortAllowsCancelledProcessCleanupWhileHeld(t *testing.T) {
	for _, abortFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "checkpointing", true: "resuming capture"}[abortFirst], func(t *testing.T) {
			f, ref, _, key := captureAbortFixture(t, false)
			if abortFirst {
				if _, err := computer.AbortCapture(t.Context(), f.Pool, key, ref); err != nil {
					t.Fatal(err)
				}
			}
			var runID, leaseID uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT run_id,source_run_lease_id FROM computer_checkpoint_runs WHERE checkpoint_id=$1 ORDER BY run_id LIMIT 1`, ref.CheckpointID).Scan(&runID, &leaseID); err != nil {
				t.Fatal(err)
			}
			canceler, err := run.NewCanceler(f.Pool)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = canceler.Cancel(t.Context(), run.CancellationRequest{IdempotencyKey: uuid.NewV7().String(), OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RunID: runID}); err != nil {
				t.Fatal(err)
			}
			principal := workergroup.HostPrincipal{HostID: ref.Host.HostID, GroupID: ref.Host.GroupID, Epoch: ref.Host.Epoch}
			writer := computer.WriterRef{EnvironmentID: f.EnvironmentID, InstanceID: ref.InstanceID}
			if err = f.Pool.QueryRow(t.Context(), `SELECT i.writer_generation,h.claim_version,g.claim_version FROM computer_instances i JOIN worker_hosts h ON h.id=i.worker_host_id JOIN worker_groups g ON g.id=h.worker_group_id WHERE i.id=$1`, ref.InstanceID).Scan(&writer.WriterGeneration, &principal.HostClaimVersion, &principal.GroupClaimVersion); err != nil {
				t.Fatal(err)
			}
			process, err := computer.RunCleanup(t.Context(), f.Pool, principal, writer)
			if err != nil || process == nil || process.RunID != runID || process.RunLeaseID != leaseID {
				t.Fatalf("held member cleanup=%+v %v", process, err)
			}
			if err = computer.ReconcileRun(t.Context(), f.Pool, principal, writer, *process); err != nil {
				t.Fatal(err)
			}
			plan, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = computer.CompleteCaptureAbort(t.Context(), f.Pool, ref, plan.Instance.DesiredVersion, []uuid.UUID{leaseID}); err != nil {
				t.Fatal(err)
			}
			var preserved bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT status='cancelled' AND process_reconciled_at IS NOT NULL FROM run_leases WHERE id=$1`, leaseID).Scan(&preserved); err != nil || !preserved {
				t.Fatalf("cleanup proof lost: %v %v", preserved, err)
			}
		})
	}
}

func TestCaptureAbortRetiresAbandonedBlobsWithoutLosingReceipt(t *testing.T) {
	f, ref, manifest, key := captureAbortFixture(t, true)
	if _, err := computer.RegisterCheckpoint(t.Context(), f.Pool, ref, manifest); err != nil {
		t.Fatal(err)
	}
	q := db.New(f.Pool)
	objects, err := q.ListCheckpointObjects(t.Context(), pgvalue.UUID(ref.CheckpointID))
	if err != nil || len(objects) != 4 {
		t.Fatalf("objects=%v %v", objects, err)
	}
	if candidates, err := q.ListAbandonedCasBlobs(t.Context(), 100); err != nil || len(candidates) != 0 {
		t.Fatalf("live capture collectible=%v %v", candidates, err)
	}
	plan, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = computer.CompleteCaptureAbort(t.Context(), f.Pool, ref, plan.Instance.DesiredVersion, nil); err != nil {
		t.Fatal(err)
	}
	// All declared objects are abandoned, including any whose upload completed
	// before failure. Blob retirement never deletes the replay/ownership receipt.
	candidates, err := q.ListAbandonedCasBlobs(t.Context(), 100)
	if err != nil || len(candidates) != len(objects) {
		t.Fatalf("aborted candidates=%v %v", candidates, err)
	}
	for _, object := range objects {
		if n, err := q.RetireAbandonedCasBlob(t.Context(), object.Digest); err != nil || n != 1 {
			t.Fatalf("retirement=%d %v", n, err)
		}
	}
	replay, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
	if err != nil || !replay.Acknowledged || replay.Instance.ReclaimedAt.Valid || replay.Instance.DesiredState != "ready" || replay.Checkpoint.Status != "aborted" {
		t.Fatalf("collector changed source receipt=%+v %v", replay, err)
	}
}

func TestCaptureAbortCompetesWithAdoption(t *testing.T) {
	for attempt := 0; attempt < 4; attempt++ {
		t.Run(fmt.Sprint(attempt), func(t *testing.T) {
			f, ref, manifest, objects := computertest.ReadyCapture(t, false)
			key, err := disk.NewFencingKey(make([]byte, disk.FencingKeySize))
			if err != nil {
				t.Fatal(err)
			}
			hash, err := computer.WriterTokenHash(key, ref.InstanceID, uuid.MustParse(manifest.RecoveryPoint.ComputerID), manifest.RecoveryPoint.WriterGeneration)
			if err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_token_hash=$2 WHERE id=$1`, ref.InstanceID, hash)
			publisher, err := computer.NewPublisher(f.Pool, objects)
			if err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			type abortResult struct {
				plan computer.CaptureAbortPlan
				err  error
			}
			aborted := make(chan abortResult, 1)
			adopted := make(chan error, 1)
			go func() {
				<-start
				plan, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
				aborted <- abortResult{plan, err}
			}()
			go func() { <-start; _, err := publisher.CompleteCheckpoint(t.Context(), ref, manifest); adopted <- err }()
			close(start)
			a, readyErr := <-aborted, <-adopted
			if a.err != nil {
				t.Fatal(a.err)
			}
			var status, desired, admission string
			if err = f.Pool.QueryRow(t.Context(), `SELECT c.status,i.desired_state,i.admission_state FROM computer_checkpoints c JOIN computer_instances i ON i.id=c.source_computer_instance_id WHERE c.id=$1`, ref.CheckpointID).Scan(&status, &desired, &admission); err != nil {
				t.Fatal(err)
			}
			if a.plan.Adopted {
				if readyErr != nil || status != "ready" || desired != "closed" || len(a.plan.Members) != 0 {
					t.Fatalf("adoption winner=%s %s %v %+v", status, desired, readyErr, a.plan)
				}
			} else if !errors.Is(readyErr, computer.ErrAuthorityChanged) || status != "aborted" || desired != "ready" || admission != "resuming_capture" {
				t.Fatalf("abort winner=%s %s %s readiness=%v", status, desired, admission, readyErr)
			}
		})
	}
}

func TestCaptureAbortBackoffStartsAtAcknowledgment(t *testing.T) {
	f, ref, _, key := captureAbortFixture(t, false)
	plan, err := computer.AbortCapture(t.Context(), f.Pool, key, ref)
	if err != nil {
		t.Fatal(err)
	}
	// A slow abort must still get a full retry interval after actual resumption.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_checkpoints SET invalidated_at=clock_timestamp()-interval '1 hour' WHERE id=$1`, ref.CheckpointID)
	if _, err = computer.CompleteCaptureAbort(t.Context(), f.Pool, ref, plan.Instance.DesiredVersion, nil); err != nil {
		t.Fatal(err)
	}
	request := computer.Capture{CheckpointID: uuid.NewV7(), EnvironmentID: f.EnvironmentID, InstanceID: ref.InstanceID, WriterGeneration: plan.Instance.WriterGeneration, MembershipRevision: plan.Instance.MembershipRevision, DesiredVersion: plan.Instance.DesiredVersion}
	begin := func() error {
		return db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error { _, err := computer.BeginCapture(t.Context(), tx, request); return err })
	}
	if err := begin(); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("immediate capture=%v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_checkpoints SET abort_acknowledged_at=clock_timestamp()-$2*interval '1 millisecond' WHERE id=$1`, ref.CheckpointID, computer.CaptureRetryDelay.Milliseconds()+1)
	if err := begin(); err != nil {
		t.Fatalf("capture after backoff=%v", err)
	}
}
