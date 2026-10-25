package identity

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db"
)

// Authentication failures carry a fixed reason, never provider/error payloads or
// candidate email addresses. Until a binding is verified they are installation facts.
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
