package runadmission

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrWorkspaceReservationConflict = errors.New("Workspace reservation changed")
	ErrSecretUnavailable            = errors.New("Workspace Secret is unavailable")
)

type TaskRequest struct {
	Run                   db.CreateAdmittedRootTaskRunParams
	WorkspaceStateVersion int64
}

type Store interface {
	LockWorkspaceSecretsForAdmission(context.Context, pgtype.UUID) ([]db.LockWorkspaceSecretsForAdmissionRow, error)
	CreateAdmittedRootTaskRun(context.Context, db.CreateAdmittedRootTaskRunParams) (db.CreateAdmittedRootTaskRunRow, error)
	ReserveWorkspaceForRun(context.Context, db.ReserveWorkspaceForRunParams) (db.Workspace, error)
	CreateSecretResolution(context.Context, db.CreateSecretResolutionParams) (db.SecretResolution, error)
	CreateRunAdmissionOutbox(context.Context, db.CreateRunAdmissionOutboxParams) (db.OutboxMessage, error)
}

func CreateTask(ctx context.Context, store Store, request TaskRequest) (db.CreateAdmittedRootTaskRunRow, error) {
	if store == nil {
		return db.CreateAdmittedRootTaskRunRow{}, errors.New("Run admission store is required")
	}
	bindings, err := store.LockWorkspaceSecretsForAdmission(ctx, request.Run.WorkspaceID)
	if err != nil {
		return db.CreateAdmittedRootTaskRunRow{}, fmt.Errorf("lock Workspace Secrets: %w", err)
	}
	for _, binding := range bindings {
		if binding.SecretState != "active" || !binding.CurrentVersionID.Valid {
			return db.CreateAdmittedRootTaskRunRow{}, ErrSecretUnavailable
		}
	}

	run, err := store.CreateAdmittedRootTaskRun(ctx, request.Run)
	if err != nil {
		return db.CreateAdmittedRootTaskRunRow{}, fmt.Errorf("create admitted Task Run: %w", err)
	}
	if _, err := store.ReserveWorkspaceForRun(ctx, db.ReserveWorkspaceForRunParams{
		RunID:                 run.ID,
		EnvironmentID:         request.Run.EnvironmentID,
		ID:                    request.Run.WorkspaceID,
		ExpectedStateVersion:  request.WorkspaceStateVersion,
		ExpectedHeadVersionID: request.Run.BaseWorkspaceVersionID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.CreateAdmittedRootTaskRunRow{}, ErrWorkspaceReservationConflict
		}
		return db.CreateAdmittedRootTaskRunRow{}, fmt.Errorf("reserve Workspace for Run: %w", err)
	}

	for _, binding := range bindings {
		if _, err := store.CreateSecretResolution(ctx, db.CreateSecretResolutionParams{
			ID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
			WorkspaceID:          request.Run.WorkspaceID,
			RunID:                run.ID,
			AttemptNumber:        pgtype.Int4{Int32: 1, Valid: true},
			PlacementKind:        binding.PlacementKind,
			PlacementTarget:      binding.PlacementTarget,
			SecretID:             binding.SecretID,
			SecretVersionID:      binding.CurrentVersionID,
			RevocationGeneration: binding.RevocationGeneration,
		}); err != nil {
			return db.CreateAdmittedRootTaskRunRow{}, fmt.Errorf("record Run Secret resolution: %w", err)
		}
	}
	if _, err := store.CreateRunAdmissionOutbox(ctx, db.CreateRunAdmissionOutboxParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		WorkspaceID:   request.Run.WorkspaceID,
		EnvironmentID: request.Run.EnvironmentID,
		RunID:         run.ID,
	}); err != nil {
		return db.CreateAdmittedRootTaskRunRow{}, fmt.Errorf("create Run admission outbox: %w", err)
	}
	return run, nil
}
