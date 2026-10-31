package session

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/jackc/pgx/v5"
)

func LoseLeaseProcesses(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env, computerID uuid.UUID, epoch int64, members []ComputerMember, physicalStopped bool, disposition ComputerLossDisposition) error {
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
		if err := loseProcess(ctx, auditPolicy, tx, e, physicalStopped, disposition, computerID); err != nil {
			return err
		}
	}
	return nil
}

// The caller holds all owner/Computer locks. The failure marker makes a later
// physical fence or checkpoint-loss reconciliation safe.
// ComputerLossDisposition distinguishes a recoverable Computer loss from an
// independent runtime crash. Only the latter counts toward a failure hold.
type ComputerLossDisposition bool

const (
	ComputerLossRequiresAttention ComputerLossDisposition = false
	ComputerLossRecoverable       ComputerLossDisposition = true
)

// LoseProcess exposes independent runtime loss to cross-package fixtures.
// Production Computer loss uses LoseLeaseProcesses and its explicit disposition.
func LoseProcess(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, e Execution, physicalStopped bool) error {
	return loseProcess(ctx, auditPolicy, tx, e, physicalStopped, ComputerLossRequiresAttention, uuid.Nil())
}

const ComputerExecutionLost = "computer_execution_lost"

// ComputerLossInterruption is an event-time fact, independent of later process
// reconstruction, hold acknowledgment or event projection timing. RecoveryGap
// is whether the Session is held by a recovery gap when the Turn is lost.
type ComputerLossInterruption struct {
	Reason      string    `json:"reason"`
	ComputerID  uuid.UUID `json:"computerId"`
	LeaseEpoch  int64     `json:"leaseEpoch"`
	RecoveryGap bool      `json:"recoveryGap"`
}

func loseProcess(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, e Execution, physicalStopped bool, disposition ComputerLossDisposition, lossComputer uuid.UUID) error {
	var recorded, terminal, fenced, neverStarted bool
	if err := tx.QueryRow(ctx, `SELECT p.failure_recorded_at IS NOT NULL,s.status='closed',p.fenced_at IS NOT NULL, p.status IN ('starting','stopping') AND p.attachment_sequence=0 AND p.failure_recorded_at IS NULL AND l.initialized_at IS NULL AND l.restored_from_save_id IS NULL
 FROM session_processes p JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch) JOIN sessions s ON (s.environment_id,s.id)=(p.environment_id,p.session_id)
 WHERE p.environment_id=$1 AND p.session_id=$2 AND p.epoch=$3 AND p.computer_lease_epoch=$4 FOR NO KEY UPDATE OF p`, e.EnvironmentID, e.SessionID, e.ProcessEpoch, e.LeaseEpoch).Scan(&recorded, &terminal, &fenced, &neverStarted); err != nil {
		return err
	}
	// Immutable checkpoint members may already have a confirmed physical stop.
	// Loss of the remaining Computer must not rewrite that outcome or its hold.
	if fenced {
		return nil
	}
	// An initial assignment on a never-ready fresh VM cannot have run setup.
	// Expiry retains custody; confirmed absence retires it without a false
	// execution-loss hold. Independent controls still win.
	var err error
	if neverStarted && !terminal {
		if physicalStopped {
			_, err = tx.Exec(ctx, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch)
		}
		if err != nil {
			return err
		}
		return notifySessionsChanged(ctx, tx, e.EnvironmentID, e.SessionID)
	}
	if !recorded {
		reason := failureComputerLost
		if disposition == ComputerLossRequiresAttention {
			reason = failureExecutionLost
		}
		if err := recordSessionFailure(ctx, auditPolicy, tx, audit.Internal(audit.ControlPlane), e, reason); err != nil {
			return err
		}
	}
	var interruption json.RawMessage
	if lossComputer != uuid.Nil() {
		// Recovery gap settlement precedes process loss. Only a Session held by
		// the gap has its interruption explained by the acknowledgement request;
		// a notified peer still learns that its running request was lost.
		var gap bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM session_holds WHERE environment_id=$1 AND session_id=$2 AND kind='recovery_gap' AND released_at IS NULL)`, e.EnvironmentID, e.SessionID).Scan(&gap); err != nil {
			return err
		}
		interruption, err = json.Marshal(ComputerLossInterruption{ComputerExecutionLost, lossComputer, e.LeaseEpoch, gap})
		if err != nil {
			return err
		}
	}
	if err := interruptRunningSessionTurnWithData(ctx, auditPolicy, tx, audit.Internal(audit.ControlPlane), e, interruption); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE session_processes SET status='lost',
 fenced_at=CASE WHEN $4 THEN COALESCE(fenced_at,clock_timestamp()) ELSE fenced_at END
 WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch, physicalStopped)
	if err != nil {
		return err
	}
	return notifySessionsChanged(ctx, tx, e.EnvironmentID, e.SessionID)
}
