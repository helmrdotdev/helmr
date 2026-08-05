package controlplane

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestProjectSessionStatusCollapsesInternalStates(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	actorID := uuid.Must(uuid.NewV7())
	for state, want := range map[string]api.SessionStatus{
		"open":       api.SessionStatusOpen,
		"closing":    api.SessionStatusOpen,
		"closed":     api.SessionStatusClosed,
		"cancelling": api.SessionStatusCancelled,
		"cancelled":  api.SessionStatusCancelled,
	} {
		got, err := projectSessionStatus(actorReadRecord{
			id: pgvalue.UUID(actorID), state: state,
			createdAt: pgvalue.Timestamptz(now), updatedAt: pgvalue.Timestamptz(now),
		})
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if got.Status != want {
			t.Fatalf("%s: status = %q, want %q", state, got.Status, want)
		}
	}

	runID := uuid.Must(uuid.NewV7())
	failed, err := projectSessionStatus(actorReadRecord{
		id: pgvalue.UUID(actorID), state: "failed",
		createdAt: pgvalue.Timestamptz(now), updatedAt: pgvalue.Timestamptz(now),
		failureCode: pgvalue.Text("run_failed"), failureRunID: pgvalue.UUID(runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != api.SessionStatusFailed ||
		failed.Failure == nil ||
		failed.Failure.RunID != runID.String() {
		t.Fatalf("failed status = %+v", failed)
	}
}
