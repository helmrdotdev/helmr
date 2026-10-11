package controlplane

import (
	"testing"
	"time"
	"uuid"
)

func TestPublicLifecycleProjectionsRejectUnknownInternalValues(t *testing.T) {
	tests := []struct {
		name    string
		project func() error
	}{
		{name: "worker", project: func() error { _, err := workerPublicStatus("future"); return err }},
		{name: "secret", project: func() error { _, err := secretPublicStatus("future"); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.project(); err == nil {
				t.Fatal("unknown internal lifecycle value was projected")
			}
		})
	}
}

func TestResourceCollectionCursorsRoundTripOwnerScope(t *testing.T) {
	createdAt := time.Date(2026, 8, 5, 12, 0, 0, 123, time.UTC).Format(time.RFC3339Nano)
	id := uuid.NewV7().String()
	apiKeyCursor := apiKeyListCursor{
		ProjectID: uuid.NewV7().String(), EnvironmentID: uuid.NewV7().String(),
		Filter: "active", CreatedAt: createdAt, ID: id,
	}
	raw, err := encodeAPIKeyListCursor(apiKeyCursor)
	if err != nil {
		t.Fatal(err)
	}
	decodedAPIKeyCursor, err := decodeAPIKeyListCursor(raw)
	if err != nil || decodedAPIKeyCursor != apiKeyCursor {
		t.Fatalf("API key cursor = %+v, err = %v", decodedAPIKeyCursor, err)
	}

	invitationCursor := invitationListCursor{
		OrgID: uuid.NewV7().String(), CreatedAt: createdAt, ID: id,
	}
	raw, err = encodeInvitationListCursor(invitationCursor)
	if err != nil {
		t.Fatal(err)
	}
	decodedInvitationCursor, err := decodeInvitationListCursor(raw)
	if err != nil || decodedInvitationCursor != invitationCursor {
		t.Fatalf("invitation cursor = %+v, err = %v", decodedInvitationCursor, err)
	}
}
