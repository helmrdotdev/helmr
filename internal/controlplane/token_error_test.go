package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/token"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type tokenErrorCase struct {
	name    string
	err     error
	status  int
	code    string
	message string
}

// tokenInputError is the token owner's rejection of an ambiguous result,
// which it reports before opening a transaction.
func tokenInputError(t *testing.T) error {
	t.Helper()
	_, err := new(token.Tokens).Complete(t.Context(), token.Target{}, json.RawMessage(`{"a":1,"a":2}`), "")
	var input token.InputError
	if !errors.As(err, &input) {
		t.Fatalf("ambiguous result error = %v, want token.InputError", err)
	}
	return err
}

func TestTokenErrorMapsOwnerErrors(t *testing.T) {
	internal := errors.New("database is down")
	assertTokenErrors(t, tokenError, []tokenErrorCase{
		{"stale claims first", errors.Join(token.ErrCreateAuthority, workergroup.ErrStaleClaims), http.StatusUnauthorized, "unauthorized", "worker authentication is required"},
		{"receipt expired", fmt.Errorf("claim: %w", idempotency.ExpiredError{}), http.StatusGone, "operation_expired", "operation receipt has expired"},
		{"idempotency conflict", idempotency.ConflictError{}, http.StatusConflict, "idempotency_conflict", "idempotency key conflicts with an earlier token operation"},
		{"create authority", fmt.Errorf("lock: %w", token.ErrCreateAuthority), http.StatusConflict, "conflict", "token create source authority is stale"},
		{"no rows", fmt.Errorf("complete claim: %w", pgx.ErrNoRows), http.StatusNotFound, "token_not_found", "token was not found"},
		{"not found", token.ErrNotFound, http.StatusNotFound, "token_not_found", "token was not found"},
		{"input", tokenInputError(t), http.StatusBadRequest, "bad_request", "result must be unambiguous JSON"},
		{"expired", &token.ExpiredError{}, http.StatusGone, "token_expired", "token has expired"},
		{"completion conflict", token.ErrCompletionConflict, http.StatusConflict, "token_completion_conflict", "token completion conflicts with the existing result"},
		{"cancelled", token.ErrCancelled, http.StatusConflict, "token_cancelled", "token was cancelled"},
		{"completed", token.ErrCompleted, http.StatusConflict, "token_completed", "token is already completed"},
		{"receipt invalid", token.ErrReceiptInvalid, http.StatusInternalServerError, "internal_error", "internal server error"},
		{"credential denied", token.ErrCredentialDenied, http.StatusInternalServerError, "internal_error", "internal server error"},
		{"stale source without claims", run.ErrStaleSource, http.StatusInternalServerError, "internal_error", "internal server error"},
		{"internal", internal, http.StatusInternalServerError, "internal_error", "internal server error"},
	})
}

func TestPublicTokenErrorPreservesInputAndInfrastructureFailures(t *testing.T) {
	denied := func(name string, err error) tokenErrorCase {
		return tokenErrorCase{name, err, http.StatusUnauthorized, "token_scope_denied", "token credential is invalid"}
	}
	assertTokenErrors(t, publicTokenError, []tokenErrorCase{
		{"completion conflict", fmt.Errorf("complete: %w", token.ErrCompletionConflict), http.StatusConflict, "token_completion_conflict", "token completion conflicts with the existing result"},
		{"expired", &token.ExpiredError{}, http.StatusGone, "token_expired", "token has expired"},
		{"cancelled", token.ErrCancelled, http.StatusConflict, "token_cancelled", "token was cancelled"},
		denied("credential denied", token.ErrCredentialDenied),
		{"input", tokenInputError(t), http.StatusBadRequest, "bad_request", "result must be unambiguous JSON"},
		{"completed", token.ErrCompleted, http.StatusInternalServerError, "internal_error", "internal server error"},
		denied("not found", token.ErrNotFound),
		denied("no rows", pgx.ErrNoRows),
		{"receipt expired", idempotency.ExpiredError{}, http.StatusInternalServerError, "operation_expired", "internal server error"},
		{"idempotency conflict", idempotency.ConflictError{}, http.StatusInternalServerError, "internal_error", "internal server error"},
		{"receipt invalid", token.ErrReceiptInvalid, http.StatusInternalServerError, "internal_error", "internal server error"},
		{"stale claims", workergroup.ErrStaleClaims, http.StatusInternalServerError, "internal_error", "internal server error"},
		{"internal", errors.New("database is down"), http.StatusInternalServerError, "internal_error", "internal server error"},
	})
}

func assertTokenErrors(t *testing.T, mapper func(error) error, cases []tokenErrorCase) {
	t.Helper()
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeError(recorder, mapper(test.err))
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", recorder.Code, test.status, recorder.Body.String())
			}
			body := decodeHTTPError(t, recorder.Body.Bytes())
			if body.Code != test.code || body.Message != test.message {
				t.Fatalf("error = %s %q, want %s %q", body.Code, body.Message, test.code, test.message)
			}
		})
	}
}
