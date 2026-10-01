package workergroup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// DefaultHostCredentialTTL is the lifetime of a worker host credential when the
// host authentication configuration does not set one.
const DefaultHostCredentialTTL = 15 * time.Minute

// HostAuthConfig holds the keys that hash worker host secrets and sign
// worker host credentials, and the host credential lifetime.
type HostAuthConfig struct {
	hostSecretKey []byte
	signingKey    []byte
	ttl           time.Duration
}

// NewHostAuthConfig validates the host secret hashing key and the host
// credential signing key. A ttl that is not positive selects
// DefaultHostCredentialTTL.
func NewHostAuthConfig(hostSecretKey []byte, signingKey []byte, ttl time.Duration) (HostAuthConfig, error) {
	if err := auth.ValidateMACKey(hostSecretKey); err != nil {
		return HostAuthConfig{}, fmt.Errorf("worker host secret key: %w", err)
	}
	if err := validateHostCredentialSigningKey(signingKey); err != nil {
		return HostAuthConfig{}, err
	}
	if ttl <= 0 {
		ttl = DefaultHostCredentialTTL
	}
	return HostAuthConfig{
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
// worker epoch.
type HostCredential struct {
	Value     string
	ExpiresIn time.Duration
	Epoch     int64
}

// IssueHostCredential authenticates a worker host secret, advancing the host's
// epoch when the service ID changed, and signs a host credential carrying the
// host and group claim versions. The host secret, host, group and pool rows
// are locked by the single authenticating statement. The host credential is
// issued at now() read after that statement returns, so waiting for its locks
// does not shorten the lifetime the host credential advertises.
func IssueHostCredential(ctx context.Context, q db.Querier, cfg HostAuthConfig, exchange HostCredentialRequest, now func() time.Time) (HostCredential, error) {
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
	hostSecret, err := q.AuthenticateWorkerHostSecret(ctx, db.AuthenticateWorkerHostSecretParams{
		WorkerHostID: pgvalue.UUID(hostID),
		SecretHash:   secretHash,
		ServiceID:    pgvalue.UUID(serviceID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return HostCredential{}, ErrUnauthenticated
	}
	if err != nil {
		return HostCredential{}, fmt.Errorf("authenticate worker host %s secret: %w", hostID, err)
	}
	hostSecretID, err := pgvalue.UUIDValue(hostSecret.ID)
	if err != nil {
		return HostCredential{}, fmt.Errorf("worker host secret id: %w", err)
	}
	issuedAt := now()
	if !hostSecret.CurrentEpoch.Valid || hostSecret.CurrentEpoch.Int64 <= 0 {
		return HostCredential{}, errors.New("worker epoch was not established")
	}
	claims, err := (hostCredentialAuthority{
		WorkerHostID:      pgvalue.MustUUIDValue(hostSecret.WorkerHostID),
		HostSecretID:      hostSecretID,
		WorkerGroupID:     pgvalue.MustUUIDValue(hostSecret.WorkerGroupID),
		ClaimVersion:      hostSecret.ClaimVersion,
		GroupClaimVersion: hostSecret.GroupClaimVersion,
		WorkerEpoch:       hostSecret.CurrentEpoch.Int64,
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
	return HostCredential{Value: signed, ExpiresIn: cfg.ttl, Epoch: hostSecret.CurrentEpoch.Int64}, nil
}

type exchangeInput struct {
	ServiceID uuid.UUID
}

// hostCredentialAuthority is loaded by the epoch-exchange transaction. It keeps
// identity and policy authority out of the supervisor request body.
type hostCredentialAuthority struct {
	WorkerGroupID     uuid.UUID
	WorkerHostID      uuid.UUID
	HostSecretID      uuid.UUID
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
	if authority.HostSecretID == uuid.Nil() {
		return hostCredentialClaims{}, errors.New("host_secret_id is required")
	}
	if authority.WorkerEpoch <= 0 || authority.ClaimVersion <= 0 || authority.GroupClaimVersion <= 0 {
		return hostCredentialClaims{}, errors.New("worker epoch and claim versions must be positive")
	}

	return hostCredentialClaims{
		WorkerGroupID: authority.WorkerGroupID.String(), WorkerHostID: authority.WorkerHostID.String(),
		HostSecretID: authority.HostSecretID.String(), WorkerEpoch: authority.WorkerEpoch,
		ClaimVersion: authority.ClaimVersion, GroupClaimVersion: authority.GroupClaimVersion,
		IssuedAt: issuedAt, ExpiresAt: expiresAt,
	}, nil
}

const (
	hostSecretPrefix = "hlmr_wi_"
	hostSecretBytes  = 32
)

// generatedHostSecret is a new worker host secret: the raw value returned to
// the enrolling host once, its display prefix and its keyed hash.
type generatedHostSecret struct {
	Raw        string
	KeyPrefix  string
	SecretHash []byte
}

func generateHostSecret(hashSecret []byte) (generatedHostSecret, error) {
	raw, err := auth.GenerateOpaque(hostSecretBytes)
	if err != nil {
		return generatedHostSecret{}, err
	}
	secret := hostSecretPrefix + raw
	hash, err := auth.HashToken(hashSecret, secret)
	if err != nil {
		return generatedHostSecret{}, err
	}
	return generatedHostSecret{
		Raw:        secret,
		KeyPrefix:  hostSecretKeyPrefix(secret),
		SecretHash: hash,
	}, nil
}

func hostSecretKeyPrefix(key string) string {
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, hostSecretPrefix) || len(key) <= len(hostSecretPrefix)+8 {
		return key
	}
	return key[:len(hostSecretPrefix)+8]
}
