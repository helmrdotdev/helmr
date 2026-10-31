package session

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// SessionDelivery is the current process-owned control, not a public acceptance
// receipt. Its identity survives an uncertain response on the same attachment.
type SessionDelivery struct {
	StopSequence int64
	StopTurnID   *uuid.UUID
	StopDeadline *time.Time
	Sequence     int64
	Generation   int64
	Kind         string
	Acknowledged bool
	Error        string
	ExpiresAt    time.Time
}

// PrepareSessionDelivery reconciles desired state with the exact current host
// attachment. Reattachment creates a new delivery even when state is unchanged:
// the guest may have acquired a new hold while a prior receipt was in flight.
func PrepareSessionDelivery(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, attachment int64) (SessionDelivery, error) {
	var result SessionDelivery
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		// An unchanged delivery is an observation, not a tree control decision.
		// One statement observes current ownership, holds and delivery together.
		var found bool
		var err error
		result, found, err = readCurrentSessionDelivery(ctx, tx, host, e, attachment)
		if err != nil || found {
			return err
		}
		authority, err := lockRuntimeAuthority(ctx, tx, host, e)
		if err != nil {
			return err
		}
		kind, err := desiredSessionControl(ctx, tx, e)
		if err != nil {
			return err
		}
		var sequence, priorAttachment, generation int64
		var priorKind, failure *string
		var acknowledged *time.Time
		var currentAttachment int64
		var failureRecorded bool
		if err = tx.QueryRow(ctx, `SELECT attachment_sequence,control_sequence,control_attachment,control_generation,control_kind,control_acknowledged_at,control_error,failure_recorded_at IS NOT NULL
 FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&currentAttachment, &sequence, &priorAttachment, &generation, &priorKind, &acknowledged, &failure, &failureRecorded); err != nil {
			return err
		}
		if attachment <= 0 || attachment != currentAttachment {
			return ErrDenied
		}
		// A recorded runtime rejection survives reattachment and control changes
		// until its lifecycle failure is durable. It must never become a fresh retry.
		pendingFailure := failure != nil && !failureRecorded
		if pendingFailure {
			kind = *priorKind
		}
		if !pendingFailure && (priorKind == nil || *priorKind != kind || generation != authority.Generation || priorAttachment != attachment) {
			if sequence == 9223372036854775807 {
				return ErrNotReady
			}
			sequence++
			if _, err = tx.Exec(ctx, `UPDATE session_processes SET control_sequence=$4,control_attachment=$5,control_generation=$6,control_kind=$7,control_acknowledged_at=NULL,control_error=NULL
 WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch, sequence, attachment, authority.Generation, kind); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `UPDATE sessions SET idle_since=NULL WHERE environment_id=$1 AND id=$2`, e.EnvironmentID, e.SessionID); err != nil {
				return err
			}
			if err = notifySessionsChanged(ctx, tx, e.EnvironmentID, e.SessionID); err != nil {
				return err
			}
			acknowledged, failure = nil, nil
		}
		// A process-row wait may consume the remaining physical lease lifetime.
		authority, err = lockRuntimeAuthority(ctx, tx, host, e)
		if err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, `SELECT stop_sequence,stop_turn_id,stop_deadline_at FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&result.StopSequence, &result.StopTurnID, &result.StopDeadline); err != nil {
			return err
		}
		result = SessionDelivery{StopSequence: result.StopSequence, StopTurnID: result.StopTurnID, StopDeadline: result.StopDeadline, Sequence: sequence, Generation: authority.Generation, Kind: kind, Acknowledged: acknowledged != nil, ExpiresAt: authority.ExpiresAt}
		if failure != nil {
			result.Error = *failure
		}
		return nil
	})
	if err != nil {
		return SessionDelivery{}, hideMissing(err)
	}
	return result, nil
}

func desiredSessionControl(ctx context.Context, tx pgx.Tx, e Execution) (string, error) {
	var stopped, held bool
	err := tx.QueryRow(ctx, `SELECT s.status='closed' OR p.status IN ('stopping','lost','stopped') OR (p.stop_deadline_at IS NOT NULL AND p.stop_turn_id IS NULL),
 `+db.SessionHeldSQL+`
 FROM sessions s
 JOIN session_processes p ON (p.environment_id,p.session_id)=(s.environment_id,s.id)
 WHERE s.environment_id=$1 AND s.id=$2 AND p.epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&stopped, &held)
	if err != nil {
		return "", err
	}
	if stopped {
		return "shutdown", nil
	}
	if held {
		return "suspend", nil
	}
	return "resume", nil
}

