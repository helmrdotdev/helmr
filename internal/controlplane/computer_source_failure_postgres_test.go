package controlplane

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"testing"
	"uuid"
)

func TestPublishedComputerFailureSettlesSessionAfterReclaim(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "cancel"}[cancel], func(t *testing.T) {
			f := newQueuedActorCheckpointFixture(t, json.RawMessage(`"queued"`))
			var revision int64
			if err := f.Pool.QueryRow(t.Context(), `SELECT revision FROM runs WHERE id=$1`, f.runID).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			reservation, err := f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: revision})
			if err != nil {
				t.Fatal(err)
			}
			failed := db.MarkRuntimeInstanceFailedParams{ID: reservation.RuntimeInstanceID, WorkerInstanceID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1, ReasonCode: pgvalue.Text(workerapi.RuntimeFailureComputerSource), Error: []byte(`{"message":"missing retained root"}`)}
			var group uuid.UUID
			if err = f.Pool.QueryRow(t.Context(), `SELECT desired_version,observed_version,worker_group_id FROM runtime_instances WHERE id=$1`, failed.ID).Scan(&failed.DesiredVersion, &failed.ExpectedObservedVersion, &group); err != nil {
				t.Fatal(err)
			}
			// A stale report must not poison the published head.
			stale := failed
			stale.ExpectedObservedVersion++
			if _, err = f.server.markRuntimeInstanceFailed(t.Context(), group, stale); err == nil {
				t.Fatal("stale failure accepted")
			}
			var poisoned bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT recovery_failure IS NOT NULL FROM computers WHERE id=$1`, f.workspaceID).Scan(&poisoned); err != nil || poisoned {
				t.Fatalf("stale report poisoned Computer: %v %v", poisoned, err)
			}
			if _, err = f.server.markRuntimeInstanceFailed(t.Context(), group, failed); err != nil {
				t.Fatal(err)
			}
			if _, err = f.server.markRuntimeInstanceFailed(t.Context(), group, failed); err == nil {
				t.Fatal("stale duplicate accepted")
			}
			var status, reason string
			var count int
			if err = f.Pool.QueryRow(t.Context(), `SELECT r.status,c.recovery_failure->>'code',c.recovery_preparation_count FROM runs r JOIN computers c ON c.id=r.workspace_id WHERE r.id=$1`, f.runID).Scan(&status, &reason, &count); err != nil {
				t.Fatal(err)
			}
			if status != "system_failed" || reason != "computer_source_unavailable" || count != 0 {
				t.Fatalf("failure=%s %s count=%d", status, reason, count)
			}
			if cancel {
				if _, err = f.server.applySessionCancel(t.Context(), session.ControlRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}}); err != nil {
					t.Fatal(err)
				}
			}
			r, _ := session.NewReconciler(f.Pool)
			if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || !deferred {
				t.Fatalf("before reclaim=%v %v", deferred, err)
			}
			p := db.ReclaimFailedRuntimeInstanceParams{ID: failed.ID, WorkerInstanceID: failed.WorkerInstanceID, WorkerEpoch: 1, CleanupProof: []byte(`{"method":"host_reconciled"}`)}
			if err = f.Pool.QueryRow(t.Context(), `SELECT desired_version,observed_version FROM runtime_instances WHERE id=$1`, p.ID).Scan(&p.DesiredVersion, &p.ExpectedObservedVersion); err != nil {
				t.Fatal(err)
			}
			if _, err = db.New(f.Pool).ReclaimFailedRuntimeInstance(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if deferred, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || deferred {
					t.Fatalf("after reclaim=%v %v", deferred, err)
				}
			}
			var computer, turn string
			var owned bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT s.status,coalesce(s.failure->>'code',''),c.status,c.owner_session_id IS NOT NULL,(SELECT status FROM session_turns WHERE session_id=s.id AND sequence=1) FROM sessions s JOIN computers c ON c.id=s.workspace_id WHERE s.id=$1`, f.sessionID).Scan(&status, &reason, &computer, &owned, &turn); err != nil {
				t.Fatal(err)
			}
			expected := "failed"
			if cancel {
				expected = "closed"
			}
			if status != expected || computer != "recovery_required" || owned || turn != "cancelled" {
				t.Fatalf("settled=%s %s %s owned=%v turn=%s", status, reason, computer, owned, turn)
			}
			if !cancel && reason != "computer_source_unavailable" {
				t.Fatal(reason)
			}
			var detail string
			if err = f.Pool.QueryRow(t.Context(), `SELECT recovery_failure->'details'->>'message' FROM computers WHERE id=$1`, f.workspaceID).Scan(&detail); err != nil || detail != "missing retained root" {
				t.Fatalf("cause=%s %v", detail, err)
			}
		})
	}
}

