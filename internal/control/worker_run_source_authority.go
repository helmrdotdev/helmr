package control

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleWorkerRunSource = errors.New("worker Run source authority is stale")

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
	lease api.WorkerRunLeaseReceipt,
) (workerRunSourceAuthority, error) {
	parsed, err := parseRunLeaseReceipt(lease)
	if err != nil {
		return workerRunSourceAuthority{}, fmt.Errorf("%w: invalid receipt", errStaleWorkerRunSource)
	}
	if lease.WorkerGroupID != worker.WorkerGroupID ||
		parsed.workerInstanceID != worker.WorkerInstanceID ||
		lease.WorkerEpoch != worker.WorkerEpoch ||
		lease.WorkerProtocolVersion != worker.ProtocolVersion {
		return workerRunSourceAuthority{}, fmt.Errorf("%w: worker identity mismatch", errStaleWorkerRunSource)
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
		authority.run.ID != pgvalue.UUID(parsed.runID) ||
		authority.run.Status != db.RunStatusRunning ||
		authority.runLease.State != db.RunLeaseStateRunning ||
		!authority.run.ActiveStartedAt.Valid ||
		!authority.attempt.EntrypointEnteredAt.Valid ||
		authority.attempt.TerminalAt.Valid ||
		authority.runLease.FinalizationOperationID.Valid {
		return workerRunSourceAuthority{}, fmt.Errorf("%w: live authority mismatch", errStaleWorkerRunSource)
	}
	current, err := projectActorTurnLease(authority)
	if err != nil || !equalRunLeaseReceipt(current, lease) {
		return workerRunSourceAuthority{}, fmt.Errorf("%w: receipt mismatch", errStaleWorkerRunSource)
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
