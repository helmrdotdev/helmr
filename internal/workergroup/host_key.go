package workergroup

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// DefaultHostCredentialTTL is the lifetime of a worker host credential when the
// host authentication configuration does not set one.
const DefaultHostCredentialTTL = 15 * time.Minute

// HostAuthConfig holds the key that signs host credential challenges, the
// key that signs worker host credentials, and the host credential lifetime.
type HostAuthConfig struct {
	challengeKey []byte
	signingKey   []byte
	ttl          time.Duration
}

// NewHostAuthConfig validates the challenge key and the host credential
// signing key. A ttl that is not positive selects DefaultHostCredentialTTL.
func NewHostAuthConfig(challengeKey []byte, signingKey []byte, ttl time.Duration) (HostAuthConfig, error) {
	if err := auth.ValidateMACKey(challengeKey); err != nil {
		return HostAuthConfig{}, fmt.Errorf("worker host challenge key: %w", err)
	}
	if err := validateHostCredentialSigningKey(signingKey); err != nil {
		return HostAuthConfig{}, err
	}
	if ttl <= 0 {
		ttl = DefaultHostCredentialTTL
	}
	return HostAuthConfig{
		challengeKey: append([]byte(nil), challengeKey...),
		signingKey:   append([]byte(nil), signingKey...),
		ttl:          ttl,
	}, nil
}

// unheldPublicKey is a valid Ed25519 public key whose private key was
// discarded at startup.
var unheldPublicKey = func() []byte {
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		panic(err)
	}
	return public
}()

// HostChallengeTTL bounds how long a host may take to sign a challenge.
const HostChallengeTTL = time.Minute

// HostChallenge is an unpredictable, expiring challenge bound to one host and
// one worker service. The Control Plane keeps no record of it: its MAC proves
// the Control Plane issued it. Answering it twice inside its lifetime yields
// nothing an observer of the resulting credential would not already hold.
type HostChallenge struct {
	Value     string
	ExpiresIn time.Duration
}

type hostChallengePayload struct {
	HostID    string `json:"h"`
	ServiceID string `json:"s"`
	ExpiresAt int64  `json:"e"`
	Nonce     string `json:"n"`
}

// IssueHostChallenge returns a challenge the host answers by signing
// auth.HostChallengeMessage with its machine key. It reveals nothing about whether
// the host exists.
func IssueHostChallenge(cfg HostAuthConfig, rawHostID, rawServiceID string, now time.Time) (HostChallenge, error) {
	hostID, err := ids.Parse(rawHostID)
	if err != nil {
		return HostChallenge{}, invalidInput("worker_host_id must be a canonical UUIDv7")
	}
	serviceID, err := ids.Parse(rawServiceID)
	if err != nil {
		return HostChallenge{}, invalidInput("service_id must be a canonical UUIDv7")
	}
	nonce, err := auth.GenerateOpaque(16)
	if err != nil {
		return HostChallenge{}, err
	}
	payload, err := json.Marshal(hostChallengePayload{HostID: hostID.String(), ServiceID: serviceID.String(), ExpiresAt: now.Add(HostChallengeTTL).Unix(), Nonce: nonce})
	if err != nil {
		return HostChallenge{}, err
	}
	mac, err := auth.MAC(cfg.challengeKey, payload)
	if err != nil {
		return HostChallenge{}, err
	}
	return HostChallenge{
		Value:     base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac),
		ExpiresIn: HostChallengeTTL,
	}, nil
}

// verifyHostChallenge checks that the Control Plane issued the challenge for
// this host and service and that it has not expired.
func (cfg HostAuthConfig) verifyHostChallenge(challenge string, hostID, serviceID uuid.UUID, now time.Time) bool {
	encodedPayload, encodedMAC, ok := strings.Cut(challenge, ".")
	if !ok {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(encodedMAC)
	if err != nil {
		return false
	}
	want, err := auth.MAC(cfg.challengeKey, payload)
	if err != nil || !hmac.Equal(got, want) {
		return false
	}
	var decoded hostChallengePayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return false
	}
	return decoded.HostID == hostID.String() && decoded.ServiceID == serviceID.String() && now.Unix() < decoded.ExpiresAt
}