func TestPublishedComputerFailureTransactionRollback(t *testing.T) {
	f := newQueuedActorCheckpointFixture(t, nil)
	var revision int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT revision FROM runs WHERE id=$1`, f.runID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	reservation, err := f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: revision})
	if err != nil {
		t.Fatal(err)
	}
	p := db.MarkRuntimeInstanceFailedParams{ID: reservation.RuntimeInstanceID, WorkerInstanceID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1, ReasonCode: pgvalue.Text(workerapi.RuntimeFailureComputerSource), Error: []byte(`{"message":"missing root"}`)}
	var group uuid.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT desired_version,observed_version,worker_group_id FROM runtime_instances WHERE id=$1`, p.ID).Scan(&p.DesiredVersion, &p.ExpectedObservedVersion, &group); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_runtime_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.observed_state='failed' THEN RAISE EXCEPTION 'injected failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_runtime_failure BEFORE UPDATE ON runtime_instances FOR EACH ROW EXECUTE FUNCTION reject_runtime_failure()`)
	if _, err = f.server.markRuntimeInstanceFailed(t.Context(), group, p); err == nil {
		t.Fatal("injected failure ignored")
	}
	var clean bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT c.recovery_failure IS NULL AND c.status='active' AND r.observed_state='allocated' FROM runtime_instances r JOIN computers c ON c.id=r.workspace_id WHERE r.id=$1`, p.ID).Scan(&clean); err != nil || !clean {
		t.Fatalf("partial failure=%v %v", clean, err)
	}
}

func TestPublishedComputerFailureRejectsAlreadyStartedRuntime(t *testing.T) {
	f := newQueuedActorCheckpointFixture(t, nil)
	f.placeAndStart(t)
	p := db.MarkRuntimeInstanceFailedParams{WorkerInstanceID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1, ReasonCode: pgvalue.Text(workerapi.RuntimeFailureComputerSource), Error: []byte(`{"message":"late source failure"}`)}
	var group uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id,desired_version,observed_version,worker_group_id FROM runtime_instances WHERE workspace_id=$1 AND reclaimed_at IS NULL`, f.workspaceID).Scan(&p.ID, &p.DesiredVersion, &p.ExpectedObservedVersion, &group); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.markRuntimeInstanceFailed(t.Context(), group, p); err == nil {
		t.Fatal("late preparation report accepted")
	}
	var healthy bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT c.recovery_failure IS NULL AND c.status='active' AND r.observed_state='ready' FROM runtime_instances r JOIN computers c ON c.id=r.workspace_id WHERE r.id=$1`, p.ID).Scan(&healthy); err != nil || !healthy {
		t.Fatalf("live Runtime poisoned: %v %v", healthy, err)
	}
}

func TestPublishedComputerFailureTerminatesTaskWithoutRetry(t *testing.T) {
	f, work, runtimeID := prepareReservedRuntimeFailure(t)
	server := &Server{db: db.New(f.Pool), tx: f.Pool}
	var computerID, headID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT c.id,c.head_version_id FROM computers c JOIN runs r ON r.workspace_id=c.id WHERE r.id=$1`, work.RunID).Scan(&computerID, &headID); err != nil {
		t.Fatal(err)
	}
	dbtest.InsertComputerGeneration(t, t.Context(), f.Pool, f.EnvironmentID, computerID, headID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET computer_source_version_id=(SELECT head_version_id FROM computers WHERE id=runtime_instances.workspace_id) WHERE id=$1`, runtimeID)
	p := db.MarkRuntimeInstanceFailedParams{ID: runtimeID, WorkerInstanceID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1, DesiredVersion: 1, ExpectedObservedVersion: 2, ReasonCode: pgvalue.Text(workerapi.RuntimeFailureComputerSource), Error: []byte(`{"message":"missing Task source"}`)}
	var group uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT worker_group_id FROM runtime_instances WHERE id=$1`, runtimeID).Scan(&group); err != nil {
		t.Fatal(err)
	}
	if _, err := server.markRuntimeInstanceFailed(t.Context(), group, p); err != nil {
		t.Fatal(err)
	}
	var status, computer, reason string
	var owned bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT r.status,c.status,c.owner_run_id IS NOT NULL,r.failure->>'code' FROM runs r JOIN computers c ON c.id=r.workspace_id WHERE r.id=$1`, work.RunID).Scan(&status, &computer, &owned, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "system_failed" || computer != "recovery_required" || owned || reason != "computer_source_unavailable" {
		t.Fatalf("Task failure=%s %s %v %s", status, computer, owned, reason)
	}
}

func TestPublishedComputerFailureRequiresRetainedCurrentHead(t *testing.T) {
	f, work, runtimeID := prepareReservedRuntimeFailure(t)
	server := &Server{db: db.New(f.Pool), tx: f.Pool}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET computer_source_version_id=NULL WHERE id=$1`, runtimeID)
	p := db.MarkRuntimeInstanceFailedParams{ID: runtimeID, WorkerInstanceID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1, DesiredVersion: 1, ExpectedObservedVersion: 2, ReasonCode: pgvalue.Text(workerapi.RuntimeFailureComputerSource), Error: []byte(`{"message":"old root missing"}`)}
	var group uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT worker_group_id FROM runtime_instances WHERE id=$1`, runtimeID).Scan(&group); err != nil {
		t.Fatal(err)
	}
	if _, err := server.markRuntimeInstanceFailed(t.Context(), group, p); err != nil {
		t.Fatal(err)
	}
	var status string
	var clean bool
	var attempts int
	if err := f.Pool.QueryRow(t.Context(), `SELECT c.status,c.recovery_failure IS NULL,r.runtime_preparation_count FROM runs r JOIN computers c ON c.id=r.workspace_id WHERE r.id=$1`, work.RunID).Scan(&status, &clean, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "active" || !clean || attempts != 1 {
		t.Fatalf("unretained head poisoned: %s %v %d", status, clean, attempts)
	}
}
