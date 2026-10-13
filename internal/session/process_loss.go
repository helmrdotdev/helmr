package session

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

func LoseLeaseProcesses(ctx context.Context, tx pgx.Tx, env, computerID uuid.UUID, epoch int64, members []ComputerMember, physicalStopped bool) error {
	for _, member := range members {
		if member.LeaseEpoch != epoch {
			continue
		}
		var save uuid.UUID
		err := tx.QueryRow(ctx, `SELECT cp.disk_save_id FROM checkpoints cp
 JOIN checkpoint_processes m ON (m.environment_id,m.checkpoint_id)=(cp.environment_id,cp.id)
 WHERE cp.environment_id=$1 AND cp.computer_id=$2 AND cp.status IN ('ready','restoring')
 AND (cp.source_lease_epoch=$3 OR cp.target_lease_epoch=$3) AND m.session_id=$4 AND m.process_epoch=$5`, env, computerID, epoch, member.Session, member.Epoch).Scan(&save)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			retained, err := computer.CheckpointDiskEligible(ctx, tx, env, computerID, save)
			if err != nil {
				return err
			}
			if retained {
				continue
			}
		}
		e := Execution{EnvironmentID: env, SessionID: member.Session, ProcessEpoch: member.Epoch, LeaseEpoch: epoch}
		if err := LoseProcess(ctx, tx, e, physicalStopped); err != nil {
			return err
		}
	}
	return nil
}

// The caller holds all owner/Computer locks. The failure marker makes a later
// physical fence or checkpoint-loss reconciliation safe after explicit hold release.
func LoseProcess(ctx context.Context, tx pgx.Tx, e Execution, physicalStopped bool) error {
	var recorded, terminal, fenced, neverStarted bool
	if err := tx.QueryRow(ctx, `SELECT p.failure_recorded_at IS NOT NULL,s.status IN ('closed','cancelled') OR EXISTS(SELECT 1 FROM (`+db.RevokedComputerSecretsSQL+`) revoked WHERE revoked.environment_id=s.environment_id AND revoked.computer_id=s.computer_id),p.fenced_at IS NOT NULL, p.status IN ('starting','stopping') AND p.attachment_sequence=0 AND p.failure_recorded_at IS NULL AND l.initialized_at IS NULL AND l.restored_from_save_id IS NULL
 FROM session_processes p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch) JOIN sessions s ON (s.environment_id,s.id)=(p.environment_id,p.session_id)
 WHERE p.environment_id=$1 AND p.session_id=$2 AND p.epoch=$3 AND p.computer_lease_epoch=$4 FOR NO KEY UPDATE OF p`, e.EnvironmentID, e.SessionID, e.ProcessEpoch, e.LeaseEpoch).Scan(&recorded, &terminal, &fenced, &neverStarted); err != nil {
		return err
	}
	// Immutable checkpoint members may already have a confirmed physical stop.
	// Loss of the remaining Computer must not rewrite that outcome or its hold.
	if fenced {
		return nil
	}
	revocationRecorded, err := recordComputerSecretRevocationFailure(ctx, tx, e)
	if err != nil {
		return err
	}
	// An initial assignment on a never-ready fresh VM cannot have run setup.
	// Expiry retains custody; confirmed absence retires it without a false
	// execution-loss hold. Independent controls and revocations still win.
	if neverStarted && !terminal && !revocationRecorded {
		if physicalStopped {
			_, err = tx.Exec(ctx, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch)
		}
		return err
	}
	if !recorded && !terminal && !revocationRecorded {
		if err := recordSessionFailure(ctx, tx, e, "Computer execution authority lost without an eligible continuation"); err != nil {
			return err
		}
	}
	if err := interruptRunningSessionTurn(ctx, tx, e); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE session_processes SET status='lost',failure_recorded_at=COALESCE(failure_recorded_at,clock_timestamp()),
 fenced_at=CASE WHEN $4 THEN COALESCE(fenced_at,clock_timestamp()) ELSE fenced_at END
 WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch, physicalStopped)
	return err
}
