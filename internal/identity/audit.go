package identity

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/jackc/pgx/v5"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db"
)

// Authentication failures carry a fixed reason, never provider/error payloads or
// candidate email addresses. Until a binding is verified they are platform facts.
func recordAuthenticationFailure(ctx context.Context, database db.TxBeginner, cfg Config, event audit.Event, result *error) {
	err := *result
	if err == nil || errors.Is(err, ErrAuthorizationPending) || errors.Is(err, audit.ErrCapture) || db.CommitOutcomeUnknown(err) {
		return
	}
	reason, outcome := "operation_failed", "failed"
	switch {
	case errors.Is(err, ErrInvalidToken), errors.Is(err, ErrWrongAccount), errors.Is(err, ErrInvalidDeviceCode), errors.Is(err, ErrDeviceCodeNotFound), errors.Is(err, ErrDeviceCodeExpired):
		reason, outcome = "authentication_invalid", "denied"
	case errors.Is(err, ErrDeviceAccessDenied), errors.Is(err, ErrDeviceApproverChanged), errors.Is(err, ErrOrganizationRequired), errors.Is(err, access.ErrDenied):
		reason, outcome = "permission_denied", "denied"
	case errors.Is(err, ErrAlreadyMember):
		reason, outcome = "state_conflict", "denied"
	}
	event.Outcome = outcome
	event.Unresolved = true
	if captureErr := audit.RecordFailure(ctx, database, cfg.auditPolicy, event, reason); captureErr != nil {
		*result = captureErr
	}
}

func accountSecurityEvent(ctx context.Context, tx pgx.Tx, cfg Config, p auth.Principal, action string, credential uuid.UUID, method string) error {
	if err := audit.Append(ctx, tx, cfg.auditPolicy, audit.Event{Evidence: audit.FromPrincipal(p), Action: action, TargetKind: "user", TargetID: p.UserID, TargetUserID: p.UserID, Details: audit.IdentitySecurityDetails{CredentialID: credential, Method: method}}); err != nil {
		return err
	}
	if action == "identity.profile_updated" {
		return nil
	}
	var recipient string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(primary_email,'') FROM users WHERE id=$1`, p.UserID).Scan(&recipient); err != nil {
		return err
	}
	return enqueueSecurityNotification(ctx, tx, p.UserID, action, recipient)
}
func operatorSecurityEvent(ctx context.Context, tx pgx.Tx, cfg Config, p auth.Principal, action string, credential uuid.UUID) error {
	target, kind := p.OperatorID, "operator"
	if action == "operator.sign_in" || action == "operator.session_confirmed" || action == "operator.session_revoked" {
		target, kind = p.SessionID, "login_session"
	}
	evidence := audit.FromPrincipal(p)
	if p.SessionID == uuid.Nil() {
		evidence.Authentication.CredentialKind = "passkey"
		evidence.Authentication.CredentialID = p.PasskeyID
	}
	return audit.Append(ctx, tx, cfg.auditPolicy, audit.Event{Evidence: evidence, Action: action, TargetKind: kind, TargetID: target, Details: audit.IdentitySecurityDetails{CredentialID: credential, Method: "passkey"}})
}
