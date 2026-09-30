package command

import (
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

// computerFixture returns a Run test database with an active Computer that
// admits Commands.
func computerFixture(t *testing.T) (runtest.Fixture, uuid.UUID) {
	t.Helper()
	f := runtest.New(t)
	lease := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	var computerID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM run_leases WHERE id=$1`, lease.LeaseID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	return f, computerID
}

func createRequest(f runtest.Fixture, computerID uuid.UUID, key string) CreateRequest {
	return CreateRequest{
		OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, ComputerID: computerID,
		Creator: Creator{SubjectType: string(auth.PrincipalKindAPIKey), SubjectID: uuid.NewV7().String()},
		Argv:    []string{"true"}, IdempotencyKey: key,
	}
}

// Create admits one pending Command per idempotency key, stores the
// {"command_id"} receipt and replays the Command for the same request.
func TestCreateAdmitsOnceAndReplaysReceipt(t *testing.T) {
	f, computerID := computerFixture(t)
	request := createRequest(f, computerID, "create-once")
	created, err := Create(t.Context(), f.Pool, request)
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != db.ComputerCommandStatusPending || created.ComputerInstanceID.Valid || created.ComputerID != pgvalue.UUID(computerID) {
		t.Fatalf("created = %+v", created)
	}
	var receipt string
	var keys int
	var revision int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT c.receipt->>'command_id',(SELECT count(*) FROM jsonb_object_keys(c.receipt)),computers.revision FROM idempotency_claims c
 JOIN computer_commands command ON command.claim_id=c.id JOIN computers ON computers.id=command.computer_id WHERE command.id=$1`, created.ID).Scan(&receipt, &keys, &revision); err != nil {
		t.Fatal(err)
	}
	if receipt != pgvalue.UUIDString(created.ID) || keys != 1 {
		t.Fatalf("receipt command_id = %s with %d keys", receipt, keys)
	}
	replayed, err := Create(t.Context(), f.Pool, request)
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("replay = %+v, %v", replayed, err)
	}
	var after int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT revision FROM computers WHERE id=$1`, computerID).Scan(&after); err != nil || after != revision {
		t.Fatalf("replay touched the Computer: revision %d -> %d, %v", revision, after, err)
	}
	changed := request
	changed.Argv = []string{"false"}
	var conflict idempotency.ConflictError
	if _, err := Create(t.Context(), f.Pool, changed); !errors.As(err, &conflict) {
		t.Fatalf("changed request = %v, want idempotency conflict", err)
	}
	other := request
	other.ProjectID = uuid.NewV7()
	if _, err := Create(t.Context(), f.Pool, other); !errors.Is(err, computer.ErrNotFound) {
		t.Fatalf("other project = %v", err)
	}
}