// HostCredentialRequest is a worker host's answer to a challenge, exchanged for
// a host credential. ServiceID identifies the worker service instance: a new
// service ID starts a new epoch, and repeating the current one keeps it.
type HostCredentialRequest struct {
	HostID    string
	ServiceID string
	Challenge string
	// Signature is the machine key's Ed25519 signature of
	// auth.HostChallengeMessage(Challenge).
	Signature []byte
}

// HostCredential is a signed, short-lived worker host credential bound to one
// worker epoch.
type HostCredential struct {
	Value     string
	ExpiresIn time.Duration
	Epoch     int64
	// returned is set when the exchange brought a lost host back, recording
	// the group's owner for the audit record.
	returned *returnedHost
}

type returnedHost struct {
	hostID, ownerOrgID uuid.UUID
}

// IssueHostCredential verifies a worker host's signed challenge against its
// machine key, advancing the host's epoch when the service ID changed, and
// signs a host credential carrying the host and group claim versions. The
// host key, host, group and pool rows
// are locked by the single authenticating statement. The host credential is
// issued at now() read after that statement returns, so waiting for its locks
// does not shorten the lifetime the host credential advertises.
func IssueHostCredential(ctx context.Context, txb db.TxBeginner, policy auditpolicy.Policy, cfg HostAuthConfig, exchange HostCredentialRequest, now func() time.Time) (HostCredential, error) {
	var result HostCredential
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var err error
		result, err = issueHostCredential(ctx, db.New(tx), cfg, exchange, now)
		if err != nil {
			return err
		}
		if returned := result.returned; returned != nil {
			if err := audit.Append(ctx, tx, policy, audit.Event{Evidence: audit.Internal(audit.ControlPlane), OrgID: returned.ownerOrgID, Action: "worker_host.returned", TargetKind: "worker_host", TargetID: returned.hostID}); err != nil {
				return err
			}
		}
		return dbnotify.Notify(ctx, tx, "dispatch", "capacity")
	})
	return result, err
}

