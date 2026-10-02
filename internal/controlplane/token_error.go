package controlplane

import (
	"errors"

	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/token"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// tokenError maps a token owner error from a Token creation, completion or
// cancellation to the API error the caller is told about. Stale worker
// claims ask a worker to re-authenticate before anything else is
// considered. An expired idempotency receipt is gone, a reused key with
// another request and a stale runtime creation source are conflicts, a
// missing Token is not found, rejected input is a bad request and terminal
// Token states are coded conflicts, or gone for an expired Token. Other
// errors pass through unchanged.
func tokenError(err error) error {
	if errors.Is(err, workergroup.ErrStaleClaims) {
		return unauthorized(errors.New("worker authentication is required"))
	}
	var receiptExpired idempotency.ExpiredError
	var collision idempotency.ConflictError
	var input token.InputError
	var expired *token.ExpiredError
	switch {
	case errors.As(err, &receiptExpired):
		return gone(receiptExpired)
	case errors.As(err, &collision):
		return conflict(codedError{code: "idempotency_conflict", message: "idempotency key conflicts with an earlier token operation"})
	case errors.Is(err, token.ErrCreateAuthority):
		return conflict(token.ErrCreateAuthority)
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, token.ErrNotFound):
		return notFound(errTokenNotFound)
	case errors.As(err, &input):
		return badRequest(input)
	case errors.As(err, &expired):
		return gone(errTokenExpired)
	case errors.Is(err, token.ErrCompletionConflict):
		return conflict(errTokenCompletionConflict)
	case errors.Is(err, token.ErrCancelled):
		return conflict(errTokenCancelled)
	case errors.Is(err, token.ErrCompleted):
		return conflict(errTokenCompleted)
	default:
		return err
	}
}

// publicTokenError maps a callback or bearer completion error. Only the
// terminal outcomes and input rejections retain their status. Credential
// mismatches conceal resource existence; internal failures remain internal.
func publicTokenError(err error) error {
	var expired *token.ExpiredError
	var input token.InputError
	switch {
	case errors.Is(err, token.ErrCompletionConflict):
		return conflict(errTokenCompletionConflict)
	case errors.As(err, &expired):
		return gone(errTokenExpired)
	case errors.Is(err, token.ErrCancelled):
		return conflict(errTokenCancelled)
	case errors.Is(err, token.ErrCredentialDenied), errors.Is(err, token.ErrNotFound), errors.Is(err, pgx.ErrNoRows):
		return unauthorized(errTokenScopeDenied)
	case errors.As(err, &input):
		return badRequest(input)
	default:
		return err
	}
}
