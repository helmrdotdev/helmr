package dispatch

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
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
	if replaced, err := replaceComputerProgram(ctx, tx, candidate, env, computerID); err != nil {
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
	r, err := lockRunPlacementAuthority(ctx, tx, candidate, p.computer.EnvironmentID, p.computer.ID)
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

// replaceComputerProgram takes the program replacement step the Run's
// Computer needs, and reports whether it took one; Run placement retries
// after this transaction. Disk promotion requires no capacity for the
// obsolete VM.
func replaceComputerProgram(ctx context.Context, tx pgx.Tx, candidate ReadyRunCandidate, environmentID, computerID pgtype.UUID) (bool, error) {
	var deploymentID pgtype.UUID
	if err := tx.QueryRow(ctx, `SELECT deployment_id FROM runs WHERE id=$1 AND org_id=$2 AND revision=$3`, candidate.RunID, candidate.OrgID, candidate.ExpectedRunRevision).Scan(&deploymentID); err != nil {
		return false, err
	}
	replacement, err := computer.LockReplacement(ctx, tx, computer.ReplacementRef{EnvironmentID: pgvalue.MustUUIDValue(environmentID), ComputerID: pgvalue.MustUUIDValue(computerID), DeploymentID: pgvalue.MustUUIDValue(deploymentID)})
	if err != nil {
		return false, err
	}
	switch replacement.Kind() {
	case computer.ReplacementCapture:
		// Capture locks the stable resident set before any new target member lock.
		if _, err = replacement.Capture(ctx); err != nil {
			return false, err
		}
		_, err = lockRunPlacementAuthority(ctx, tx, candidate, environmentID, computerID)
		return err == nil, err
	case computer.ReplacementPromotion:
		if _, err = lockRunPlacementAuthority(ctx, tx, candidate, environmentID, computerID); err != nil {
			return false, err
		}
		err = replacement.Promote(ctx)
		return err == nil, err
	default:
		return false, nil
	}
}

func classifyRunCandidateError(err error) error {
	switch {
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, computer.ErrReplacementChanged):
		return ErrCandidateChanged
	case errors.Is(err, computer.ErrReplacementBlocked):
		return ErrCapacityUnavailable
	}
	return err
}
