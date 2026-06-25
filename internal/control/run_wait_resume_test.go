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

func TestRequeueResolvedRunWaitsEnsuresWorkspaceMaterialization(t *testing.T) {
	orgID := uuid.Must(uuid.NewV7())
	projectID := uuid.Must(uuid.NewV7())
	environmentID := uuid.Must(uuid.NewV7())
	workspaceID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	runWaitID := uuid.Must(uuid.NewV7())
	materializationID := uuid.Must(uuid.NewV7())
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
		materializationID: pgvalue.UUID(materializationID),
	}

	rows, err := requeueResolvedRunWaitsWithStore(context.Background(), store, pgvalue.UUID(orgID))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if store.requeueParams.OrgID != pgvalue.UUID(orgID) || store.requeueParams.LimitCount != runWaitRequeueLimit {
		t.Fatalf("requeue params = %+v", store.requeueParams)
	}
	if store.ensureParams.WorkspaceID != pgvalue.UUID(workspaceID) {
		t.Fatalf("ensure workspace = %s, want %s", pgvalue.UUIDString(store.ensureParams.WorkspaceID), workspaceID)
	}
	if store.ensureParams.Priority != 17 {
		t.Fatalf("ensure priority = %d, want 17", store.ensureParams.Priority)
	}
	var request map[string]string
	if err := json.Unmarshal(store.ensureParams.Request, &request); err != nil {
		t.Fatal(err)
	}
	if request["source"] != "run_wait_resume" || request["run_id"] != runID.String() || request["run_wait_id"] != runWaitID.String() {
		t.Fatalf("ensure request = %+v", request)
	}
	if store.linkParams.RunID != pgvalue.UUID(runID) || store.linkParams.WorkspaceMaterializationID != pgvalue.UUID(materializationID) {
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
		ensureErr: errors.New("materialization prerequisites missing"),
	}

	_, err := requeueResolvedRunWaitsWithStore(context.Background(), store, pgvalue.UUID(orgID))
	if err == nil || !errors.Is(err, store.ensureErr) {
		t.Fatalf("err = %v, want ensure error", err)
	}
	if store.linkParams.RunID.Valid {
		t.Fatalf("link should not run after ensure failure: %+v", store.linkParams)
	}
}

type runWaitResumeFakeStore struct {
	rows              []db.RequeueResolvedRunWaitsRow
	materializationID pgtype.UUID
	ensureErr         error
	linkErr           error
	requeueParams     db.RequeueResolvedRunWaitsParams
	ensureParams      db.EnsureWorkspaceMaterializationRequestedParams
	linkParams        db.SetQueuedRunWorkspaceMaterializationParams
}

func (s *runWaitResumeFakeStore) RequeueResolvedRunWaits(_ context.Context, arg db.RequeueResolvedRunWaitsParams) ([]db.RequeueResolvedRunWaitsRow, error) {
	s.requeueParams = arg
	return s.rows, nil
}

func (s *runWaitResumeFakeStore) EnsureWorkspaceMaterializationRequested(_ context.Context, arg db.EnsureWorkspaceMaterializationRequestedParams) (db.EnsureWorkspaceMaterializationRequestedRow, error) {
	s.ensureParams = arg
	if s.ensureErr != nil {
		return db.EnsureWorkspaceMaterializationRequestedRow{}, s.ensureErr
	}
	return db.EnsureWorkspaceMaterializationRequestedRow{ID: s.materializationID}, nil
}

func (s *runWaitResumeFakeStore) SetQueuedRunWorkspaceMaterialization(_ context.Context, arg db.SetQueuedRunWorkspaceMaterializationParams) error {
	s.linkParams = arg
	return s.linkErr
}
