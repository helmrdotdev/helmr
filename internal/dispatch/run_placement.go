package dispatch

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type ReadyRunCandidate struct {
	OrgID, RunID        pgtype.UUID
	ExpectedRunRevision int64
}
type ReadyRunPlacement struct {
	Lease                            db.RunLease
	LeaseCreated                     bool
	WorkerHostID, ComputerInstanceID pgtype.UUID
	WorkerEpoch                      int64
}

func (d *Authority) PlaceReadyRun(ctx context.Context, candidate ReadyRunCandidate) (ReadyRunPlacement, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return ReadyRunPlacement{}, err
	}
	defer rollback(ctx, tx)
	env, queue, key, err := lockRunQueueScope(ctx, tx, candidate)
	if err != nil {
		return ReadyRunPlacement{}, classifyRunCandidateError(err)
	}
	if err = lockRunSecrets(ctx, tx, candidate); err != nil {
		return ReadyRunPlacement{}, err
	}
	var computerID pgtype.UUID
	if err = tx.QueryRow(ctx, `SELECT computer_id FROM runs WHERE org_id=$1 AND id=$2 AND revision=$3`, candidate.OrgID, candidate.RunID, candidate.ExpectedRunRevision).Scan(&computerID); err != nil {
		return ReadyRunPlacement{}, classifyRunCandidateError(err)
	}
	if replaced, err := prepareComputerProgram(ctx, tx, candidate, env, computerID); err != nil {
		return ReadyRunPlacement{}, classifyRunCandidateError(err)
	} else if replaced {
		if err := tx.Commit(ctx); err != nil {
			return ReadyRunPlacement{}, err
		}
		return ReadyRunPlacement{}, ErrCapacityUnavailable
	}
	p, err := discoverComputerPlacement(ctx, tx, env, computerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReadyRunPlacement{}, ErrCapacityUnavailable
	}
	if err != nil {
		return ReadyRunPlacement{}, err
	}
	p, err = lockComputerPlacement(ctx, tx, p)
	if err != nil {
		return ReadyRunPlacement{}, classifyRunCandidateError(err)
	}
	r, err := lockRunPlacementAuthority(ctx, tx, candidate, p)
	if err != nil {
		return ReadyRunPlacement{}, classifyRunCandidateError(err)
	}
	if r.EnvironmentID != env || r.QueueName != queue || r.ConcurrencyKey != key {
		return ReadyRunPlacement{}, ErrCandidateChanged
	}
	i := p.instance
	if !i.ID.Valid {
		if p.checkpoint.Valid && p.program.Valid && p.program != r.DeploymentID {
			return ReadyRunPlacement{}, ErrCapacityUnavailable
		}
		if !p.checkpoint.Valid {
			p.program = r.DeploymentID
		}
		i, err = d.allocateComputerPlacement(ctx, tx, p)
		if err != nil {
			return ReadyRunPlacement{}, err
		}
	} else if i.ProgramDeploymentID != r.DeploymentID {
		return ReadyRunPlacement{}, ErrCapacityUnavailable
	}
	result := ReadyRunPlacement{WorkerHostID: i.WorkerHostID, ComputerInstanceID: i.ID, WorkerEpoch: i.WorkerEpoch}
	if i.ObservedState == "ready" && i.DesiredState == "ready" && i.AdmissionState == "open" && i.MountState == "mounted" && i.ObservedDesiredVersion == i.DesiredVersion {
		result.Lease, err = d.grantFreshRun(ctx, tx, r, i)
		if err != nil {
			return ReadyRunPlacement{}, classifyRunCandidateError(err)
		}
		result.LeaseCreated = true
	}
	if err = tx.Commit(ctx); err != nil {
		return ReadyRunPlacement{}, err
	}
	return result, nil
}

func classifyRunCandidateError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCandidateChanged
	}
	return err
}
