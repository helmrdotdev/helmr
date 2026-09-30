package computer

import (
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestValidateKeyPreservesExactBytes(t *testing.T) {
	edge := " repository "
	if err := ValidateKey(&edge); err == nil {
		t.Fatal("expected edge whitespace to be rejected without normalization")
	}
	exact := "répository"
	if err := ValidateKey(&exact); err != nil {
		t.Fatalf("valid exact UTF-8 key: %v", err)
	}
	if err := ValidateKey(nil); err != nil {
		t.Fatalf("absent key: %v", err)
	}
}

func TestPublicStatusUsesPublicSpelling(t *testing.T) {
	for state, want := range map[string]Status{
		db.ComputerStatusActive:           StatusAvailable,
		db.ComputerStatusRecoveryRequired: StatusAvailable,
		db.ComputerStatusDeleting:         StatusDeleting,
		db.ComputerStatusDeleted:          StatusDeleted,
	} {
		if status, err := publicStatus(state); err != nil || status != want {
			t.Fatalf("status(%s) = %q, %v; want %q", state, status, err, want)
		}
	}
	if _, err := publicStatus("future"); err == nil {
		t.Fatal("unknown internal lifecycle value was projected")
	}
}

func TestListItemProjectsKeyAndStatus(t *testing.T) {
	now := pgvalue.Timestamptz(time.Date(2026, time.August, 6, 12, 0, 0, 0, time.UTC))
	item, err := listItem(
		pgvalue.UUID(uuid.NewV7()), pgvalue.Text("repository"), "repository-agent",
		pgvalue.UUID(uuid.NewV7()), db.ComputerStatusActive, now, now, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if item.Key == nil || *item.Key != "repository" || item.Status != StatusAvailable {
		t.Fatalf("item=%+v", item)
	}
}
