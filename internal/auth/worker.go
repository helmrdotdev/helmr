package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	WorkerProtocolVersion     = "helmr.worker.v0"
	WorkerRoleRun             = "run"
	WorkerRoleBuild           = "build"
	WorkerTokenIssuer         = "helmr-control-plane"
	WorkerTokenAudience       = "helmr-worker"
	minWorkerTokenSecretBytes = 32
)

var (
	ErrInvalidWorkerToken    = errors.New("invalid worker JWT")
	ErrExpiredWorkerToken    = errors.New("expired worker JWT")
	ErrWeakWorkerTokenSecret = errors.New("worker JWT signing secret must be at least 32 bytes")
)

type WorkerClaims struct {
	WorkerGroupID     string
	WorkerInstanceID  string
	CredentialID      string
	WorkerEpoch       int64
	ClaimVersion      int64
	GroupClaimVersion int64
	Roles             []string
	ProtocolVersion   string
	IssuedAt          time.Time
	ExpiresAt         time.Time
}

type workerJWTClaims struct {
	WorkerGroupID     string   `json:"worker_group_id"`
	WorkerInstanceID  string   `json:"worker_instance_id"`
	CredentialID      string   `json:"credential_id"`
	WorkerEpoch       int64    `json:"worker_epoch"`
	ClaimVersion      int64    `json:"claim_version"`
	GroupClaimVersion int64    `json:"group_claim_version"`
	Roles             []string `json:"roles"`
	ProtocolVersion   string   `json:"protocol_version"`
	jwt.RegisteredClaims
}

func IssueWorkerToken(secret []byte, payload WorkerClaims) (string, error) {
	if err := ValidateWorkerTokenSecret(secret); err != nil {
		return "", err
	}
	if err := validateWorkerClaims(payload); err != nil {
		return "", err
	}
	claims := workerJWTClaims{
		WorkerGroupID: payload.WorkerGroupID, WorkerInstanceID: payload.WorkerInstanceID,
		CredentialID: payload.CredentialID, WorkerEpoch: payload.WorkerEpoch,
		ClaimVersion: payload.ClaimVersion, GroupClaimVersion: payload.GroupClaimVersion,
		Roles: append([]string(nil), payload.Roles...), ProtocolVersion: payload.ProtocolVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: WorkerTokenIssuer, Subject: payload.WorkerInstanceID,
			Audience: jwt.ClaimStrings{WorkerTokenAudience},
			IssuedAt: jwt.NewNumericDate(payload.IssuedAt), ExpiresAt: jwt.NewNumericDate(payload.ExpiresAt),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["typ"] = "JWT"
	signed, err := token.SignedString(secret)
	if err != nil {
		return "", fmt.Errorf("sign worker JWT: %w", err)
	}
	return signed, nil
}

func VerifyWorkerToken(secret []byte, rawToken string, now time.Time) (WorkerClaims, error) {
	if err := ValidateWorkerTokenSecret(secret); err != nil {
		return WorkerClaims{}, err
	}
	if now.IsZero() {
		return WorkerClaims{}, fmt.Errorf("%w: verification time is zero", ErrInvalidWorkerToken)
	}
	if rawToken == "" || strings.TrimSpace(rawToken) != rawToken {
		return WorkerClaims{}, fmt.Errorf("%w: token is empty or non-canonical", ErrInvalidWorkerToken)
	}

	var claims workerJWTClaims
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(WorkerTokenIssuer), jwt.WithAudience(WorkerTokenAudience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithStrictDecoding(),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	token, err := parser.ParseWithClaims(rawToken, &claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("%w: unexpected signing method %s", ErrInvalidWorkerToken, token.Method.Alg())
		}
		return secret, nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return WorkerClaims{}, fmt.Errorf("%w: %w", ErrExpiredWorkerToken, err)
		}
		return WorkerClaims{}, fmt.Errorf("%w: %w", ErrInvalidWorkerToken, err)
	}
	if token == nil || !token.Valid {
		return WorkerClaims{}, ErrInvalidWorkerToken
	}
	if typ, ok := token.Header["typ"].(string); !ok || typ != "JWT" {
		return WorkerClaims{}, fmt.Errorf("%w: unexpected token type", ErrInvalidWorkerToken)
	}

	payload := WorkerClaims{
		WorkerGroupID: claims.WorkerGroupID, WorkerInstanceID: claims.WorkerInstanceID,
		CredentialID: claims.CredentialID, WorkerEpoch: claims.WorkerEpoch,
		ClaimVersion: claims.ClaimVersion, GroupClaimVersion: claims.GroupClaimVersion,
		Roles: append([]string(nil), claims.Roles...), ProtocolVersion: claims.ProtocolVersion,
	}
	if claims.IssuedAt != nil {
		payload.IssuedAt = claims.IssuedAt.Time.UTC()
	}
	if claims.ExpiresAt != nil {
		payload.ExpiresAt = claims.ExpiresAt.Time.UTC()
	}
	if err := validateWorkerClaims(payload); err != nil {
		return WorkerClaims{}, fmt.Errorf("%w: %w", ErrInvalidWorkerToken, err)
	}
	if claims.Subject != payload.WorkerInstanceID {
		return WorkerClaims{}, fmt.Errorf("%w: subject does not match worker_instance_id", ErrInvalidWorkerToken)
	}
	if claims.Issuer != WorkerTokenIssuer || len(claims.Audience) != 1 || claims.Audience[0] != WorkerTokenAudience {
		return WorkerClaims{}, fmt.Errorf("%w: non-canonical issuer or audience", ErrInvalidWorkerToken)
	}
	return payload, nil
}

func ValidateWorkerTokenSecret(secret []byte) error {
	if len(secret) < minWorkerTokenSecretBytes {
		return ErrWeakWorkerTokenSecret
	}
	return nil
}

func validateWorkerClaims(payload WorkerClaims) error {
	if payload.WorkerGroupID == "" || strings.TrimSpace(payload.WorkerGroupID) != payload.WorkerGroupID {
		return errors.New("worker_group_id must be nonempty and canonical")
	}
	if payload.WorkerInstanceID == "" || strings.TrimSpace(payload.WorkerInstanceID) != payload.WorkerInstanceID {
		return errors.New("worker_instance_id must be nonempty and canonical")
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
	if len(payload.Roles) == 0 {
		return errors.New("roles is empty")
	}
	previous := ""
	for _, role := range payload.Roles {
		if role != WorkerRoleRun && role != WorkerRoleBuild {
			return fmt.Errorf("unsupported worker role %q", role)
		}
		if role <= previous {
			return errors.New("roles must be sorted and unique")
		}
		previous = role
	}
	if payload.ProtocolVersion != WorkerProtocolVersion {
		return fmt.Errorf("protocol_version must be %q", WorkerProtocolVersion)
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
