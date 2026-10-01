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

// DefaultHostCredentialTTL is the lifetime of a worker host credential when the
// credential configuration does not set one.
const DefaultHostCredentialTTL = 15 * time.Minute

// CredentialConfig holds the keys that hash worker host secrets and sign
// worker host credentials, and the host credential lifetime.
type CredentialConfig struct {
	hostSecretKey []byte
	signingKey    []byte
	ttl           time.Duration
}

// NewCredentialConfig validates the host secret hashing key and the epoch
// host credential signing key. A ttl that is not positive selects DefaultHostCredentialTTL.
func NewCredentialConfig(hostSecretKey []byte, signingKey []byte, ttl time.Duration) (CredentialConfig, error) {
	if err := auth.ValidateMACKey(hostSecretKey); err != nil {
		return CredentialConfig{}, fmt.Errorf("worker host secret key: %w", err)
	}
	if err := validateHostCredentialSigningKey(signingKey); err != nil {
		return CredentialConfig{}, err
	}
	if ttl <= 0 {
		ttl = DefaultHostCredentialTTL
	}
	return CredentialConfig{
		hostSecretKey: append([]byte(nil), hostSecretKey...),
		signingKey:    append([]byte(nil), signingKey...),
		ttl:           ttl,
	}, nil
}

// HostCredentialRequest is a worker host's request to exchange its host secret
// for a host credential. ServiceID identifies the worker service instance: a new
// service ID starts a new epoch, and repeating the current one keeps it.
type HostCredentialRequest struct {
	HostID    string
	Secret    string
	ServiceID string
}

// HostCredential is a signed, short-lived worker host credential bound to one
// host epoch.
type HostCredential struct {
	Value     string
	ExpiresIn time.Duration
	Epoch     int64
}

// IssueHostCredential authenticates a worker host secret, advancing the host's
// epoch when the service ID changed, and signs a host credential carrying the
// host and group claim versions. The credential, host, group and pool rows
// are locked by the single authenticating statement. The credential is issued at
// now() read after that statement returns, so waiting for its locks does
// not shorten the lifetime the credential advertises.
func IssueHostCredential(ctx context.Context, q db.Querier, cfg CredentialConfig, exchange HostCredentialRequest, now func() time.Time) (HostCredential, error) {
	if exchange.HostID == "" {
		return HostCredential{}, invalidInput("worker_host_id is required")
	}
	hostID, err := ids.Parse(exchange.HostID)
	if err != nil {
		return HostCredential{}, invalidInput("worker_host_id must be a canonical UUIDv7")
	}
	secretHash, err := auth.HashToken(cfg.hostSecretKey, exchange.Secret)
	if err != nil {
		return HostCredential{}, ErrUnauthenticated
	}
	serviceID, err := ids.Parse(exchange.ServiceID)
	if err != nil {
		return HostCredential{}, invalidInput("service_id must be a canonical UUIDv7")
	}
	credential, err := q.AuthenticateWorkerHostCredential(ctx, db.AuthenticateWorkerHostCredentialParams{
		WorkerHostID: pgvalue.UUID(hostID),
		SecretHash:   secretHash,
		ServiceID:    pgvalue.UUID(serviceID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return HostCredential{}, ErrUnauthenticated
	}
	if err != nil {
		return HostCredential{}, fmt.Errorf("authenticate worker host %s credential: %w", hostID, err)
	}
	credentialID, err := pgvalue.UUIDValue(credential.ID)
	if err != nil {
		return HostCredential{}, fmt.Errorf("worker host credential id: %w", err)
	}
	issuedAt := now()
	if !credential.CurrentEpoch.Valid || credential.CurrentEpoch.Int64 <= 0 {
		return HostCredential{}, errors.New("worker epoch was not established")
	}
	claims, err := (hostCredentialAuthority{
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
		return HostCredential{}, fmt.Errorf("derive worker host %s credential claims: %w", hostID, err)
	}
	signed, err := signHostCredential(cfg.signingKey, claims)
	if err != nil {
		return HostCredential{}, fmt.Errorf("sign worker host %s credential: %w", hostID, err)
	}
	return HostCredential{Value: signed, ExpiresIn: cfg.ttl, Epoch: credential.CurrentEpoch.Int64}, nil
}

type exchangeInput struct {
	ServiceID uuid.UUID
}

// hostCredentialAuthority is loaded by the epoch-exchange transaction. It keeps
// identity and policy authority out of the supervisor request body.
type hostCredentialAuthority struct {
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
func (authority hostCredentialAuthority) Claims(input exchangeInput, issuedAt, expiresAt time.Time) (hostCredentialClaims, error) {
	if err := input.Validate(); err != nil {
		return hostCredentialClaims{}, err
	}
	if authority.WorkerGroupID == uuid.Nil() {
		return hostCredentialClaims{}, errors.New("worker_group_id is required")
	}
	if authority.WorkerHostID == uuid.Nil() {
		return hostCredentialClaims{}, errors.New("worker_host_id is required")
	}
	if authority.CredentialID == uuid.Nil() {
		return hostCredentialClaims{}, errors.New("credential_id is required")
	}
	if authority.WorkerEpoch <= 0 || authority.ClaimVersion <= 0 || authority.GroupClaimVersion <= 0 {
		return hostCredentialClaims{}, errors.New("worker epoch and claim versions must be positive")
	}

	return hostCredentialClaims{
		WorkerGroupID: authority.WorkerGroupID.String(), WorkerHostID: authority.WorkerHostID.String(),
		CredentialID: authority.CredentialID.String(), WorkerEpoch: authority.WorkerEpoch,
		ClaimVersion: authority.ClaimVersion, GroupClaimVersion: authority.GroupClaimVersion,
		IssuedAt: issuedAt, ExpiresAt: expiresAt,
	}, nil
}