// AcknowledgeSessionDelivery records application only. A shutdown delivery result
// is not process fencing; only a separately observed physical stop can do that.
func AcknowledgeSessionDelivery(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, attachment, sequence, generation int64, kind, failure string, stopSequence int64) error {
	if sequence <= 0 || generation <= 0 || len(failure) > 4096 || !utf8.ValidString(failure) || strings.ContainsRune(failure, 0) || (kind != "suspend" && kind != "resume" && kind != "shutdown") {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		authority, err := lockRuntimeAuthority(ctx, tx, host, e)
		if err != nil {
			return err
		}
		currentKind, err := desiredSessionControl(ctx, tx, e)
		if err != nil {
			return err
		}
		if generation != authority.Generation || kind != currentKind {
			return ErrConflict
		}
		var acknowledged *time.Time
		var previousError *string
		var stoppedTurn *uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT control_acknowledged_at,control_error,stop_turn_id FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&acknowledged, &previousError, &stoppedTurn); err != nil {
			return err
		}
		changed := failure == "" && (acknowledged == nil || kind != "shutdown" && stoppedTurn != nil) || failure != "" && acknowledged == nil && (previousError == nil || *previousError != failure)
		tag, err := tx.Exec(ctx, `UPDATE session_processes SET
 control_acknowledged_at=CASE WHEN $8='' THEN COALESCE(control_acknowledged_at,clock_timestamp()) ELSE control_acknowledged_at END,
 control_error=CASE WHEN control_acknowledged_at IS NULL THEN NULLIF($8,'') ELSE control_error END,
 stop_turn_id=CASE WHEN $8='' AND $7<>'shutdown' AND stop_turn_id IS NOT NULL THEN NULL ELSE stop_turn_id END,
 stop_deadline_at=CASE WHEN $8='' AND $7<>'shutdown' AND stop_turn_id IS NOT NULL THEN NULL ELSE stop_deadline_at END
 WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 AND attachment_sequence=$4 AND control_attachment=$4 AND control_sequence=$5 AND control_generation=$6 AND control_kind=$7 AND ($8<>'' OR $7='shutdown' OR (stop_deadline_at IS NULL) OR (stop_sequence=$9 AND stop_turn_id IS NOT NULL AND stop_deadline_at>clock_timestamp()))`, e.EnvironmentID, e.SessionID, e.ProcessEpoch, attachment, sequence, generation, kind, failure, stopSequence)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
		_, err = lockRuntimeAuthority(ctx, tx, host, e)
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		return notifySessionsChanged(ctx, tx, e.EnvironmentID, e.SessionID)
	}))
}

