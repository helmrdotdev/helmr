package computer

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
)

func TestListMembersReadsComputerThenValidatesItsCursor(t *testing.T) {
	f := newFixture(t)
	first := f.AddRunLease(t, "running", time.Now())
	var computerID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM runs WHERE id=$1`, first.RunID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	q := db.New(f.Pool)
	page, err := ListMembers(t.Context(), q, f.scope, computerID, MembersQuery{})
	if err != nil || len(page.Members) != 1 || page.NextCursor != "" {
		t.Fatalf("members = %+v, %v", page, err)
	}
	member := page.Members[0]
	if member.Kind != "task" || member.ID != first.RunID.String() || member.RunID != first.RunID.String() || member.State != "admitted" {
		t.Fatalf("member = %+v", member)
	}
	other := f.insertComputer(t, "other")
	if _, err := ListMembers(t.Context(), q, f.scope, uuid.NewV7(), MembersQuery{Cursor: "broken"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent Computer with a broken cursor = %v, want not found first", err)
	}
	var input InputError
	if _, err := ListMembers(t.Context(), q, f.scope, other, MembersQuery{Cursor: "broken"}); !errors.As(err, &input) {
		t.Fatalf("broken cursor = %v", err)
	}
	if _, err := ListMembers(t.Context(), q, f.scope, other, MembersQuery{Limit: MaxListLimit + 1}); !errors.As(err, &input) {
		t.Fatalf("oversized limit = %v", err)
	}
	if err := ValidateMembersQuery(f.EnvironmentID, other, MembersQuery{Limit: DefaultListLimit}); err != nil {
		t.Fatal(err)
	}
}
