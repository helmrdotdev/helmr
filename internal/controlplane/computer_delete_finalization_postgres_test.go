package controlplane

import (
	"fmt"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5/pgtype"
)

// Deleting finalization skips Computers that still have members, so it is
// exercised with a Task member that only the control plane admits.
func TestComputerDeleteFinalizationSkipsBlockedRowsAndIsConcurrent(t *testing.T) {
	product := sessiontest.New(t, 4)
	blockedComputerID := product.ComputerIDs[0]
	if _, err := startTaskRun(t.Context(), product, taskStartRequest{
		OrgID: product.OrgID, ProjectID: product.ProjectID, EnvironmentID: product.EnvironmentID,
		TaskDeclaredID: "resize-image", PayloadPresent: true,
		Payload: []byte(`{"source":"delete-finalizer-blocker"}`), ComputerID: blockedComputerID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := product.Pool.Exec(t.Context(), `
UPDATE computers
   SET status = 'deleting', desired_state = 'deleted', updated_at = now() - interval '1 hour'
 WHERE id = $1`, blockedComputerID); err != nil {
		t.Fatal(err)
	}
	for index, computerID := range product.ComputerIDs[1:] {
		if _, err := computer.Delete(t.Context(), product.Pool, computer.Deletion{
			Scope:      computer.Scope{OrgID: product.OrgID, ProjectID: product.ProjectID, EnvironmentID: product.EnvironmentID},
			ComputerID: computerID, IdempotencyKey: fmt.Sprintf("computer-delete-concurrent-%d", index),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := product.Pool.Exec(t.Context(), `
UPDATE computers SET updated_at = now() - ($2::int * interval '10 minutes')
 WHERE id = $1`, computerID, 3-index); err != nil {
			t.Fatal(err)
		}
	}

	type result struct {
		ids []pgtype.UUID
		err error
	}
	first, err := db.New(product.Pool).FinalizeDeletingComputers(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || pgvalue.MustUUIDValue(first[0]) != product.ComputerIDs[1] {
		t.Fatalf("oldest eligible finalization = %+v, want %s", first, product.ComputerIDs[1])
	}

	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			ids, err := db.New(product.Pool).FinalizeDeletingComputers(t.Context(), 1)
			results <- result{ids: ids, err: err}
		}()
	}
	close(start)
	seen := map[uuid.UUID]struct{}{product.ComputerIDs[1]: {}}
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		for _, rawID := range result.ids {
			id := pgvalue.MustUUIDValue(rawID)
			if _, duplicate := seen[id]; duplicate {
				t.Fatalf("computer %s finalized twice", id)
			}
			seen[id] = struct{}{}
		}
	}
	if len(seen) != 3 {
		t.Fatalf("finalized computers = %v, want three eligible rows", seen)
	}
	var blockedStatus db.ComputerStatus
	if err := product.Pool.QueryRow(t.Context(), `
SELECT status FROM computers WHERE id = $1`, blockedComputerID).Scan(&blockedStatus); err != nil {
		t.Fatal(err)
	}
	if blockedStatus != db.ComputerStatusDeleting {
		t.Fatalf("blocked computer state = %s, want deleting", blockedStatus)
	}
}
