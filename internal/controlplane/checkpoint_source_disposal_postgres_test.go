package controlplane

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestPublishedCheckpointSurvivesSourceStopFailurePostgres(t *testing.T) {
	f := newActorCheckpointFixture(t)
	capture := f.capture(t, "first turn")
	f.turn(t, 1, capture, true)
	seq := int64(1)
	waitID := uuid.NewV7()
	params, _ := json.Marshal(workerActorInputWaitParams{SessionID: f.sessionID.String(), AfterInputSequence: seq})
	f.workerCall(t, f.server.workerCreateRunWait, workerapi.CreateRunWaitRequest{CorrelationID: uuid.NewV7().String(), Lease: f.fence(), RunWaitID: waitID.String(), ResumeAttachID: uuid.NewV7().String(), Kind: "actor_input", Params: params, ActorSpeculativeInputSequence: &seq}, nil)
	checkpoint := f.publishWaitCheckpoint(t, waitID, capture)
	sourceID := f.claim.runtime.ID
	failure := workerapi.WorkspaceMountFailRequest{OrgID: f.OrgID.String(), WorkspaceMountID: pgvalue.UUIDString(f.claim.workspaceMount.ID), Error: json.RawMessage(`{"message":"source stop failed"}`)}
	f.workerCall(t, f.server.workerFailWorkspaceMount, failure, nil)
	var desired, observed int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT desired_version,observed_version FROM runtime_instances WHERE id=$1`, sourceID).Scan(&desired, &observed); err != nil {
		t.Fatal(err)
	}
	f.workerCall(t, f.server.workerFailWorkspaceMount, failure, nil)
	var replayObserved int64
	var mountState, mountReason, checkpointState string
	if err := f.Pool.QueryRow(t.Context(), `SELECT m.status,m.terminal_reason_code,c.status,r.observed_version FROM workspace_mounts m JOIN runtime_instances r ON r.id=m.runtime_instance_id JOIN run_checkpoints c ON c.id=$2 WHERE m.id=$1`, f.claim.workspaceMount.ID, uuid.MustParse(checkpoint.CheckpointID)).Scan(&mountState, &mountReason, &checkpointState, &replayObserved); err != nil {
		t.Fatal(err)
	}
	if mountState != "unmounted" || mountReason != "checkpointed" || checkpointState != "ready" || replayObserved != observed {
		t.Fatalf("publication changed after stop failure: %s/%s/%s %d->%d", mountState, mountReason, checkpointState, observed, replayObserved)
	}
	targets, err := f.server.db.ListRuntimeReconcileTargets(t.Context(), db.ListRuntimeReconcileTargetsParams{WorkerInstanceID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerEpoch: 1, RowLimit: 100, ObservationFreshnessSeconds: 300})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, target := range targets {
		if target.ID == sourceID && target.ObservedState == "failed" && !target.ReclaimedAt.Valid {
			found = true
		}
	}
	if !found {
		t.Fatalf("failed source not offered to physical reclaim: %+v", targets)
	}
	_, err = f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.EnqueueOnly, Data: json.RawMessage(`{"sequence":2}`)})
	if err != nil {
		t.Fatal(err)
	}
	reconciler, err := session.NewReconciler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	var turnID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM session_turns WHERE session_id=$1 AND sequence=2`, f.sessionID).Scan(&turnID); err != nil {
		t.Fatal(err)
	}
	if _, err = reconciler.ReconcileInput(t.Context(), f.EnvironmentID, f.sessionID, turnID); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT revision FROM runs WHERE id=$1`, f.runID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	result, err := f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: revision})
	if !errors.Is(err, dispatch.ErrCandidateChanged) || result.RuntimeInstanceID.Valid {
		t.Fatalf("restore before source exclusion: %+v %v", result, err)
	}
	// This receipt simulates successful exact host cleanup, not a real VM stop.
	f.workerCall(t, f.server.workerMarkRuntimeInstanceFailed, workerapi.RuntimeInstanceStateRequest{ID: pgvalue.UUIDString(sourceID), WorkerEpoch: 1, DesiredVersion: desired, ExpectedObservedVersion: observed, CleanupProof: &workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupHostReconciled, CompletedAt: time.Now()}}, nil)
	f.placeAndStart(t)
	f.turn(t, 2, capture, false)
	var state string
	var reclaimed bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT observed_state,reclaimed_at IS NOT NULL FROM runtime_instances WHERE id=$1`, sourceID).Scan(&state, &reclaimed); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || !reclaimed {
		t.Fatalf("failure history lost: %s reclaimed=%t", state, reclaimed)
	}
}
