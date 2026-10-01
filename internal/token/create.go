package token

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type createReceipt struct {
	TokenID string `json:"token_id"`
}

// CreateRequest is an external Token creation in an environment. An empty
// IdempotencyKey creates a new Token on every request.
type CreateRequest struct {
	Scope
	TimeoutMS      int64
	Metadata       json.RawMessage
	Tags           []string
	IdempotencyKey string
}

// RunCreateRequest is a Token creation by the Run executing under Fence. Its
// IdempotencyKey is required.
type RunCreateRequest struct {
	Fence          run.ExecutionFence
	TimeoutMS      int64
	Metadata       json.RawMessage
	Tags           []string
	IdempotencyKey string
}

type createInput struct {
	scope     Scope
	timeoutMS int64
	metadata  json.RawMessage
	tags      []string
	createdBy json.RawMessage
}

// Create creates an external Token in one transaction: the optional
// idempotency claim, then the Token and its public access credential. A
// completed claim replays the Token as it was created, with its credentials
// derived again; replayed reports it.
func (t *Tokens) Create(ctx context.Context, request CreateRequest) (created Created, replayed bool, err error) {
	err = db.RunTx(ctx, t.txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		var claim *db.IdempotencyClaim
		var claims *idempotency.Transaction
		if request.IdempotencyKey != "" {
			claimRequest, err := idempotency.NewExternalTokenCreateRequest(
				pgvalue.MustUUIDValue(request.EnvironmentID),
				request.IdempotencyKey,
				idempotency.TokenCreateFingerprint{
					TimeoutMS: &request.TimeoutMS,
					Metadata:  request.Metadata,
					Tags:      request.Tags,
				},
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
			if acquired.Claim.Status == "completed" {
				created, err = t.replayCreate(ctx, q, acquired.Claim.Receipt)
				replayed = true
				return err
			}
			if acquired.Claim.Status != "pending" {
				return ErrReceiptInvalid
			}
			claim = &acquired.Claim
		}
		var receipt json.RawMessage
		var err error
		created, receipt, err = t.create(ctx, q, createInput{
			scope: request.Scope, timeoutMS: request.TimeoutMS,
			metadata: request.Metadata, tags: request.Tags,
			createdBy: json.RawMessage(`{"kind":"external"}`),
		})
		if err != nil {
			return err
		}
		if claim != nil {
			if _, err := claims.Complete(ctx, *claim, receipt); err != nil {
				return err
			}
		}
		return nil
	})
	return created, replayed, err
}

// CreateForRun creates a Token for a live Run in one transaction. It reads
// the lease's location without locking, acquires the idempotency claim, then
// locks the live execution through the run owner and requires a live source
// before replaying or creating. A lease that is not live is
// ErrCreateAuthority; stale worker claims are returned as the run owner
// reports them.
func (t *Tokens) CreateForRun(ctx context.Context, request RunCreateRequest) (created Created, replayed bool, err error) {
	err = db.RunTx(ctx, t.txb, func(tx pgx.Tx) error {
		located, err := locateCreateSource(ctx, tx, request.Fence)
		if err != nil {
			return err
		}
		claimRequest, err := idempotency.NewRuntimeTokenCreateRequest(
			pgvalue.MustUUIDValue(located.EnvironmentID()),
			pgvalue.MustUUIDValue(located.RunID()),
			request.IdempotencyKey,
			idempotency.TokenCreateFingerprint{
				TimeoutMS: &request.TimeoutMS,
				Metadata:  request.Metadata,
				Tags:      request.Tags,
			},
		)
		if err != nil {
			return err
		}
		claims, err := idempotency.TransactionFor(tx)
		if err != nil {
			return err
		}
		acquired, err := claims.Acquire(ctx, claimRequest)
		if err != nil {
			return err
		}
		located, err = lockCreateSource(ctx, tx, request.Fence)
		if err != nil {
			return err
		}
		q := db.New(tx)
		if acquired.Claim.Status == "completed" {
			created, err = t.replayCreate(ctx, q, acquired.Claim.Receipt)
			replayed = true
			return err
		}
		if acquired.Claim.Status != "pending" {
			return ErrReceiptInvalid
		}
		var receipt json.RawMessage
		created, receipt, err = t.create(ctx, q, createInput{
			scope:     Scope{OrgID: located.OrgID(), ProjectID: located.ProjectID(), EnvironmentID: located.EnvironmentID()},
			timeoutMS: request.TimeoutMS, metadata: request.Metadata, tags: request.Tags,
			createdBy: json.RawMessage(`{"kind":"runtime"}`),
		})
		if err != nil {
			return err
		}
		if _, err := claims.Complete(ctx, acquired.Claim, receipt); err != nil {
			return err
		}
		return nil
	})
	return created, replayed, err
}

// locateCreateSource reads the fenced lease's location without locking. A
// lease the fence does not address is ErrCreateAuthority.
func locateCreateSource(ctx context.Context, tx pgx.Tx, fence run.ExecutionFence) (run.LiveLocator, error) {
	located, err := run.LocateLiveExecution(ctx, tx, fence)
	if errors.Is(err, pgx.ErrNoRows) {
		return run.LiveLocator{}, ErrCreateAuthority
	}
	return located, err
}

