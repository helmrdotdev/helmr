package capacity

import (
	"context"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

var queuedDemandTestGroupID = uuid.MustParse("01900000-0000-7000-8000-000000000201")

func TestHasQueuedDemandUsesWorkerGroupRegion(t *testing.T) {
	for _, test := range []struct {
		name      string
		runs      []db.ListQueuedRunEligibleScopesRow
		commands  []db.ListPendingComputerCommandCapacityCandidatesRow
		want      bool
		wantExecs int
	}{
		{name: "queued run", runs: []db.ListQueuedRunEligibleScopesRow{{}}, want: true},
		{name: "pending Computer Exec", commands: []db.ListPendingComputerCommandCapacityCandidatesRow{{}}, want: true, wantExecs: 1},
		{name: "idle", wantExecs: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeQueuedDemandStore{
				group: db.WorkerGroup{ID: pgvalue.UUID(queuedDemandTestGroupID), RegionID: "us-east-1"},
				runs:  test.runs, commands: test.commands,
			}
			got, err := HasQueuedDemand(t.Context(), store, queuedDemandTestGroupID)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("HasQueuedDemand() = %t, want %t", got, test.want)
			}
			if store.groupID != pgvalue.UUID(queuedDemandTestGroupID) || store.runParams.RegionFilter != "us-east-1" || store.runParams.RowLimit != 1 {
				t.Fatalf("group = %v, run params = %+v", store.groupID, store.runParams)
			}
			if store.execCalls != test.wantExecs {
				t.Fatalf("Computer Exec calls = %d, want %d", store.execCalls, test.wantExecs)
			}
			if test.wantExecs == 1 && (store.execParams.RegionID != "us-east-1" || store.execParams.RowLimit != 1) {
				t.Fatalf("Computer Exec params = %+v", store.execParams)
			}
		})
	}
}

type fakeQueuedDemandStore struct {
	group      db.WorkerGroup
	runs       []db.ListQueuedRunEligibleScopesRow
	commands   []db.ListPendingComputerCommandCapacityCandidatesRow
	groupID    pgtype.UUID
	runParams  db.ListQueuedRunEligibleScopesParams
	execParams db.ListPendingComputerCommandCapacityCandidatesParams
	execCalls  int
}

func (s *fakeQueuedDemandStore) GetWorkerGroup(_ context.Context, id pgtype.UUID) (db.WorkerGroup, error) {
	s.groupID = id
	return s.group, nil
}

func (s *fakeQueuedDemandStore) ListQueuedRunEligibleScopes(_ context.Context, params db.ListQueuedRunEligibleScopesParams) ([]db.ListQueuedRunEligibleScopesRow, error) {
	s.runParams = params
	return s.runs, nil
}

func (s *fakeQueuedDemandStore) ListPendingComputerCommandCapacityCandidates(_ context.Context, params db.ListPendingComputerCommandCapacityCandidatesParams) ([]db.ListPendingComputerCommandCapacityCandidatesRow, error) {
	s.execCalls++
	s.execParams = params
	return s.commands, nil
}
