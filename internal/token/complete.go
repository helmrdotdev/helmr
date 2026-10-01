package token

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

type operationReceipt struct {
	TokenID string `json:"token_id"`
	Outcome string `json:"outcome"`
}

// completionAuthorizer locks and authorizes the Token a public completion
// addresses inside its transaction, returning the public access credential
// it used, if any.
type completionAuthorizer func(context.Context, *db.Queries) (Target, *db.PublicAccessToken, error)

// Complete completes an authorized Token with result in one transaction: the
// optional idempotency claim, then the completion, which locks the Token and
// publishes its reconciliation intent. Completing again with the same result
// succeeds; another result is ErrCompletionConflict, and a cancelled Token is
// ErrCancelled. An expired Token is *ExpiredError after the expiry and the
// claim's failed receipt commit. A result that is not unambiguous JSON is an
// InputError.
func (t *Tokens) Complete(ctx context.Context, target Target, result json.RawMessage, idempotencyKey string) (db.Token, error) {
	return t.complete(ctx, target, result, idempotencyKey, nil)
}

// CompleteWithCallback completes the Token the callback secret addresses, as
// Complete does without an idempotency key. The Token is locked and its
// callback secret verified inside the transaction; a secret that does not
// address it is ErrCredentialDenied.
func (t *Tokens) CompleteWithCallback(ctx context.Context, tokenID uuid.UUID, callbackSecret string, result json.RawMessage) (db.Token, error) {
	return t.complete(ctx, Target{}, result, "", func(ctx context.Context, q *db.Queries) (Target, *db.PublicAccessToken, error) {
		fingerprint := auth.HashCredential(callbackSecret)
		row, err := q.GetTokenForCallbackCompletion(ctx, db.GetTokenForCallbackCompletionParams{
			ID: pgvalue.UUID(tokenID), CallbackSecretFingerprint: fingerprint,
		})
		if err != nil {
			return Target{}, nil, ErrCredentialDenied
		}
		credentials, err := t.key.Derive(pgvalue.MustUUIDValue(row.ID))
		if err != nil ||
			!hmac.Equal(credentials.CallbackFingerprint, row.CallbackSecretFingerprint) ||
			!hmac.Equal(credentials.CallbackFingerprint, fingerprint) {
			return Target{}, nil, ErrCredentialDenied
		}
		return TargetOf(row), nil, nil
	})
}

// CompleteWithBearer completes the Token a public access token addresses, as
// Complete does without an idempotency key. The credential is locked and
// verified inside the transaction and counted as used only when the
// completion published a reconciliation intent; a credential that does not
// address the Token is ErrCredentialDenied.
func (t *Tokens) CompleteWithBearer(ctx context.Context, tokenID uuid.UUID, bearer string, result json.RawMessage) (db.Token, error) {
	return t.complete(ctx, Target{}, result, "", func(ctx context.Context, q *db.Queries) (Target, *db.PublicAccessToken, error) {
		publicAccess, err := q.LockPublicAccessTokenByHash(ctx, auth.HashCredential(bearer))
		if err != nil {
			return Target{}, nil, ErrCredentialDenied
		}
		row, err := q.GetTokenByID(ctx, pgvalue.UUID(tokenID))
		if err != nil {
			return Target{}, nil, ErrCredentialDenied
		}
		if publicAccess.TokenID != row.ID {
			return Target{}, nil, ErrCredentialDenied
		}
		credentials, err := t.key.Derive(pgvalue.MustUUIDValue(row.ID))
		if err != nil || !hmac.Equal(credentials.PublicAccessHash, publicAccess.TokenHash) ||
			!hmac.Equal(credentials.PublicAccessHash, auth.HashCredential(bearer)) {
			return Target{}, nil, ErrCredentialDenied
		}
		return TargetOf(row), &publicAccess, nil
	})
}

