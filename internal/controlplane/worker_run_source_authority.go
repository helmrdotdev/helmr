package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleWorkerRunSource = errors.New("worker run source authority is stale")

type workerRunSourceAuthority struct {
	OrgID         pgtype.UUID
	ProjectID     pgtype.UUID
	EnvironmentID pgtype.UUID
	DeploymentID  pgtype.UUID
	WorkspaceID   pgtype.UUID
	RunID         pgtype.UUID
	AttemptNumber int32
}

func authorizeWorkerRunSource(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	lease workerapi.RunLeaseFence,
) (workerRunSourceAuthority, error) {
	parsed, err := parseRunLeaseFence(lease)
	if err != nil {
		return workerRunSourceAuthority{}, fmt.Errorf("%w: invalid receipt", errStaleWorkerRunSource)
	}
	locators, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
		ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: lease.LeaseSequence,
		WorkerGroupID:    worker.WorkerGroupID,
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:      worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
	})
	if err != nil {
		return workerRunSourceAuthority{}, staleWorkerRunSource(err)
	}
	authority, err := lockLiveRunLeaseAuthority(
		ctx, q, worker, pgvalue.UUID(parsed.leaseID), lease.LeaseSequence, locators,
	)
	if err != nil ||
		authority.run.Status != db.RunStatusRunning ||
		authority.runLease.State != db.RunLeaseStateRunning ||
		!authority.run.ActiveStartedAt.Valid ||
		!authority.attempt.EntrypointEnteredAt.Valid ||
		authority.attempt.TerminalAt.Valid ||
		authority.runLease.FinalizationOperationID.Valid {
		return workerRunSourceAuthority{}, fmt.Errorf("%w: live authority mismatch", errStaleWorkerRunSource)
	}
	return workerRunSourceAuthority{
		OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID,
		DeploymentID:  authority.run.DeploymentID,
		WorkspaceID:   locators.WorkspaceID, RunID: locators.RunID,
		AttemptNumber: locators.AttemptNumber,
	}, nil
}

func staleWorkerRunSource(err error) error {
	if err == nil || errors.Is(err, pgx.ErrNoRows) ||
		errors.Is(err, errStaleRunLeaseClaim) {
		return errStaleWorkerRunSource
	}
	return err
}
