package controlplane

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func lostSessionComputer(t *testing.T) *actorCheckpointFixture {
	t.Helper()
	f := newActorCheckpointFixture(t)
	f.turn(t, 1)
	reconciler, registration := actorTokenWait(t, f, session.TurnScope{})
	registration.ActorSpeculativeInputSequence.Int64 = 1
	registration.TurnID.Valid = false
	registration.RunGeneration.Valid = false
	if _, err := reconciler.RegisterWait(t.Context(), registration); err != nil {
		t.Fatal(err)
	}
	lease, err := parseRunLeaseFence(f.fence())
	if err != nil {
		t.Fatal(err)
	}
	wait, err := f.server.requestWorkerRunWaitCheckpoint(t.Context(), f.worker, f.fence(), lease, registration.WaitID)
	if err != nil {
		t.Fatal(err)
	}
	f.workerCall(t, f.server.workerMarkCheckpointFailed, workerapi.CheckpointFailedRequest{Lease: f.fence(), RunWaitID: registration.WaitID.String(), CheckpointID: pgvalue.UUIDString(wait.SuspendCheckpointID), RequestVersion: wait.CheckpointRequestVersion, Error: "capture failed"}, nil)
	f.reportRuntimeClosed(t)
	return f
}
