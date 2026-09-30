package computer

import (
	"encoding/json"
	"errors"
	"testing"
)

// goldenCreateReceipt is a creation receipt as the control plane wrote it
// before this owner existed, from the public Computer resource encoding.
const goldenCreateReceipt = `{"computer":{"residency":"cold","id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31","key":"repository","sandbox_id":"computer.v1","deployment_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32","status":"available","secrets":[{"secret":"API_TOKEN","env":{"name":"TOKEN","mode":"protected","allowed_origins":["https://example.com"]}},{"secret":"API_TOKEN","file":{"path":"/run/secrets/key"}}],"last_activity_at":"2026-09-30T12:00:00.123456Z","created_at":"2026-09-30T12:00:00.123456Z","updated_at":"2026-09-30T12:00:00.123456Z"}}`

// goldenStoredCreateReceipt is the same receipt as PostgreSQL jsonb returns
// it, with its key order and spacing.
const goldenStoredCreateReceipt = `{"computer": {"id": "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31", "key": "repository", "status": "available", "secrets": [{"env": {"mode": "protected", "name": "TOKEN", "allowed_origins": ["https://example.com"]}, "secret": "API_TOKEN"}, {"file": {"path": "/run/secrets/key"}, "secret": "API_TOKEN"}], "residency": "cold", "sandbox_id": "computer.v1", "created_at": "2026-09-30T12:00:00.123456Z", "updated_at": "2026-09-30T12:00:00.123456Z", "deployment_id": "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32", "last_activity_at": "2026-09-30T12:00:00.123456Z"}}`

func TestCreateReceiptDecodesAndEncodesStoredReceipts(t *testing.T) {
	for name, raw := range map[string]string{"encoded": goldenCreateReceipt, "stored": goldenStoredCreateReceipt} {
		t.Run(name, func(t *testing.T) {
			created, err := createdFromReceipt([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if created.ComputerID.String() != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" || created.Snapshot.Key == nil || *created.Snapshot.Key != "repository" || len(created.Snapshot.Secrets) != 2 {
				t.Fatalf("decoded = %+v", created)
			}
			encoded, err := json.Marshal(createReceipt{Computer: created.Snapshot})
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != goldenCreateReceipt {
				t.Fatalf("re-encoded receipt\n%s\nwant\n%s", encoded, goldenCreateReceipt)
			}
		})
	}
}

func TestCreateReceiptRejectsForeignReceipts(t *testing.T) {
	for name, raw := range map[string]string{
		"malformed":     `{"computer":`,
		"missing id":    `{"computer":{"sandbox_id":"computer.v1","status":"available"}}`,
		"deleted":       `{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31","sandbox_id":"computer.v1","deployment_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32","status":"deleted","last_activity_at":"2026-09-30T12:00:00Z","created_at":"2026-09-30T12:00:00Z","updated_at":"2026-09-30T12:00:00Z"}}`,
		"invalid key":   `{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31","key":" padded ","sandbox_id":"computer.v1","deployment_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32","status":"available","last_activity_at":"2026-09-30T12:00:00Z","created_at":"2026-09-30T12:00:00Z","updated_at":"2026-09-30T12:00:00Z"}}`,
		"missing times": `{"computer":{"id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31","sandbox_id":"computer.v1","deployment_id":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32","status":"available"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := createdFromReceipt([]byte(raw)); !errors.Is(err, ErrReceiptInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestDeleteReceiptKeepsItsStoredShape(t *testing.T) {
	encoded, err := json.Marshal(deleteReceipt{ComputerID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"})
	if err != nil || string(encoded) != `{"computerId":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"}` {
		t.Fatalf("delete receipt = %s, %v", encoded, err)
	}
	deleted, err := DeletionClaim{replayed: true, receipt: encoded}.Receipt()
	if err != nil || !deleted.Replayed || deleted.ComputerID.String() != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31" {
		t.Fatalf("decoded = %+v, %v", deleted, err)
	}
}
