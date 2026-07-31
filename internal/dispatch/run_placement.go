package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type ReadyRunCandidate struct {
	OrgID                   pgtype.UUID
	RunID                   pgtype.UUID
	ExpectedRunStateVersion int64
}

type ReadyRunPlacement struct {
	Lease             db.RunLease
	LeaseCreated      bool
	WorkspaceMountID  pgtype.UUID
	WorkerInstanceID  pgtype.UUID
	WorkerEpoch       int64
	RuntimeInstanceID pgtype.UUID
}

type runWorkspaceMount struct {
	id                pgtype.UUID
	workerID          pgtype.UUID
	epoch             int64
	runtimeID         pgtype.UUID
	state             db.WorkspaceMountState
	fencingGeneration int64
}

func (d *Authority) PlaceReadyRun(
	ctx context.Context,
	candidate ReadyRunCandidate,
) (ReadyRunPlacement, error) {
	mount, err := d.prepareRunWorkspace(ctx, candidate)
	if err != nil {
		return ReadyRunPlacement{}, err
	}
	placement := ReadyRunPlacement{
		WorkspaceMountID:  mount.id,
		WorkerInstanceID:  mount.workerID,
		WorkerEpoch:       mount.epoch,
		RuntimeInstanceID: mount.runtimeID,
	}
	if mount.state != db.WorkspaceMountStateMounted {
		return placement, nil
	}
	lease, err := d.grantFreshRun(ctx, candidate, mount)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			stillQueued, revalidateErr := d.readyRunCandidateExists(ctx, candidate)
			if revalidateErr != nil {
				return ReadyRunPlacement{}, revalidateErr
			}
			if !stillQueued {
				return ReadyRunPlacement{}, ErrCandidateChanged
			}
		}
		return ReadyRunPlacement{}, err
	}
	placement.Lease = lease
	placement.LeaseCreated = true
	return placement, nil
}

func (d *Authority) readyRunCandidateExists(
	ctx context.Context,
	candidate ReadyRunCandidate,
) (bool, error) {
	var exists bool
	if err := d.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
      FROM runs
     WHERE org_id = $1
       AND id = $2
       AND state_version = $3
       AND status = 'queued'
       AND current_run_lease_id IS NULL
       AND (
           first_lease_at IS NOT NULL
           OR queued_expires_at IS NULL
           OR queued_expires_at > transaction_timestamp()
       )
)`, candidate.OrgID, candidate.RunID, candidate.ExpectedRunStateVersion).Scan(&exists); err != nil {
		return false, fmt.Errorf("revalidate queued Run: %w", err)
	}
	return exists, nil
}