// lockCreateSource reads the fenced lease's location again, then locks the
// live execution and requires a live source.
func lockCreateSource(ctx context.Context, tx pgx.Tx, fence run.ExecutionFence) (run.LiveLocator, error) {
	located, err := locateCreateSource(ctx, tx, fence)
	if err != nil {
		return run.LiveLocator{}, err
	}
	_, err = run.LockLiveSource(ctx, tx, fence)
	if errors.Is(err, run.ErrStaleSource) {
		return run.LiveLocator{}, ErrCreateAuthority
	}
	if err != nil {
		return run.LiveLocator{}, err
	}
	return located, nil
}

func (t *Tokens) create(ctx context.Context, q db.Querier, input createInput) (Created, json.RawMessage, error) {
	now, err := q.GetTokenCreateTime(ctx)
	if err != nil || !now.Valid {
		return Created{}, nil, errors.New("load token create time")
	}
	expiresAt := pgvalue.Timestamptz(
		now.Time.Add(time.Duration(input.timeoutMS) * time.Millisecond),
	)
	tokenID := uuid.NewV7()
	credentials, err := t.key.Derive(tokenID)
	if err != nil {
		return Created{}, nil, err
	}
	tokenRow, err := q.CreateToken(ctx, db.CreateTokenParams{
		ID:    pgvalue.UUID(tokenID),
		OrgID: input.scope.OrgID, ProjectID: input.scope.ProjectID,
		EnvironmentID: input.scope.EnvironmentID, ExpiresAt: expiresAt,
		CallbackSecretFingerprint: credentials.CallbackFingerprint,
		Metadata:                  input.metadata, Tags: input.tags,
	})
	if err != nil {
		return Created{}, nil, fmt.Errorf("create token: %w", err)
	}
	_, err = q.CreatePublicAccessToken(ctx, db.CreatePublicAccessTokenParams{
		ID:        pgvalue.UUID(uuid.NewV7()),
		TokenID:   tokenRow.ID,
		TokenHash: credentials.PublicAccessHash,
		ExpiresAt: expiresAt, Metadata: []byte(`{}`),
		CreatedBy: input.createdBy,
	})
	if err != nil {
		return Created{}, nil, fmt.Errorf("create token public access credential: %w", err)
	}
	receipt, err := json.Marshal(createReceipt{TokenID: tokenID.String()})
	if err != nil {
		return Created{}, nil, err
	}
	created, err := t.created(tokenRow, credentials.PublicAccessToken, credentials.CallbackSecret)
	if err != nil {
		return Created{}, nil, err
	}
	return created, receipt, nil
}

// replayCreate reads the Token a completed creation receipt names and derives
// its credentials again. A receipt whose Token or credential no longer
// matches the derived credentials is ErrReceiptInvalid.
func (t *Tokens) replayCreate(ctx context.Context, q db.Querier, rawReceipt []byte) (Created, error) {
	var receipt createReceipt
	if err := decodeReceipt(rawReceipt, &receipt); err != nil {
		return Created{}, ErrReceiptInvalid
	}
	tokenID, err := ids.Parse(receipt.TokenID)
	if err != nil {
		return Created{}, ErrReceiptInvalid
	}
	tokenRow, err := q.GetTokenByID(ctx, pgvalue.UUID(tokenID))
	if err != nil {
		return Created{}, ErrReceiptInvalid
	}
	publicAccess, err := q.GetPublicAccessTokenForToken(ctx, pgvalue.UUID(tokenID))
	if err != nil {
		return Created{}, ErrReceiptInvalid
	}
	credentials, err := t.key.Derive(tokenID)
	if err != nil {
		return Created{}, err
	}
	if !hmac.Equal(credentials.CallbackFingerprint, tokenRow.CallbackSecretFingerprint) ||
		!hmac.Equal(credentials.PublicAccessHash, publicAccess.TokenHash) {
		return Created{}, ErrReceiptInvalid
	}
	return t.created(tokenRow, credentials.PublicAccessToken, credentials.CallbackSecret)
}

func (t *Tokens) created(row db.Token, publicAccessToken string, callbackSecret string) (Created, error) {
	if !row.ExpiresAt.Valid {
		return Created{}, errors.New("token expiry is unavailable")
	}
	callbackURL, err := t.callbackURL(row.ID, callbackSecret)
	if err != nil {
		return Created{}, err
	}
	return Created{token: row, publicAccessToken: publicAccessToken, callbackURL: callbackURL}, nil
}

// callbackURL is the API origin's callback path for the Token and secret.
func (t *Tokens) callbackURL(tokenID pgtype.UUID, callbackSecret string) (string, error) {
	if t.apiOrigin == nil {
		return "", errors.New("token callback origin is not configured")
	}
	return t.apiOrigin.ResolveReference(&url.URL{
		Path: "/api/token-callbacks/" + pgvalue.UUIDString(tokenID) + "/" + callbackSecret,
	}).String(), nil
}
