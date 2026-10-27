package session

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// AdmitMCP commits the authenticated request admission before dispatch. The
// selected operation owns its own accepted effect and terminal outcome facts.
func AdmitMCP(ctx context.Context, policy auditpolicy.Policy, pool db.TxBeginner, caller Caller, tool string) error {
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		// Transport admission must permit reconciliation of an existing receipt
		// during a hold. Each operation still owns fresh business admission.
		if caller.Kind != "session" || caller.ID == uuid.Nil() || caller.ID != caller.Execution.SessionID {
			return ErrDenied
		}
		if err := lockRuntimeHost(ctx, tx, caller); err != nil {
			return err
		}
		owner, err := lockSession(ctx, tx, caller.Execution.EnvironmentID, caller.ID)
		if err != nil {
			return hideMissing(err)
		}
		if err := executionReceipt(ctx, tx, caller.Execution, owner); err != nil {
			return err
		}
		if _, err := RuntimeMCPPolicy(ctx, tx, caller); err != nil {
			return err
		}
		origin, err := loadSessionOrigin(ctx, tx, caller.Execution.EnvironmentID, caller.ID)
		if err != nil {
			return err
		}
		return audit.Append(ctx, tx, policy, audit.Event{Evidence: callerRuntimeAuditEvidence(caller), EnvironmentID: caller.Execution.EnvironmentID, SessionID: caller.ID, Action: "mcp.admitted", Outcome: "accepted", TargetKind: "mcp", TargetID: caller.ID, Causality: audit.Causality{InitiatorKind: origin.kind(), InitiatorID: origin.id()}, Details: audit.MCPDetails{Tool: tool}})
	})
	if err == nil || errors.Is(err, audit.ErrCapture) || db.CommitOutcomeUnknown(err) {
		return err
	}
	// The authenticated worker is known, but a failed runtime check does not
	// establish the claimed Session or Environment as the caller's custody.
	actor := audit.Unauthenticated("absent", "")
	if caller.Host != nil {
		actor = audit.Worker(caller.Host.HostID, caller.Host.HostClaimVersion)
		if refusal, ok := errors.AsType[*audit.Refusal](err); ok {
			actor.Authorization = refusal.Authorization
		}
	}
	reason, outcome := "operation_failed", "failed"
	if errors.Is(err, ErrDenied) {
		reason, outcome = "stale_authority", "denied"
	}
	details := audit.FailureDetails{}
	if audit.KnownMCPTool(tool) {
		details.Tool = tool
	}
	if captureErr := audit.RecordFailure(ctx, pool, policy, audit.Event{Evidence: actor, Action: "mcp.refused", Outcome: outcome, TargetKind: "mcp", Unresolved: true, Details: details}, reason); captureErr != nil {
		return captureErr
	}
	return err
}