func (t *Tokens) complete(
	ctx context.Context,
	target Target,
	rawResult json.RawMessage,
	idempotencyKey string,
	authorize completionAuthorizer,
) (db.Token, error) {
	canonical, err := jsoncanon.Transform(rawResult)
	if err != nil {
		return db.Token{}, InputError{message: "result must be unambiguous JSON"}
	}
	fingerprint := sha256.Sum256(canonical)
	var completed db.Token
	var expired bool
	err = db.RunTx(ctx, t.txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		var publicAccess *db.PublicAccessToken
		if authorize != nil {
			var err error
			target, publicAccess, err = authorize(ctx, q)
			if err != nil {
				return err
			}
		}
		var claim *db.IdempotencyClaim
		var claims *idempotency.Transaction
		if idempotencyKey != "" {
			claimRequest, err := idempotency.NewTokenCompleteRequest(
				pgvalue.MustUUIDValue(target.EnvironmentID), pgvalue.MustUUIDValue(target.ID), idempotencyKey, canonical,
			)
			if err != nil {
				return err
			}
			claims, err = idempotency.TransactionFor(tx)
			if err != nil {
				return err
			}
			acquired, err := claims.Acquire(ctx, claimRequest)
			if err != nil {
				return err
			}
			if replay, done, err := replayOperation(ctx, q, acquired.Claim, target, "completed"); done {
				completed = replay
				return err
			}
			claim = &acquired.Claim
		}
		row, err := q.CompleteToken(ctx, db.CompleteTokenParams{
			CompletionFingerprint: fingerprint[:],
			OrgID:                 target.OrgID, ProjectID: target.ProjectID,
			EnvironmentID: target.EnvironmentID, ID: target.ID,
			Result:          canonical,
			ControlOutboxID: pgvalue.UUID(uuid.NewV7()),
		})
		if err != nil {
			return err
		}
		switch {
		case row.CompletionConflict:
			return ErrCompletionConflict
		case row.CompletionExpired:
			completed = tokenFromCompleteRow(row)
			expired = true
			if claim != nil {
				return failOperation(ctx, claims, *claim, target)
			}
			return nil
		case row.CompletionCancelled:
			return ErrCancelled
		}
		completed = tokenFromCompleteRow(row)
		if publicAccess != nil && row.ReconciliationEnqueued {
			if _, err := q.MarkPublicAccessTokenUsed(ctx, publicAccess.ID); err != nil {
				return ErrCredentialDenied
			}
		}
		if claim != nil {
			return completeOperation(ctx, claims, *claim, target, "completed")
		}
		return nil
	})
	if err == nil && expired {
		err = &ExpiredError{}
	}
	return completed, err
}

// Cancel cancels an authorized Token in one transaction: the optional
// idempotency claim, then the cancellation, which locks the Token and
// publishes its reconciliation intent. Cancelling again succeeds; a
// completed Token is ErrCompleted. An expired Token is *ExpiredError after
// the expiry and the claim's failed receipt commit.
func (t *Tokens) Cancel(ctx context.Context, target Target, idempotencyKey string) (db.Token, error) {
	var cancelled db.Token
	var expired bool
	err := db.RunTx(ctx, t.txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		var claim *db.IdempotencyClaim
		var claims *idempotency.Transaction
		if idempotencyKey != "" {
			claimRequest, err := idempotency.NewTokenCancelRequest(
				pgvalue.MustUUIDValue(target.EnvironmentID),
				pgvalue.MustUUIDValue(target.ID),
				idempotencyKey,
			)
			if err != nil {
				return err
			}
			claims, err = idempotency.TransactionFor(tx)
			if err != nil {
				return err
			}
			acquired, err := claims.Acquire(ctx, claimRequest)
			if err != nil {
				return err
			}
			if replay, done, err := replayOperation(ctx, q, acquired.Claim, target, "cancelled"); done {
				cancelled = replay
				return err
			}
			claim = &acquired.Claim
		}
		row, err := q.CancelToken(ctx, db.CancelTokenParams{
			OrgID: target.OrgID, ProjectID: target.ProjectID,
			EnvironmentID: target.EnvironmentID, ID: target.ID,
			ControlOutboxID: pgvalue.UUID(uuid.NewV7()),
		})
		if err != nil {
			return err
		}
		switch {
		case row.CancellationExpired:
			cancelled = tokenFromCancelRow(row)
			expired = true
			if claim != nil {
				return failOperation(ctx, claims, *claim, target)
			}
			return nil
		case row.CancellationCompleted:
			return ErrCompleted
		}
		cancelled = tokenFromCancelRow(row)
		if claim != nil {
			return completeOperation(ctx, claims, *claim, target, "cancelled")
		}
		return nil
	})
	if err == nil && expired {
		err = &ExpiredError{}
	}
	return cancelled, err
}

