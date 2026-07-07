package control

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestRequeueResolvedRunWaitsEnsuresWorkspaceMount(t *testing.T) {
	orgID := uuid.Must(uuid.NewV7())
	projectID := uuid.Must(uuid.NewV7())
	environmentID := uuid.Must(uuid.NewV7())
	workspaceID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	runWaitID := uuid.Must(uuid.NewV7())
	workspaceMountID := uuid.Must(uuid.NewV7())
	store := &runWaitResumeFakeStore{
		rows: []db.RequeueResolvedRunWaitsRow{{
			ID:            pgvalue.UUID(runWaitID),
			OrgID:         pgvalue.UUID(orgID),
			ProjectID:     pgvalue.UUID(projectID),
			EnvironmentID: pgvalue.UUID(environmentID),
			RunID:         pgvalue.UUID(runID),
			WorkspaceID:   pgvalue.UUID(workspaceID),
			Priority:      17,
		}},
		workspaceMountID: pgvalue.UUID(workspaceMountID),
	}
	rows, err := requeueResolvedRunWaitsWithStore(context.Background(), store, pgvalue.UUID(orgID), "worker-group-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if store.requeueParams.OrgID != pgvalue.UUID(orgID) || store.requeueParams.WorkerGroupID != "worker-group-test" || store.requeueParams.LimitCount != runWaitRequeueLimit {
		t.Fatalf("requeue params = %+v", store.requeueParams)
	}
	if store.ensureParams.WorkspaceID != pgvalue.UUID(workspaceID) {
		t.Fatalf("ensure workspace = %s, want %s", pgvalue.UUIDString(store.ensureParams.WorkspaceID), workspaceID)
	}
	if store.ensureParams.RequestPriority != 17 {
		t.Fatalf("ensure priority = %d, want 17", store.ensureParams.RequestPriority)
	}
	var request map[string]string
	if err := json.Unmarshal(store.ensureParams.Request, &request); err != nil {
		t.Fatal(err)
	}
	if request["source"] != "run_resume_wait" || request["run_id"] != runID.String() || request["run_wait_id"] != runWaitID.String() {
		t.Fatalf("ensure request = %+v", request)
	}
	if store.linkParams.RunID != pgvalue.UUID(runID) || store.linkParams.WorkspaceMountID != pgvalue.UUID(workspaceMountID) {
		t.Fatalf("link params = %+v", store.linkParams)
	}
}

func TestRequeueResolvedRunWaitsReturnsEnsureFailure(t *testing.T) {
	orgID := uuid.Must(uuid.NewV7())
	store := &runWaitResumeFakeStore{
		rows: []db.RequeueResolvedRunWaitsRow{{
			ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:         pgvalue.UUID(orgID),
			ProjectID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
			EnvironmentID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
			RunID:         pgvalue.UUID(uuid.Must(uuid.NewV7())),
			WorkspaceID:   pgvalue.UUID(uuid.Must(uuid.NewV7())),
		}},
		ensureErr: errors.New("mount prerequisites missing"),
	}

	_, err := requeueResolvedRunWaitsWithStore(context.Background(), store, pgvalue.UUID(orgID), "worker-group-test", nil)
	if err == nil || !errors.Is(err, store.ensureErr) {
		t.Fatalf("err = %v, want ensure error", err)
	}
	if store.linkParams.RunID.Valid {
		t.Fatalf("link should not run after ensure failure: %+v", store.linkParams)
	}
}

