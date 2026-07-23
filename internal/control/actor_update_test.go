package control

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
)

func TestNormalizeActorUpdatePreservesPresenceAndCanonicalizesAnnotations(t *testing.T) {
	expiresAt := time.Date(2030, 2, 3, 4, 5, 6, 7, time.FixedZone("test", 2*60*60))
	normalized, err := normalizeActorUpdate(actorUpdateRequest{
		EnvironmentID:    uuid.Must(uuid.NewV7()),
		ActorDeclaredID:  "operator.v1",
		Address:          actorReadAddress{key: "thread:1"},
		MetadataPresent:  true,
		Metadata:         json.RawMessage(`{"z":1,"a":2}`),
		TagsPresent:      true,
		Tags:             []string{" beta ", "alpha", "beta"},
		ExpiresAtPresent: true,
		ExpiresAt:        &expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(normalized.Metadata) != `{"a":2,"z":1}` {
		t.Fatalf("metadata = %s", normalized.Metadata)
	}
	if len(normalized.Tags) != 2 || normalized.Tags[0] != "alpha" || normalized.Tags[1] != "beta" {
		t.Fatalf("tags = %#v", normalized.Tags)
	}
	if normalized.ExpiresAt == nil || normalized.ExpiresAt.Location() != time.UTC {
		t.Fatalf("expires at = %v", normalized.ExpiresAt)
	}
}

func TestNormalizeActorUpdateSupportsEmptyClearsAndRejectsEmptyMutation(t *testing.T) {
	environmentID := uuid.Must(uuid.NewV7())
	metadata := json.RawMessage(`{}`)
	tags := []string{}
	for _, request := range []actorUpdateRequest{
		{
			EnvironmentID: environmentID, ActorDeclaredID: "operator.v1",
			Address:         actorReadAddress{publicID: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa"},
			MetadataPresent: true, Metadata: metadata,
		},
		{
			EnvironmentID: environmentID, ActorDeclaredID: "operator.v1",
			Address:     actorReadAddress{key: "thread:1"},
			TagsPresent: true, Tags: tags,
		},
	} {
		if _, err := normalizeActorUpdate(request); err != nil {
			t.Fatalf("clear request failed: %v", err)
		}
	}
	_, err := normalizeActorUpdate(actorUpdateRequest{
		EnvironmentID: environmentID, ActorDeclaredID: "operator.v1",
		Address: actorReadAddress{key: "thread:1"},
	})
	if !errors.Is(err, errActorUpdateInvalid) {
		t.Fatalf("empty update error = %v", err)
	}
}

func TestValidateUpdateActorRequestRequiresAddressAndMutation(t *testing.T) {
	metadata := json.RawMessage(`{}`)
	tags := []string{}
	for _, request := range []api.UpdateActorRequest{
		{ActorKey: "thread:1", Metadata: &metadata},
		{ActorID: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa", Tags: &tags},
	} {
		if err := api.ValidateUpdateActorRequest(request); err != nil {
			t.Fatalf("valid request failed: %v", err)
		}
	}
	for _, request := range []api.UpdateActorRequest{
		{ActorKey: "thread:1"},
		{ActorID: "act_aaaaaaaaaaaaaaaaaaaaaaaaaa", ActorKey: "thread:1", Tags: &tags},
		{ActorKey: "thread:1", Metadata: rawMessagePointer(json.RawMessage(`[]`))},
		{ActorKey: "thread:1", Tags: slicePointer[string](nil)},
	} {
		if err := api.ValidateUpdateActorRequest(request); err == nil {
			t.Fatalf("invalid request succeeded: %+v", request)
		}
	}
}

func rawMessagePointer(value json.RawMessage) *json.RawMessage {
	return &value
}

func slicePointer[T any](value []T) *[]T {
	return &value
}
