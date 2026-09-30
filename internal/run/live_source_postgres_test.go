package run

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestLiveSourceIsTheLockedExecutionsComputer(t *testing.T) {
	f, work, fence := executionClaimFixture(t)
	if _, err := claimExecutionTest(t, f, fence, true); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	started, err := StartExecution(t.Context(), tx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err = EnterExecution(t.Context(), tx, fence, started.Run().EntrypointKind, started.Run().EntrypointDeclaredID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var computerID pgtype.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM runs WHERE id=$1`, work.RunID).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	execution, err := LockLiveExecution(t.Context(), tx, fence)
	if err != nil {
		t.Fatal(err)
	}
	source, err := execution.LiveSource()
	if err != nil {
		t.Fatal(err)
	}
	if source.ComputerID() != computerID || source.RunID() != pgvalue.UUID(work.RunID) {
		t.Fatalf("source Computer=%s Run=%s, want Computer=%s Run=%s", pgvalue.UUIDString(source.ComputerID()), pgvalue.UUIDString(source.RunID()), pgvalue.UUIDString(computerID), work.RunID)
	}
}