func TestEnsureQueuedRunWorkspaceMountRelinksQueuedRun(t *testing.T) {
	orgID := uuid.Must(uuid.NewV7())
	projectID := uuid.Must(uuid.NewV7())
	environmentID := uuid.Must(uuid.NewV7())
	workspaceID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	workspaceMountID := uuid.Must(uuid.NewV7())
	store := &runWaitResumeFakeStore{
		run: db.Run{
			ID:            pgvalue.UUID(runID),
			OrgID:         pgvalue.UUID(orgID),
			ProjectID:     pgvalue.UUID(projectID),
			EnvironmentID: pgvalue.UUID(environmentID),
			WorkspaceID:   pgvalue.UUID(workspaceID),
			Status:        db.RunStatusQueued,
			Priority:      23,
		},
		workspaceMountID: pgvalue.UUID(workspaceMountID),
	}

	ensured, err := ensureQueuedRunWorkspaceMount(context.Background(), store, pgvalue.UUID(orgID), pgvalue.UUID(runID), "run_lease_conflict", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ensured {
		t.Fatal("ensured = false, want true")
	}
	if store.getRunOrgID != pgvalue.UUID(orgID) || store.getRunID != pgvalue.UUID(runID) {
		t.Fatalf("get run org/run = %s/%s", pgvalue.UUIDString(store.getRunOrgID), pgvalue.UUIDString(store.getRunID))
	}
	if store.ensureParams.WorkspaceID != pgvalue.UUID(workspaceID) {
		t.Fatalf("ensure params = %+v", store.ensureParams)
	}
	if store.ensureParams.RequestPriority != 23 {
		t.Fatalf("ensure priority = %d, want 23", store.ensureParams.RequestPriority)
	}
	var request map[string]string
	if err := json.Unmarshal(store.ensureParams.Request, &request); err != nil {
		t.Fatal(err)
	}
	if request["source"] != "run_lease_conflict" || request["run_id"] != runID.String() {
		t.Fatalf("ensure request = %+v", request)
	}
	if store.linkParams.RunID != pgvalue.UUID(runID) || store.linkParams.WorkspaceMountID != pgvalue.UUID(workspaceMountID) {
		t.Fatalf("link params = %+v", store.linkParams)
	}
}

func TestEnsureQueuedRunWorkspaceMountSkipsNonQueuedRun(t *testing.T) {
	orgID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	store := &runWaitResumeFakeStore{
		run: db.Run{
			ID:     pgvalue.UUID(runID),
			OrgID:  pgvalue.UUID(orgID),
			Status: db.RunStatusRunning,
		},
	}

	ensured, err := ensureQueuedRunWorkspaceMount(context.Background(), store, pgvalue.UUID(orgID), pgvalue.UUID(runID), "run_lease_conflict", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ensured {
		t.Fatal("ensured = true, want false")
	}
	if store.ensureParams.WorkspaceID.Valid || store.linkParams.RunID.Valid {
		t.Fatalf("unexpected mount calls: ensure=%+v link=%+v", store.ensureParams, store.linkParams)
	}
}

type runWaitResumeFakeStore struct {
	rows             []db.RequeueResolvedRunWaitsRow
	run              db.Run
	getRunOrgID      pgtype.UUID
	getRunID         pgtype.UUID
	workspaceMountID pgtype.UUID
	ensureErr        error
	linkErr          error
	requeueParams    db.RequeueResolvedRunWaitsParams
	ensureParams     db.EnsureWorkspaceMountRequestedParams
	linkParams       db.SetQueuedRunWorkspaceMountParams
}

func (s *runWaitResumeFakeStore) RequeueResolvedRunWaits(_ context.Context, arg db.RequeueResolvedRunWaitsParams) ([]db.RequeueResolvedRunWaitsRow, error) {
	s.requeueParams = arg
	return s.rows, nil
}

func (s *runWaitResumeFakeStore) GetRun(_ context.Context, arg db.GetRunParams) (db.Run, error) {
	s.getRunOrgID = arg.OrgID
	s.getRunID = arg.ID
	return s.run, nil
}

func (s *runWaitResumeFakeStore) EnsureWorkspaceMountRequested(_ context.Context, arg db.EnsureWorkspaceMountRequestedParams) (db.EnsureWorkspaceMountRequestedRow, error) {
	s.ensureParams = arg
	if s.ensureErr != nil {
		return db.EnsureWorkspaceMountRequestedRow{}, s.ensureErr
	}
	return db.EnsureWorkspaceMountRequestedRow{ID: s.workspaceMountID}, nil
}

func (s *runWaitResumeFakeStore) SetQueuedRunWorkspaceMount(_ context.Context, arg db.SetQueuedRunWorkspaceMountParams) error {
	s.linkParams = arg
	return s.linkErr
}
