package controlplane

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"testing"
	"uuid"
)

func TestRecoveryPreparationCompletesBeforeActorExecution(t *testing.T) {
	f := newQueuedActorCheckpointFixture(t, nil)
	episode := uuid.NewV7()
	// Seed an episode already admitted for physical preparation. Automatic
	// lost-Session continuation is a separate state transition.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET recovery_id=$2,recovery_version_id=head_version_id,
		recovery_reason='worker_lost',recovery_started_at=now() WHERE id=$1`, f.workspaceID, episode)
	f.placeAndStart(t)
	var completed bool
	var attempts int
	var id uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT recovery_completed_at IS NOT NULL,recovery_preparation_count,recovery_id FROM computers WHERE id=$1`, f.workspaceID).Scan(&completed, &attempts, &id); err != nil {
		t.Fatal(err)
	}
	if !completed || attempts != 1 || id != episode {
		t.Fatalf("completed=%v attempts=%d id=%s", completed, attempts, id)
	}
}

func TestRecoveryReadyPublicationRollsBackTogether(t *testing.T) {
	f := newQueuedActorCheckpointFixture(t, nil)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET recovery_id=$2,recovery_version_id=head_version_id,recovery_reason='worker_lost',recovery_started_at=now() WHERE id=$1`, f.workspaceID, uuid.NewV7())
	var revision int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT revision FROM runs WHERE id=$1`, f.runID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	reserved, err := f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: revision})
	if err != nil {
		t.Fatal(err)
	}
	p := db.MarkRuntimeInstanceReadyParams{ID: reserved.RuntimeInstanceID, WorkerInstanceID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1, ReservationSeconds: 300}
	if err = f.Pool.QueryRow(t.Context(), `SELECT desired_version,observed_version,vm_vcpu_count,cpu_config_digest FROM runtime_instances WHERE id=$1`, reserved.RuntimeInstanceID).Scan(&p.DesiredVersion, &p.ExpectedObservedVersion, &p.VMVCPUCount, &p.CPUConfigDigest); err != nil {
		t.Fatal(err)
	}

	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_recovery_completion() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.recovery_completed_at IS NOT NULL THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER reject_recovery_completion BEFORE UPDATE ON computers FOR EACH ROW EXECUTE FUNCTION reject_recovery_completion()`)
	if _, err = f.server.markRuntimeInstanceReady(t.Context(), p); err == nil {
		t.Fatal("injected completion failure ignored")
	}
	var state string
	var completed bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT r.observed_state,c.recovery_completed_at IS NOT NULL FROM runtime_instances r JOIN computers c ON c.id=r.workspace_id WHERE r.id=$1`, p.ID).Scan(&state, &completed); err != nil || state != "allocated" || completed {
		t.Fatalf("partial ready: %s %v %v", state, completed, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DROP TRIGGER reject_recovery_completion ON computers`)
	if _, err = f.server.markRuntimeInstanceReady(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT r.observed_state,c.recovery_completed_at IS NOT NULL FROM runtime_instances r JOIN computers c ON c.id=r.workspace_id WHERE r.id=$1`, p.ID).Scan(&state, &completed); err != nil || state != "ready" || !completed {
		t.Fatalf("ready: %s %v %v", state, completed, err)
	}
}