func issueHostCredential(ctx context.Context, q db.Querier, cfg HostAuthConfig, exchange HostCredentialRequest, now func() time.Time) (HostCredential, error) {
	if exchange.HostID == "" {
		return HostCredential{}, invalidInput("worker_host_id is required")
	}
	hostID, err := ids.Parse(exchange.HostID)
	if err != nil {
		return HostCredential{}, invalidInput("worker_host_id must be a canonical UUIDv7")
	}
	serviceID, err := ids.Parse(exchange.ServiceID)
	if err != nil {
		return HostCredential{}, invalidInput("service_id must be a canonical UUIDv7")
	}
	if !cfg.verifyHostChallenge(exchange.Challenge, hostID, serviceID, now()) {
		return HostCredential{}, ErrUnauthenticated
	}
	key, err := q.GetActiveWorkerHostKey(ctx, pgvalue.UUID(hostID))
	missing := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !missing {
		return HostCredential{}, fmt.Errorf("read worker host %s key: %w", hostID, err)
	}
	if missing {
		// Verify against a key nobody holds, so a host without a key costs
		// the same as a wrong signature.
		key.PublicKey = unheldPublicKey
	}
	if !ed25519.Verify(ed25519.PublicKey(key.PublicKey), auth.HostChallengeMessage(exchange.Challenge), exchange.Signature) || missing {
		return HostCredential{}, ErrUnauthenticated
	}
	// A lost host that proves its key returns before the exchange starts its
	// new epoch. The new epoch's startup recovery reports and stops what the
	// machine still runs, which settles the earlier epochs' custody. A host
	// fenced between the two statements is returned on the second attempt,
	// so a verified key is not refused over that race.
	var returned *returnedHost
	var hostKey db.AuthenticateWorkerHostKeyRow
	for attempt := 0; ; attempt++ {
		back, err := q.ReturnLostWorkerHost(ctx, db.ReturnLostWorkerHostParams{
			HostKeyID: key.ID, WorkerHostID: pgvalue.UUID(hostID), RetiredServiceID: pgvalue.UUID(uuid.NewV7()),
		})
		switch {
		case err == nil:
			returned = &returnedHost{hostID: hostID}
			if back.OwnerOrgID.Valid {
				returned.ownerOrgID = uuid.UUID(back.OwnerOrgID.Bytes)
			}
		case db.IsUniqueViolation(err):
			// A replacement enrolled under the same resource while this
			// return waited for the group: the lost host stays lost.
			return HostCredential{}, ErrUnauthenticated
		case !errors.Is(err, pgx.ErrNoRows):
			return HostCredential{}, fmt.Errorf("return worker host %s: %w", hostID, err)
		}
		hostKey, err = q.AuthenticateWorkerHostKey(ctx, db.AuthenticateWorkerHostKeyParams{
			WorkerHostID: pgvalue.UUID(hostID),
			HostKeyID:    key.ID,
			ServiceID:    pgvalue.UUID(serviceID),
		})
		if errors.Is(err, pgx.ErrNoRows) && returned == nil && attempt == 0 {
			continue
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return HostCredential{}, ErrUnauthenticated
		}
		if err != nil {
			return HostCredential{}, fmt.Errorf("authenticate worker host %s key: %w", hostID, err)
		}
		break
	}
	hostKeyID, err := pgvalue.UUIDValue(hostKey.ID)
	if err != nil {
		return HostCredential{}, fmt.Errorf("worker host key id: %w", err)
	}
	issuedAt := now()
	if !hostKey.CurrentEpoch.Valid || hostKey.CurrentEpoch.Int64 <= 0 {
		return HostCredential{}, errors.New("worker epoch was not established")
	}
	claims, err := (hostCredentialAuthority{
		WorkerHostID:      pgvalue.MustUUIDValue(hostKey.WorkerHostID),
		HostKeyID:         hostKeyID,
		WorkerGroupID:     pgvalue.MustUUIDValue(hostKey.WorkerGroupID),
		ClaimVersion:      hostKey.ClaimVersion,
		GroupClaimVersion: hostKey.GroupClaimVersion,
		WorkerEpoch:       hostKey.CurrentEpoch.Int64,
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
	return HostCredential{Value: signed, ExpiresIn: cfg.ttl, Epoch: hostKey.CurrentEpoch.Int64, returned: returned}, nil
}

type exchangeInput struct {
	ServiceID uuid.UUID
}

// hostCredentialAuthority is loaded by the epoch-exchange transaction. It keeps
// identity and policy authority out of the supervisor request body.
type hostCredentialAuthority struct {
	WorkerGroupID     uuid.UUID
	WorkerHostID      uuid.UUID
	HostKeyID         uuid.UUID
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
	if authority.HostKeyID == uuid.Nil() {
		return hostCredentialClaims{}, errors.New("host_key_id is required")
	}
	if authority.WorkerEpoch <= 0 || authority.ClaimVersion <= 0 || authority.GroupClaimVersion <= 0 {
		return hostCredentialClaims{}, errors.New("worker epoch and claim versions must be positive")
	}

	return hostCredentialClaims{
		WorkerGroupID: authority.WorkerGroupID.String(), WorkerHostID: authority.WorkerHostID.String(),
		HostKeyID: authority.HostKeyID.String(), WorkerEpoch: authority.WorkerEpoch,
		ClaimVersion: authority.ClaimVersion, GroupClaimVersion: authority.GroupClaimVersion,
		IssuedAt: issuedAt, ExpiresAt: expiresAt,
	}, nil
}