// replayOperation resolves a claim that already holds a receipt. A completed
// receipt for outcome replays the Token as it is now; a failed expired
// receipt is *ExpiredError, returned inside the transaction. done is false
// for a pending claim the operation goes on to execute.
func replayOperation(ctx context.Context, q *db.Queries, claim db.IdempotencyClaim, target Target, outcome string) (db.Token, bool, error) {
	switch claim.Status {
	case "completed":
		replayed, err := operationReceiptFromJSON(claim.Receipt)
		if err != nil || replayed.TokenID != pgvalue.UUIDString(target.ID) || replayed.Outcome != outcome {
			return db.Token{}, true, ErrReceiptInvalid
		}
		row, err := q.GetTokenByID(ctx, target.ID)
		return row, true, err
	case "failed":
		replayed, err := operationReceiptFromJSON(claim.Receipt)
		if err != nil || replayed.TokenID != pgvalue.UUIDString(target.ID) || replayed.Outcome != "expired" {
			return db.Token{}, true, ErrReceiptInvalid
		}
		return db.Token{}, true, &ExpiredError{}
	case "pending":
		return db.Token{}, false, nil
	default:
		return db.Token{}, true, ErrReceiptInvalid
	}
}

func completeOperation(ctx context.Context, claims *idempotency.Transaction, claim db.IdempotencyClaim, target Target, outcome string) error {
	receipt, err := json.Marshal(operationReceipt{TokenID: pgvalue.UUIDString(target.ID), Outcome: outcome})
	if err != nil {
		return err
	}
	_, err = claims.Complete(ctx, claim, receipt)
	return err
}

func failOperation(ctx context.Context, claims *idempotency.Transaction, claim db.IdempotencyClaim, target Target) error {
	receipt, err := json.Marshal(operationReceipt{TokenID: pgvalue.UUIDString(target.ID), Outcome: "expired"})
	if err != nil {
		return err
	}
	_, err = claims.Fail(ctx, claim, receipt)
	return err
}

func operationReceiptFromJSON(raw []byte) (operationReceipt, error) {
	var receipt operationReceipt
	if err := decodeReceipt(raw, &receipt); err != nil ||
		ids.Validate(receipt.TokenID) != nil ||
		(receipt.Outcome != "completed" &&
			receipt.Outcome != "cancelled" &&
			receipt.Outcome != "expired") {
		return operationReceipt{}, ErrReceiptInvalid
	}
	return receipt, nil
}

func tokenFromCompleteRow(row db.CompleteTokenRow) db.Token {
	return db.Token{
		ID: row.ID, OrgID: row.OrgID, ProjectID: row.ProjectID,
		EnvironmentID: row.EnvironmentID, Status: row.Status, ExpiresAt: row.ExpiresAt,
		CallbackSecretFingerprint: row.CallbackSecretFingerprint,
		CompletionFingerprint:     row.CompletionFingerprint,
		Result:                    row.Result, Error: row.Error, Metadata: row.Metadata, Tags: row.Tags,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, CompletedAt: row.CompletedAt,
		ExpiredAt: row.ExpiredAt, CancelledAt: row.CancelledAt,
	}
}

func tokenFromCancelRow(row db.CancelTokenRow) db.Token {
	return db.Token{
		ID: row.ID, OrgID: row.OrgID, ProjectID: row.ProjectID,
		EnvironmentID: row.EnvironmentID, Status: row.Status, ExpiresAt: row.ExpiresAt,
		CallbackSecretFingerprint: row.CallbackSecretFingerprint,
		CompletionFingerprint:     row.CompletionFingerprint,
		Result:                    row.Result, Error: row.Error, Metadata: row.Metadata, Tags: row.Tags,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, CompletedAt: row.CompletedAt,
		ExpiredAt: row.ExpiredAt, CancelledAt: row.CancelledAt,
	}
}
