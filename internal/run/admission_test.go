package run

import (
	"context"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCreateTaskBuildsCompleteAdmissionTuple(t *testing.T) {
	runID := pgvalue.UUID(uuid.NewV7())
	workspaceID := pgvalue.UUID(uuid.NewV7())
	environmentID := pgvalue.UUID(uuid.NewV7())
	versionID := pgvalue.UUID(uuid.NewV7())
	secretID := pgvalue.UUID(uuid.NewV7())
	secretVersionID := pgvalue.UUID(uuid.NewV7())
	store := &taskStore{
		bindings: []db.LockWorkspaceSecretsForAdmissionRow{{
			WorkspaceID:          workspaceID,
			PlacementKind:        "env",
			PlacementTarget:      "API_TOKEN",
			SecretID:             secretID,
			SecretState:          "active",
			CurrentVersionID:     secretVersionID,
			RevocationGeneration: 3,
		}},
		run: db.CreateAdmittedRootTaskRunRow{ID: runID},
	}
	created, err := CreateTask(context.Background(), store, TaskRequest{
		Run: db.CreateAdmittedRootTaskRunParams{
			EnvironmentID:          environmentID,
			WorkspaceID:            workspaceID,
			BaseWorkspaceVersionID: versionID,
		},
		WorkspaceStateVersion: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != runID {
		t.Fatalf("run ID = %+v", created.ID)
	}
	if len(store.resolutions) != 1 || len(store.resolutions[0].SecretIds) != 1 {
		t.Fatalf("Secret resolution batches = %+v", store.resolutions)
	}
	resolution := store.resolutions[0]
	if resolution.RunID != runID || resolution.SecretVersionIds[0] != secretVersionID || resolution.RevocationGenerations[0] != 3 {
		t.Fatalf("Secret resolution = %+v", resolution)
	}
	if store.reserve.ExpectedStateVersion != 7 || store.reserve.ExpectedHeadVersionID != versionID {
		t.Fatalf("Workspace reservation = %+v", store.reserve)
	}
}

type taskStore struct {
	bindings    []db.LockWorkspaceSecretsForAdmissionRow
	run         db.CreateAdmittedRootTaskRunRow
	reserve     db.ReserveWorkspaceForRunParams
	resolutions []db.CreateAttemptSecretResolutionsParams
}

func (s *taskStore) LockWorkspaceSecretsForAdmission(context.Context, pgtype.UUID) ([]db.LockWorkspaceSecretsForAdmissionRow, error) {
	return s.bindings, nil
}

func (s *taskStore) CreateAdmittedRootTaskRun(context.Context, db.CreateAdmittedRootTaskRunParams) (db.CreateAdmittedRootTaskRunRow, error) {
	return s.run, nil
}

func (s *taskStore) ReserveWorkspaceForRun(_ context.Context, value db.ReserveWorkspaceForRunParams) (db.Workspace, error) {
	s.reserve = value
	return db.Workspace{}, nil
}

func (s *taskStore) CreateAttemptSecretResolutions(_ context.Context, value db.CreateAttemptSecretResolutionsParams) (int64, error) {
	s.resolutions = append(s.resolutions, value)
	return int64(len(value.Ids)), nil
}