func TestActorRecoveryPreparationExhaustionTerminatesSession(t *testing.T) {
	f := newQueuedActorCheckpointFixture(t, json.RawMessage(`"queued"`))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET recovery_id=$2,recovery_version_id=head_version_id,recovery_reason='worker_lost',recovery_started_at=now() WHERE id=$1`, f.workspaceID, uuid.NewV7())
	var revision int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT revision FROM runs WHERE id=$1`, f.runID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	reservation, err := f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: revision})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET recovery_preparation_count=8 WHERE id=$1`, f.workspaceID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET preparation_expires_at=now()-interval '1 second' WHERE id=$1`, reservation.RuntimeInstanceID)
	if n, err := f.placement.RecoverExpiredRuntimeReservations(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("expiry=%d %v", n, err)
	}
	r, _ := session.NewReconciler(f.Pool)
	if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || !deferred {
		t.Fatalf("before reclaim=%v %v", deferred, err)
	}
	p := db.MarkRuntimeInstanceClosedParams{ID: reservation.RuntimeInstanceID, WorkerInstanceID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1, ReasonCode: pgvalue.Text("preparation_failed"), CleanupProof: []byte(`{"method":"host_reconciled"}`)}
	if err = f.Pool.QueryRow(t.Context(), `SELECT desired_version,observed_version FROM runtime_instances WHERE id=$1`, p.ID).Scan(&p.DesiredVersion, &p.ExpectedObservedVersion); err != nil {
		t.Fatal(err)
	}
	if _, err = db.New(f.Pool).MarkRuntimeInstanceClosed(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || deferred {
		t.Fatalf("after reclaim=%v %v", deferred, err)
	}
	var status, reason, turn string
	if err = f.Pool.QueryRow(t.Context(), `SELECT status,failure->>'code',(SELECT status FROM session_turns WHERE session_id=s.id AND sequence=1) FROM sessions s WHERE id=$1`, f.sessionID).Scan(&status, &reason, &turn); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reason != "computer_recovery_exhausted" || turn != "cancelled" {
		t.Fatalf("exhaustion=%s %s %s", status, reason, turn)
	}
}
func TestInitialActorPreparationExhaustionTerminatesSession(t *testing.T) {
	f := newQueuedActorCheckpointFixture(t, json.RawMessage(`"queued"`))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET runtime_preparation_count=7 WHERE id=$1`, f.runID)
	var revision int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT revision FROM runs WHERE id=$1`, f.runID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	reservation, err := f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: revision})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET preparation_expires_at=now()-interval '1 second' WHERE id=$1`, reservation.RuntimeInstanceID)
	if n, err := f.placement.RecoverExpiredRuntimeReservations(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("expiry=%d %v", n, err)
	}
	r, _ := session.NewReconciler(f.Pool)
	if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || !deferred {
		t.Fatalf("before reclaim=%v %v", deferred, err)
	}
	p := db.MarkRuntimeInstanceClosedParams{ID: reservation.RuntimeInstanceID, WorkerInstanceID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1, ReasonCode: pgvalue.Text("preparation_failed"), CleanupProof: []byte(`{"method":"host_reconciled"}`)}
	if err = f.Pool.QueryRow(t.Context(), `SELECT desired_version,observed_version FROM runtime_instances WHERE id=$1`, p.ID).Scan(&p.DesiredVersion, &p.ExpectedObservedVersion); err != nil {
		t.Fatal(err)
	}
	if _, err = db.New(f.Pool).MarkRuntimeInstanceClosed(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || deferred {
		t.Fatalf("after reclaim=%v %v", deferred, err)
	}
	var status, reason, turn string
	if err = f.Pool.QueryRow(t.Context(), `SELECT status,failure->>'code',(SELECT status FROM session_turns WHERE session_id=s.id AND sequence=1) FROM sessions s WHERE id=$1`, f.sessionID).Scan(&status, &reason, &turn); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reason != "runtime_preparation_failed" || turn != "cancelled" {
		t.Fatalf("exhaustion=%s %s %s", status, reason, turn)
	}
}
func TestCancellationAfterInitialPreparationExhaustionClosesSession(t *testing.T) {
	f := newQueuedActorCheckpointFixture(t, json.RawMessage(`"queued"`))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET runtime_preparation_count=7 WHERE id=$1`, f.runID)
	var revision int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT revision FROM runs WHERE id=$1`, f.runID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	reservation, err := f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: revision})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET preparation_expires_at=now()-interval '1 second' WHERE id=$1`, reservation.RuntimeInstanceID)
	if n, err := f.placement.RecoverExpiredRuntimeReservations(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("expiry=%d %v", n, err)
	}
	if _, err := f.server.applySessionCancel(t.Context(), session.ControlRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}}); err != nil {
		t.Fatal(err)
	}
	r, _ := session.NewReconciler(f.Pool)
	if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || !deferred {
		t.Fatalf("before reclaim=%v %v", deferred, err)
	}
	p := db.MarkRuntimeInstanceClosedParams{ID: reservation.RuntimeInstanceID, WorkerInstanceID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1, ReasonCode: pgvalue.Text("preparation_failed"), CleanupProof: []byte(`{"method":"host_reconciled"}`)}
	if err = f.Pool.QueryRow(t.Context(), `SELECT desired_version,observed_version FROM runtime_instances WHERE id=$1`, p.ID).Scan(&p.DesiredVersion, &p.ExpectedObservedVersion); err != nil {
		t.Fatal(err)
	}
	if _, err = db.New(f.Pool).MarkRuntimeInstanceClosed(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || deferred {
		t.Fatalf("after reclaim=%v %v", deferred, err)
	}
	var status, reason, turn string
	if err = f.Pool.QueryRow(t.Context(), `SELECT status,coalesce(failure->>'code',''),(SELECT status FROM session_turns WHERE session_id=s.id AND sequence=1) FROM sessions s WHERE id=$1`, f.sessionID).Scan(&status, &reason, &turn); err != nil {
		t.Fatal(err)
	}
	if status != "closed" || reason != "" || turn != "cancelled" {
		t.Fatalf("exhaustion=%s %s %s", status, reason, turn)
	}
}