// ObserveSessionStopped consumes the trusted guest's physical-close event. It
// cannot be substituted with a shutdown command acknowledgement. A newer control
// generation does not resurrect an already stopped process generation.
func ObserveSessionStopped(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, attachment int64) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockRuntimeHost(ctx, tx, Caller{Kind: "session", ID: e.SessionID, Execution: e, Host: &host}); err != nil {
			return err
		}
		o, err := lockSession(ctx, tx, e.EnvironmentID, e.SessionID)
		if err != nil {
			return err
		}
		var currentAttachment int64
		var stopped bool
		if err = tx.QueryRow(ctx, `SELECT attachment_sequence,status='stopped' AND fenced_at IS NOT NULL FROM session_processes
 WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 AND computer_lease_epoch=$4 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, e.ProcessEpoch, e.LeaseEpoch).Scan(&currentAttachment, &stopped); err != nil {
			return err
		}
		if attachment <= 0 || attachment != currentAttachment {
			return ErrDenied
		}
		var owned bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_leases l WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND worker_host_id=$4 AND worker_epoch=$5
 AND status IN ('active','acquiring') AND fenced_at IS NULL AND expires_at>clock_timestamp())`, e.EnvironmentID, o.computer, e.LeaseEpoch, host.HostID, host.Epoch).Scan(&owned); err != nil {
			return err
		}
		if !owned {
			return ErrDenied
		}
		if stopped {
			return nil
		}
		kind, err := desiredSessionControl(ctx, tx, e)
		if err != nil {
			return err
		}
		if err = interruptRunningSessionTurn(ctx, auditPolicy, tx, auditWorkerActor(host, e), e); err != nil {
			return err
		}
		if kind != "shutdown" {
			if err = recordSessionFailure(ctx, auditPolicy, tx, auditWorkerActor(host, e), e, failureProcessFailed); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2 AND epoch=$3`, e.EnvironmentID, e.SessionID, e.ProcessEpoch); err != nil {
			return err
		}
		return event(ctx, tx, e.EnvironmentID, e.SessionID, uuid.Nil(), "session.process_stopped")
	}))
}

func readCurrentSessionDelivery(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, e Execution, attachment int64) (SessionDelivery, bool, error) {
	if host.HostID != e.WorkerHostID || host.Epoch != e.WorkerEpoch || attachment <= 0 {
		return SessionDelivery{}, false, ErrDenied
	}
	var result SessionDelivery
	err := tx.QueryRow(ctx, `SELECT p.control_sequence,p.control_generation,p.control_kind,p.control_acknowledged_at IS NOT NULL,COALESCE(p.control_error,''),p.stop_sequence,p.stop_turn_id,p.stop_deadline_at,LEAST(l.expires_at,clock_timestamp()+interval '30 seconds')
 FROM session_processes p
 JOIN sessions s ON (s.environment_id,s.id)=(p.environment_id,p.session_id)
 JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(p.environment_id,p.computer_id,p.computer_lease_epoch)
 JOIN computers c ON (c.environment_id,c.id)=(l.environment_id,l.computer_id)
 JOIN worker_hosts h ON h.id=l.worker_host_id
 JOIN worker_groups g ON g.id=h.worker_group_id
 WHERE p.environment_id=$1 AND p.session_id=$2 AND p.epoch=$3 AND l.epoch=$4
 AND h.id=$5 AND h.current_epoch=$6 AND l.worker_epoch=$6 AND g.id=$7 AND g.claim_version=$8 AND h.claim_version=$9
 AND h.status IN ('active','draining') AND g.status IN ('active','paused','draining')
 AND l.fenced_at IS NULL AND l.expires_at>clock_timestamp() AND c.integrity_fault_at IS NULL AND c.deleted_at IS NULL
 AND ((l.status='active' AND p.status IN ('ready','starting','stopping') AND p.fenced_at IS NULL)
 OR (l.status='acquiring' AND p.status<>'stopped' AND EXISTS(SELECT 1 FROM checkpoints cp JOIN checkpoint_processes m ON (m.environment_id,m.checkpoint_id)=(cp.environment_id,cp.id)
 WHERE cp.environment_id=l.environment_id AND cp.computer_id=l.computer_id AND cp.target_lease_epoch=l.epoch AND cp.status IN ('restoring','consumed') AND cp.controls_reconciled_at IS NULL
 AND m.session_id=p.session_id AND m.process_epoch=p.epoch)))
 AND p.attachment_sequence=$10 AND p.control_attachment=$10 AND p.control_generation=s.authority_generation
 AND p.control_kind=CASE WHEN s.status='closed' OR p.status IN ('stopping','lost','stopped') OR (p.stop_deadline_at IS NOT NULL AND p.stop_turn_id IS NULL) THEN 'shutdown'
 WHEN `+db.SessionHeldSQL+` THEN 'suspend' ELSE 'resume' END`,
		e.EnvironmentID, e.SessionID, e.ProcessEpoch, e.LeaseEpoch, host.HostID, host.Epoch, host.GroupID, host.GroupClaimVersion, host.HostClaimVersion, attachment).Scan(&result.Sequence, &result.Generation, &result.Kind, &result.Acknowledged, &result.Error, &result.StopSequence, &result.StopTurnID, &result.StopDeadline, &result.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionDelivery{}, false, nil
	}
	return result, err == nil, err
}
