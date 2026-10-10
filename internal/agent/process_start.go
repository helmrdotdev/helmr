package agent

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// ProcessStart binds executable discovery and the final setup release to the
// pinned Session. Call again after slow artifact I/O: control renewal alone
// cannot authorize setup after a hold, capture, revocation or ownership change.
type ProcessStart struct {
	Authority                   RuntimeAuthority
	DeploymentID, ComputerID    uuid.UUID
	BundleDigest, AgentKey      string
	SessionKey, ParentSessionID *string
	TerminalSequence            int64
}

func AuthorizeSessionStart(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, e Execution, attachment int64) (ProcessStart, error) {
	if attachment <= 0 {
		return ProcessStart{}, ErrInvalidInput
	}
	var result ProcessStart
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		return checkSessionStart(ctx, tx, host, e, attachment, &result)
	})
	if err != nil {
		return ProcessStart{}, allocationError(err)
	}
	return result, nil
}

func checkSessionStart(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, e Execution, attachment int64, result *ProcessStart) error {
	if err := allocationLockTimeout(ctx, tx); err != nil {
		return err
	}
	if _, err := lockRuntimeAuthority(ctx, tx, host, e); err != nil {
		return err
	}
	var currentAttachment int64
	var starting bool
	if err := tx.QueryRow(ctx, `SELECT attachment_sequence,status='starting' AND fenced_at IS NULL AND failure_recorded_at IS NULL FROM session_processes
 WHERE environment_id=$1 AND session_id=$2 AND epoch=$3 FOR NO KEY UPDATE`, e.EnvironmentID, e.SessionID, e.ProcessEpoch).Scan(&currentAttachment, &starting); err != nil {
		return err
	}
	if !starting || currentAttachment != attachment {
		return ErrNotReady
	}
	kind, err := desiredSessionControl(ctx, tx, e)
	if err != nil {
		return err
	}
	if kind != "resume" {
		return ErrNotReady
	}
	if err = executionPhysical(ctx, tx, e, true, true); err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, `SELECT s.deployment_id,s.computer_id,d.bundle_digest,a.definition_key,s.session_key,s.parent_session_id::text,
 COALESCE((SELECT min(seq)-1 FROM turns WHERE environment_id=s.environment_id AND session_id=s.id AND terminal_at IS NULL),s.next_turn_seq-1)
 FROM sessions s JOIN deployments d ON (d.environment_id,d.id)=(s.environment_id,s.deployment_id)
 JOIN agent_definitions a ON (a.environment_id,a.agent_id,a.deployment_id)=(s.environment_id,s.agent_id,s.deployment_id)
 WHERE s.environment_id=$1 AND s.id=$2`, e.EnvironmentID, e.SessionID).Scan(&result.DeploymentID, &result.ComputerID, &result.BundleDigest, &result.AgentKey, &result.SessionKey, &result.ParentSessionID, &result.TerminalSequence); err != nil {
		return err
	}
	if err = computerDispatchAvailable(ctx, tx, e.EnvironmentID, result.ComputerID); err != nil {
		return err
	}
	var pendingSave bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_saves WHERE environment_id=$1 AND computer_id=$2 AND computer_lease_epoch<>$3 AND status IN ('requested','captured'))`, e.EnvironmentID, result.ComputerID, e.LeaseEpoch).Scan(&pendingSave); err != nil {
		return err
	}
	if pendingSave {
		return ErrNotReady
	}
	// Process locking and metadata reads can wait. Authority is bounded by the
	// current lease at the end, never by an envelope obtained before those waits.
	result.Authority, err = lockRuntimeAuthority(ctx, tx, host, e)
	return err
}