func TestMountedActorPreparationExhaustionReleasesComputer(t *testing.T) {
	f := newQueuedActorCheckpointFixture(t, json.RawMessage(`"queued"`))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET runtime_preparation_count=7 WHERE id=$1`, f.runID)
	var revision int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT revision FROM runs WHERE id=$1`, f.runID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	reservation, err := f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: revision})
	if err != nil {
		t.Fatal(err)
	}
	ready := db.MarkRuntimeInstanceReadyParams{ID: reservation.RuntimeInstanceID, WorkerInstanceID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1, ReservationSeconds: 300}
	if err = f.Pool.QueryRow(t.Context(), `SELECT desired_version,observed_version,vm_vcpu_count,cpu_config_digest FROM runtime_instances WHERE id=$1`, ready.ID).Scan(&ready.DesiredVersion, &ready.ExpectedObservedVersion, &ready.VMVCPUCount, &ready.CPUConfigDigest); err != nil {
		t.Fatal(err)
	}

	if _, err = f.server.markRuntimeInstanceReady(t.Context(), ready); err != nil {
		t.Fatal(err)
	}
	if _, err = f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: revision}); err != nil {
		t.Fatal(err)
	}
	failed := db.MarkRuntimeInstanceFailedParams{ID: ready.ID, WorkerInstanceID: ready.WorkerInstanceID, WorkerEpoch: 1, ReasonCode: pgvalue.Text("runtime_reconcile_failed"), Error: []byte(`{"message":"mount preparation failed"}`)}
	if err = f.Pool.QueryRow(t.Context(), `SELECT desired_version,observed_version FROM runtime_instances WHERE id=$1`, ready.ID).Scan(&failed.DesiredVersion, &failed.ExpectedObservedVersion); err != nil {
		t.Fatal(err)
	}
	var group uuid.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT worker_group_id FROM runtime_instances WHERE id=$1`, ready.ID).Scan(&group); err != nil {
		t.Fatal(err)
	}
	if _, err = f.server.markRuntimeInstanceFailed(t.Context(), group, failed); err != nil {
		t.Fatal(err)
	}
	var mount string
	if err = f.Pool.QueryRow(t.Context(), `SELECT status FROM workspace_mounts WHERE runtime_instance_id=$1`, ready.ID).Scan(&mount); err != nil || mount != "mounting" {
		t.Fatalf("before reclaim mount=%s %v", mount, err)
	}
	r, _ := session.NewReconciler(f.Pool)
	if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || !deferred {
		t.Fatalf("before reclaim=%v %v", deferred, err)
	}
	p := db.ReclaimFailedRuntimeInstanceParams{ID: reservation.RuntimeInstanceID, WorkerInstanceID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1, CleanupProof: []byte(`{"method":"host_reconciled"}`)}
	if err = f.Pool.QueryRow(t.Context(), `SELECT desired_version,observed_version FROM runtime_instances WHERE id=$1`, p.ID).Scan(&p.DesiredVersion, &p.ExpectedObservedVersion); err != nil {
		t.Fatal(err)
	}
	if _, err = db.New(f.Pool).ReclaimFailedRuntimeInstance(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || deferred {
		t.Fatalf("after reclaim=%v %v", deferred, err)
	}
	var status, reason, turn string
	if err = f.Pool.QueryRow(t.Context(), `SELECT status,failure->>'code',(SELECT status FROM session_turns WHERE session_id=s.id AND sequence=1) FROM sessions s WHERE id=$1`, f.sessionID).Scan(&status, &reason, &turn); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reason != "runtime_preparation_failed" || turn != "cancelled" {
		t.Fatalf("exhaustion=%s %s %s", status, reason, turn)
	}
	var unowned bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT m.status,c.owner_session_id IS NULL FROM workspace_mounts m JOIN computers c ON c.id=m.workspace_id WHERE m.runtime_instance_id=$1`, ready.ID).Scan(&mount, &unowned); err != nil || mount != "failed" || !unowned {
		t.Fatalf("reconciled mount=%s unowned=%v %v", mount, unowned, err)
	}

}
