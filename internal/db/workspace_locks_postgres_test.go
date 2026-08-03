package db

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestChildWorkspacePairLocksConvergeForOppositeDirections(t *testing.T) {
	fixture := newRunLeaseClaimFixture(t, t.Context())
	firstRun := fixture.addWork(t, t.Context(), "assigned", time.Now())
	secondRun := fixture.addWork(t, t.Context(), "assigned", time.Now())
	var firstWorkspace, secondWorkspace uuid.UUID
	if err := fixture.pool.QueryRow(
		t.Context(),
		"SELECT workspace_id FROM runs WHERE id = $1",
		firstRun.runID,
	).Scan(&firstWorkspace); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(
		t.Context(),
		"SELECT workspace_id FROM runs WHERE id = $1",
		secondRun.runID,
	).Scan(&secondWorkspace); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	lock := func(workspaceIDs []uuid.UUID) {
		tx, err := fixture.pool.Begin(ctx)
		if err != nil {
			results <- err
			return
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		<-start
		rows, err := New(tx).LockChildWorkspacePair(ctx, LockChildWorkspacePairParams{
			EnvironmentID: pgvalue.UUID(fixture.environmentID),
			WorkspaceIds: []pgtype.UUID{
				pgvalue.UUID(workspaceIDs[0]),
				pgvalue.UUID(workspaceIDs[1]),
			},
		})
		if err == nil && len(rows) != 2 {
			err = fmt.Errorf("locked %d workspaces, want 2", len(rows))
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		results <- err
	}
	go lock([]uuid.UUID{firstWorkspace, secondWorkspace})
	go lock([]uuid.UUID{secondWorkspace, firstWorkspace})
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}
