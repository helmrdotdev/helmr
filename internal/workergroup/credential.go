package workergroup

import (
	"context"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// DefaultTokenTTL is the lifetime of a worker host epoch token when the
// credential configuration does not set one.
const DefaultTokenTTL = 15 * time.Minute

// CredentialConfig holds the keys that hash worker host secrets and sign
// worker host epoch tokens, and the epoch token lifetime.
type CredentialConfig struct {
	hostSecretKey []byte
	signingKey    []byte
	ttl           time.Duration
}

// NewCredentialConfig validates the host secret hashing key and the epoch
// token signing key. A ttl that is not positive selects DefaultTokenTTL.
func NewCredentialConfig(hostSecretKey []byte, signingKey []byte, ttl time.Duration) (CredentialConfig, error) {
	if err := auth.ValidateMACKey(hostSecretKey); err != nil {
		return CredentialConfig{}, fmt.Errorf("worker host secret key: %w", err)
	}
	if err := validateTokenSigningKey(signingKey); err != nil {
		return CredentialConfig{}, err
	}
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	return CredentialConfig{
		hostSecretKey: append([]byte(nil), hostSecretKey...),
		signingKey:    append([]byte(nil), signingKey...),
		ttl:           ttl,
	}, nil
}

// CredentialExchange is a worker host's request to exchange its host secret
// for an epoch token. ServiceID identifies the worker service instance: a new
// service ID starts a new epoch, and repeating the current one keeps it.
type CredentialExchange struct {
	HostID    string
	Secret    string
	ServiceID string
}

// HostToken is a signed worker host epoch token.
type HostToken struct {
	Token     string
	ExpiresIn time.Duration
	Epoch     int64
}

// ExchangeCredential authenticates a worker host secret, advancing the host's
// epoch when the service ID changed, and signs an epoch token carrying the
// host and group claim versions. The credential, host, group and pool rows
// are locked by the single authenticating statement. The token is issued at
// now() read after that statement returns, so waiting for its locks does
// not shorten the lifetime the token advertises.
func ExchangeCredential(ctx context.Context, q db.Querier, cfg CredentialConfig, exchange CredentialExchange, now func() time.Time) (HostToken, error) {
	if exchange.HostID == "" {
		return HostToken{}, invalidInput("worker_host_id is required")
	}
	hostID, err := ids.Parse(exchange.HostID)
	if err != nil {
		return HostToken{}, invalidInput("worker_host_id must be a canonical UUIDv7")
	}
	secretHash, err := auth.HashToken(cfg.hostSecretKey, exchange.Secret)
	if err != nil {
		return HostToken{}, ErrUnauthenticated
	}
	serviceID, err := ids.Parse(exchange.ServiceID)
	if err != nil {
		return HostToken{}, invalidInput("service_id must be a canonical UUIDv7")
	}
	credential, err := q.AuthenticateWorkerHostCredential(ctx, db.AuthenticateWorkerHostCredentialParams{
		WorkerHostID: pgvalue.UUID(hostID),
		SecretHash:   secretHash,
		ServiceID:    pgvalue.UUID(serviceID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return HostToken{}, ErrUnauthenticated
	}
	if err != nil {
		return HostToken{}, fmt.Errorf("authenticate worker host %s credential: %w", hostID, err)
	}
	credentialID, err := pgvalue.UUIDValue(credential.ID)
	if err != nil {
		return HostToken{}, fmt.Errorf("worker host credential id: %w", err)
	}
	issuedAt := now()
	if !credential.CurrentEpoch.Valid || credential.CurrentEpoch.Int64 <= 0 {
		return HostToken{}, errors.New("worker epoch was not established")
	}
	claims, err := (tokenAuthority{
		WorkerHostID:      pgvalue.MustUUIDValue(credential.WorkerHostID),
		CredentialID:      credentialID,
		WorkerGroupID:     pgvalue.MustUUIDValue(credential.WorkerGroupID),
		ClaimVersion:      credential.ClaimVersion,
		GroupClaimVersion: credential.GroupClaimVersion,
		WorkerEpoch:       credential.CurrentEpoch.Int64,
	}).Claims(exchangeInput{
		ServiceID: serviceID,
	}, issuedAt, issuedAt.Add(cfg.ttl))
	if err != nil {
		return HostToken{}, fmt.Errorf("derive worker host %s token claims: %w", hostID, err)
	}
	signed, err := issueToken(cfg.signingKey, claims)
	if err != nil {
		return HostToken{}, fmt.Errorf("sign worker host %s token: %w", hostID, err)
	}
	return HostToken{Token: signed, ExpiresIn: cfg.ttl, Epoch: credential.CurrentEpoch.Int64}, nil
}

type exchangeInput struct {
	ServiceID uuid.UUID
}

// tokenAuthority is loaded by the epoch-exchange transaction. It keeps
// identity and policy authority out of the supervisor request body.
type tokenAuthority struct {
	WorkerGroupID     uuid.UUID
	WorkerHostID      uuid.UUID
	CredentialID      uuid.UUID
	WorkerEpoch       int64
	ClaimVersion      int64
	GroupClaimVersion int64
}

func (input exchangeInput) Validate() error {
	if input.ServiceID == uuid.Nil() {
		return errors.New("service_id is required")
	}
	return nil
}

// Claims validates the authority returned by the epoch-exchange transaction.
// ServiceID is deliberately not copied into the JWT: it is the idempotency key
// for the transaction that returned authority.WorkerEpoch.
func (authority tokenAuthority) Claims(input exchangeInput, issuedAt, expiresAt time.Time) (tokenClaims, error) {
	if err := input.Validate(); err != nil {
		return tokenClaims{}, err
	}
	if authority.WorkerGroupID == uuid.Nil() {
		return tokenClaims{}, errors.New("worker_group_id is required")
	}
	if authority.WorkerHostID == uuid.Nil() {
		return tokenClaims{}, errors.New("worker_host_id is required")
	}
	if authority.CredentialID == uuid.Nil() {
		return tokenClaims{}, errors.New("credential_id is required")
	}
	if authority.WorkerEpoch <= 0 || authority.ClaimVersion <= 0 || authority.GroupClaimVersion <= 0 {
		return tokenClaims{}, errors.New("worker epoch and claim versions must be positive")
	}

	return tokenClaims{
		WorkerGroupID: authority.WorkerGroupID.String(), WorkerHostID: authority.WorkerHostID.String(),
		CredentialID: authority.CredentialID.String(), WorkerEpoch: authority.WorkerEpoch,
		ClaimVersion: authority.ClaimVersion, GroupClaimVersion: authority.GroupClaimVersion,
		IssuedAt: issuedAt, ExpiresAt: expiresAt,
	}, nil
}
