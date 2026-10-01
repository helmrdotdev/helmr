package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"slices"
	"strings"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	// ErrNotFound reports a Token the scope does not have.
	ErrNotFound = errors.New("token was not found")
	// ErrCompletionConflict reports a completion whose result differs from
	// the Token's existing result.
	ErrCompletionConflict = errors.New("token completion conflicts with the existing result")
	// ErrCancelled reports a completion of a cancelled Token.
	ErrCancelled = errors.New("token was cancelled")
	// ErrCompleted reports a cancellation of a completed Token.
	ErrCompleted = errors.New("token is already completed")
	// ErrCreateAuthority reports a runtime creation whose source Run lease is
	// not live.
	ErrCreateAuthority = errors.New("token create source authority is stale")
	// ErrCredentialDenied reports a callback secret or public access token
	// that does not address the Token.
	ErrCredentialDenied = errors.New("token credential is invalid")
	// ErrReceiptInvalid reports a stored idempotency receipt that does not
	// describe the operation replaying it.
	ErrReceiptInvalid = errors.New("token operation receipt is invalid")
)

// ExpiredError reports that the Token had expired. A completion or
// cancellation that found the Token expired commits that outcome, and the
// failed receipt of its idempotency key, before returning it; replaying such
// a key returns it too.
type ExpiredError struct{}

func (*ExpiredError) Error() string { return "token has expired" }

// InputError reports a request value the Token owner rejects.
type InputError struct {
	message string
}

func (e InputError) Error() string { return e.message }

// Tokens runs Token creation, completion and cancellation, each in its own
// transaction.
type Tokens struct {
	txb       db.TxBeginner
	key       auth.CredentialKey
	apiOrigin *url.URL
}

// New returns the Token owner. apiOrigin is the origin callback URLs are
// served from; a creation without it fails.
func New(txb db.TxBeginner, key auth.CredentialKey, apiOrigin *url.URL) (*Tokens, error) {
	if txb == nil {
		return nil, errors.New("token transaction database is required")
	}
	if !key.Valid() {
		return nil, errors.New("token credential key is required")
	}
	return &Tokens{txb: txb, key: key, apiOrigin: apiOrigin}, nil
}

// Created is a Token as it was created, with its credentials.
type Created struct {
	token             db.Token
	publicAccessToken string
	callbackURL       string
}

// Token is the created Token row.
func (c Created) Token() db.Token { return cloneToken(c.token) }

// PublicAccessToken is the Token's public access credential.
func (c Created) PublicAccessToken() string { return c.publicAccessToken }

// CallbackURL is the URL that completes the Token with its callback secret.
func (c Created) CallbackURL() string { return c.callbackURL }

func cloneToken(row db.Token) db.Token {
	row.CallbackSecretFingerprint = bytes.Clone(row.CallbackSecretFingerprint)
	row.CompletionFingerprint = bytes.Clone(row.CompletionFingerprint)
	row.Result = bytes.Clone(row.Result)
	row.Error = bytes.Clone(row.Error)
	row.Metadata = bytes.Clone(row.Metadata)
	row.Tags = slices.Clone(row.Tags)
	return row
}

// Reader reads Tokens and list pages.
type Reader interface {
	GetToken(context.Context, db.GetTokenParams) (db.Token, error)
	ListTokens(context.Context, db.ListTokensParams) ([]db.ListTokensRow, error)
}

// Scope is the organization, project and environment a Token operation
// addresses.
type Scope struct {
	OrgID, ProjectID, EnvironmentID pgtype.UUID
}

// Target is a Token within its scope.
type Target struct {
	Scope
	ID pgtype.UUID
}

// TargetOf addresses an authorized Token.
func TargetOf(row db.Token) Target {
	return Target{Scope: Scope{OrgID: row.OrgID, ProjectID: row.ProjectID, EnvironmentID: row.EnvironmentID}, ID: row.ID}
}

// Get reads one Token without a transaction. A Token outside the scope is
// ErrNotFound.
func Get(ctx context.Context, store Reader, scope Scope, tokenID pgtype.UUID) (db.Token, error) {
	row, err := store.GetToken(ctx, db.GetTokenParams{
		OrgID: scope.OrgID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, ID: tokenID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Token{}, ErrNotFound
	}
	return row, err
}

// ListQuery filters and pages a Token list. An invalid status matches every
// Token; HasAfter resumes after the Token it names.
type ListQuery struct {
	Status         pgtype.Text
	HasAfter       bool
	AfterCreatedAt pgtype.Timestamptz
	AfterID        pgtype.UUID
	Limit          int32
}

// List reads, without a transaction, up to query.Limit Tokens of the scope
// and whether more follow.
func List(ctx context.Context, store Reader, scope Scope, query ListQuery) ([]db.ListTokensRow, bool, error) {
	rows, err := store.ListTokens(ctx, db.ListTokensParams{
		OrgID: scope.OrgID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
		Status: query.Status, HasAfter: query.HasAfter, AfterCreatedAt: query.AfterCreatedAt,
		AfterID: query.AfterID, LimitCount: query.Limit + 1,
	})
	if err != nil {
		return nil, false, err
	}
	hasMore := len(rows) > int(query.Limit)
	if hasMore {
		rows = rows[:query.Limit]
	}
	return rows, hasMore, nil
}

// decodeReceipt decodes one stored receipt object, rejecting unknown members
// and trailing values.
func decodeReceipt(raw []byte, destination any) error {
	if len(raw) == 0 {
		return errors.New("value is required")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}
