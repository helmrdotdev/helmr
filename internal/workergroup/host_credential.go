package workergroup

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Worker host credentials are HS256 JWTs issued by the control plane for
// the worker audience. The issuer, audience and claim names are wire
// contract.
const (
	HostCredentialIssuer         = "helmr-controlplane"
	HostCredentialAudience       = "helmr-worker"
	HostCredentialSigningKeySize = 32
)

var (
	errInvalidHostCredential           = errors.New("invalid worker host credential")
	errExpiredHostCredential           = errors.New("expired worker host credential")
	errInvalidHostCredentialSigningKey = errors.New("worker host credential signing key must be exactly 32 bytes")
)

// hostCredentialClaims are the verified claims of a worker host credential.
type hostCredentialClaims struct {
	WorkerGroupID     string
	WorkerHostID      string
	CredentialID      string
	WorkerEpoch       int64
	ClaimVersion      int64
	GroupClaimVersion int64
	IssuedAt          time.Time
	ExpiresAt         time.Time
}

// jwtClaims is the JSON encoding of hostCredentialClaims.
type jwtClaims struct {
	WorkerGroupID     string `json:"worker_group_id"`
	WorkerHostID      string `json:"worker_host_id"`
	CredentialID      string `json:"credential_id"`
	WorkerEpoch       int64  `json:"worker_epoch"`
	ClaimVersion      int64  `json:"claim_version"`
	GroupClaimVersion int64  `json:"group_claim_version"`
	jwt.RegisteredClaims
}

func signHostCredential(signingKey []byte, payload hostCredentialClaims) (string, error) {
	if err := validateHostCredentialSigningKey(signingKey); err != nil {
		return "", err
	}
	if err := validateHostCredentialClaims(payload); err != nil {
		return "", err
	}
	claims := jwtClaims{
		WorkerGroupID: payload.WorkerGroupID, WorkerHostID: payload.WorkerHostID,
		CredentialID: payload.CredentialID, WorkerEpoch: payload.WorkerEpoch,
		ClaimVersion: payload.ClaimVersion, GroupClaimVersion: payload.GroupClaimVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: HostCredentialIssuer, Subject: payload.WorkerHostID,
			Audience: jwt.ClaimStrings{HostCredentialAudience},
			IssuedAt: jwt.NewNumericDate(payload.IssuedAt), ExpiresAt: jwt.NewNumericDate(payload.ExpiresAt),
		},
	}
	unsigned := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	unsigned.Header["typ"] = "JWT"
	signed, err := unsigned.SignedString(signingKey)
	if err != nil {
		return "", fmt.Errorf("sign worker host credential: %w", err)
	}
	return signed, nil
}

func verifyHostCredential(signingKey []byte, rawCredential string, now time.Time) (hostCredentialClaims, error) {
	if err := validateHostCredentialSigningKey(signingKey); err != nil {
		return hostCredentialClaims{}, err
	}
	if now.IsZero() {
		return hostCredentialClaims{}, fmt.Errorf("%w: verification time is zero", errInvalidHostCredential)
	}
	if rawCredential == "" || strings.TrimSpace(rawCredential) != rawCredential {
		return hostCredentialClaims{}, fmt.Errorf("%w: credential is empty or non-canonical", errInvalidHostCredential)
	}

	var claims jwtClaims
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(HostCredentialIssuer), jwt.WithAudience(HostCredentialAudience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithStrictDecoding(),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	parsed, err := parser.ParseWithClaims(rawCredential, &claims, func(parsed *jwt.Token) (any, error) {
		if parsed.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("%w: unexpected signing method %s", errInvalidHostCredential, parsed.Method.Alg())
		}
		return signingKey, nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return hostCredentialClaims{}, fmt.Errorf("%w: %w", errExpiredHostCredential, err)
		}
		return hostCredentialClaims{}, fmt.Errorf("%w: %w", errInvalidHostCredential, err)
	}
	if parsed == nil || !parsed.Valid {
		return hostCredentialClaims{}, errInvalidHostCredential
	}
	if typ, ok := parsed.Header["typ"].(string); !ok || typ != "JWT" {
		return hostCredentialClaims{}, fmt.Errorf("%w: unexpected JWT type", errInvalidHostCredential)
	}

	payload := hostCredentialClaims{
		WorkerGroupID: claims.WorkerGroupID, WorkerHostID: claims.WorkerHostID,
		CredentialID: claims.CredentialID, WorkerEpoch: claims.WorkerEpoch,
		ClaimVersion: claims.ClaimVersion, GroupClaimVersion: claims.GroupClaimVersion,
	}
	if claims.IssuedAt != nil {
		payload.IssuedAt = claims.IssuedAt.Time.UTC()
	}
	if claims.ExpiresAt != nil {
		payload.ExpiresAt = claims.ExpiresAt.Time.UTC()
	}
	if err := validateHostCredentialClaims(payload); err != nil {
		return hostCredentialClaims{}, fmt.Errorf("%w: %w", errInvalidHostCredential, err)
	}
	if claims.Subject != payload.WorkerHostID {
		return hostCredentialClaims{}, fmt.Errorf("%w: subject does not match worker_host_id", errInvalidHostCredential)
	}
	if claims.Issuer != HostCredentialIssuer || len(claims.Audience) != 1 || claims.Audience[0] != HostCredentialAudience {
		return hostCredentialClaims{}, fmt.Errorf("%w: non-canonical issuer or audience", errInvalidHostCredential)
	}
	return payload, nil
}

func validateHostCredentialSigningKey(signingKey []byte) error {
	if len(signingKey) != HostCredentialSigningKeySize {
		return errInvalidHostCredentialSigningKey
	}
	return nil
}

func validateHostCredentialClaims(payload hostCredentialClaims) error {
	if payload.WorkerGroupID == "" || strings.TrimSpace(payload.WorkerGroupID) != payload.WorkerGroupID {
		return errors.New("worker_group_id must be nonempty and canonical")
	}
	if payload.WorkerHostID == "" || strings.TrimSpace(payload.WorkerHostID) != payload.WorkerHostID {
		return errors.New("worker_host_id must be nonempty and canonical")
	}
	if payload.CredentialID == "" || strings.TrimSpace(payload.CredentialID) != payload.CredentialID {
		return errors.New("credential_id must be nonempty and canonical")
	}
	if payload.WorkerEpoch <= 0 {
		return errors.New("worker_epoch must be positive")
	}
	if payload.ClaimVersion <= 0 {
		return errors.New("claim_version must be positive")
	}
	if payload.GroupClaimVersion <= 0 {
		return errors.New("group_claim_version must be positive")
	}
	if payload.IssuedAt.IsZero() {
		return errors.New("issued_at is zero")
	}
	if payload.ExpiresAt.IsZero() {
		return errors.New("expires_at is zero")
	}
	if !payload.ExpiresAt.After(payload.IssuedAt) {
		return errors.New("expires_at must be after issued_at")
	}
	return nil
}
