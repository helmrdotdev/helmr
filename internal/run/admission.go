package run

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrComputerAdmissionConflict = errors.New("computer admission changed")
	ErrSecretUnavailable         = errors.New("computer secret is unavailable")
)

type TaskRequest struct {
	Run              db.CreateAdmittedRootTaskRunParams
	ComputerRevision int64
}

type Store interface {
	LockComputerSecretsForAdmission(context.Context, pgtype.UUID) ([]db.LockComputerSecretsForAdmissionRow, error)
	CreateAdmittedRootTaskRun(context.Context, db.CreateAdmittedRootTaskRunParams) (db.CreateAdmittedRootTaskRunRow, error)
	TouchComputerForAdmission(context.Context, db.TouchComputerForAdmissionParams) (db.Computer, error)
	secret.AttemptResolutionStore
}

func CreateTask(ctx context.Context, store Store, request TaskRequest) (db.CreateAdmittedRootTaskRunRow, error) {
	if store == nil {
		return db.CreateAdmittedRootTaskRunRow{}, errors.New("run admission store is required")
	}
	bindings, err := store.LockComputerSecretsForAdmission(ctx, request.Run.ComputerID)
	if err != nil {
		return db.CreateAdmittedRootTaskRunRow{}, fmt.Errorf("lock computer secrets: %w", err)
	}
	if !admissionSecretsAvailable(bindings) {
		return db.CreateAdmittedRootTaskRunRow{}, ErrSecretUnavailable
	}

	run, err := store.CreateAdmittedRootTaskRun(ctx, request.Run)
	if err != nil {
		return db.CreateAdmittedRootTaskRunRow{}, fmt.Errorf("create admitted task run: %w", err)
	}
	if _, err := store.TouchComputerForAdmission(ctx, db.TouchComputerForAdmissionParams{
		EnvironmentID:    request.Run.EnvironmentID,
		ID:               request.Run.ComputerID,
		ExpectedRevision: request.ComputerRevision,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.CreateAdmittedRootTaskRunRow{}, ErrComputerAdmissionConflict
		}
		return db.CreateAdmittedRootTaskRunRow{}, fmt.Errorf("record Computer admission: %w", err)
	}

	if err := secret.CreateAttemptResolutions(ctx, store, request.Run.ComputerID, run.ID, 1, SecretResolutions(bindings)); err != nil {
		return db.CreateAdmittedRootTaskRunRow{}, fmt.Errorf("record run secret resolutions: %w", err)
	}
	return run, nil
}
