package capacity

import (
	"context"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
)

func TestHasQueuedDemandUsesWorkerGroupRegion(t *testing.T) {
	for _, test := range []struct {
		name      string
		runs      []db.ListQueuedRunEligibleScopesRow
		execs     []db.ListPendingWorkspaceExecCapacityCandidatesRow
		want      bool
		wantExecs int
	}{
		{name: "queued run", runs: []db.ListQueuedRunEligibleScopesRow{{}}, want: true},
		{name: "pending Workspace Exec", execs: []db.ListPendingWorkspaceExecCapacityCandidatesRow{{}}, want: true, wantExecs: 1},
		{name: "idle", wantExecs: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeQueuedDemandStore{
				group: db.WorkerGroup{ID: "run-workers", RegionID: "us-east-1"},
				runs:  test.runs, execs: test.execs,
			}
			got, err := HasQueuedDemand(t.Context(), store, "run-workers")
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("HasQueuedDemand() = %t, want %t", got, test.want)
			}
			if store.groupID != "run-workers" || store.runParams.RegionFilter != "us-east-1" || store.runParams.RowLimit != 1 {
				t.Fatalf("group = %q, run params = %+v", store.groupID, store.runParams)
			}
			if store.execCalls != test.wantExecs {
				t.Fatalf("Workspace Exec calls = %d, want %d", store.execCalls, test.wantExecs)
			}
			if test.wantExecs == 1 && (store.execParams.RegionID != "us-east-1" || store.execParams.RowLimit != 1) {
				t.Fatalf("Workspace Exec params = %+v", store.execParams)
			}
		})
	}
}

type fakeQueuedDemandStore struct {
	group      db.WorkerGroup
	runs       []db.ListQueuedRunEligibleScopesRow
	execs      []db.ListPendingWorkspaceExecCapacityCandidatesRow
	groupID    string
	runParams  db.ListQueuedRunEligibleScopesParams
	execParams db.ListPendingWorkspaceExecCapacityCandidatesParams
	execCalls  int
}

func (s *fakeQueuedDemandStore) GetWorkerGroup(_ context.Context, id string) (db.WorkerGroup, error) {
	s.groupID = id
	return s.group, nil
}

func (s *fakeQueuedDemandStore) ListQueuedRunEligibleScopes(_ context.Context, params db.ListQueuedRunEligibleScopesParams) ([]db.ListQueuedRunEligibleScopesRow, error) {
	s.runParams = params
	return s.runs, nil
}

func (s *fakeQueuedDemandStore) ListPendingWorkspaceExecCapacityCandidates(_ context.Context, params db.ListPendingWorkspaceExecCapacityCandidatesParams) ([]db.ListPendingWorkspaceExecCapacityCandidatesRow, error) {
	s.execCalls++
	s.execParams = params
	return s.execs, nil
}
