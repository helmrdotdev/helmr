package control

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestParseActorReadAddressRequiresOneAddress(t *testing.T) {
	actorID := uuid.Must(uuid.NewV7())
	for target, want := range map[string]actorReadAddress{
		"/?actor_id=" + actorID.String(): {id: pgvalue.UUID(actorID)},
		"/?actor_key=thread%3A1":         {key: "thread:1"},
	} {
		got, err := parseActorReadAddress(httptest.NewRequest("GET", target, nil))
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if got != want {
			t.Fatalf("%s: address = %+v, want %+v", target, got, want)
		}
	}
	for _, target := range []string{
		"/",
		"/?actor_id=" + actorID.String() + "&actor_key=thread%3A1",
		"/?actor_id=invalid",
		"/?unknown=value",
	} {
		if _, err := parseActorReadAddress(httptest.NewRequest("GET", target, nil)); err == nil {
			t.Fatalf("%s: parseActorReadAddress() succeeded", target)
		}
	}
}

func TestProjectActorStatusCollapsesInternalStates(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	actorID := uuid.Must(uuid.NewV7())
	for state, want := range map[string]api.ActorPublicStatus{
		"open":       api.ActorPublicStatusOpen,
		"closing":    api.ActorPublicStatusOpen,
		"closed":     api.ActorPublicStatusClosed,
		"cancelling": api.ActorPublicStatusCancelled,
		"cancelled":  api.ActorPublicStatusCancelled,
	} {
		got, err := projectActorStatus(actorReadRecord{
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
	failed, err := projectActorStatus(actorReadRecord{
		id: pgvalue.UUID(actorID), state: "failed",
		createdAt: pgvalue.Timestamptz(now), updatedAt: pgvalue.Timestamptz(now),
		failureCode: pgvalue.Text("run_failed"), failureRunID: pgvalue.UUID(runID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != api.ActorPublicStatusFailed ||
		failed.Failure == nil ||
		failed.Failure.RunID != runID.String() {
		t.Fatalf("failed status = %+v", failed)
	}
}
