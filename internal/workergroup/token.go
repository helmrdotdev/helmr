package workergroup

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Worker host epoch tokens are HS256 JWTs issued by the control plane for
// the worker audience. The issuer, audience and claim names are wire
// contract.
const (
	TokenIssuer         = "helmr-controlplane"
	TokenAudience       = "helmr-worker"
	TokenSigningKeySize = 32
)

var (
	errInvalidToken           = errors.New("invalid worker JWT")
	errExpiredToken           = errors.New("expired worker JWT")
	errInvalidTokenSigningKey = errors.New("worker JWT signing key must be exactly 32 bytes")
)

// tokenClaims are the verified claims of a worker host epoch token.
type tokenClaims struct {
	WorkerGroupID     string
	WorkerHostID      string
	CredentialID      string
	WorkerEpoch       int64
	ClaimVersion      int64
	GroupClaimVersion int64
	IssuedAt          time.Time
	ExpiresAt         time.Time
}

// jwtClaims is the JSON encoding of tokenClaims.
type jwtClaims struct {
	WorkerGroupID     string `json:"worker_group_id"`
	WorkerHostID      string `json:"worker_host_id"`
	CredentialID      string `json:"credential_id"`
	WorkerEpoch       int64  `json:"worker_epoch"`
	ClaimVersion      int64  `json:"claim_version"`
	GroupClaimVersion int64  `json:"group_claim_version"`
	jwt.RegisteredClaims
}

func issueToken(signingKey []byte, payload tokenClaims) (string, error) {
	if err := validateTokenSigningKey(signingKey); err != nil {
		return "", err
	}
	if err := validateTokenClaims(payload); err != nil {
		return "", err
	}
	claims := jwtClaims{
		WorkerGroupID: payload.WorkerGroupID, WorkerHostID: payload.WorkerHostID,
		CredentialID: payload.CredentialID, WorkerEpoch: payload.WorkerEpoch,
		ClaimVersion: payload.ClaimVersion, GroupClaimVersion: payload.GroupClaimVersion,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: TokenIssuer, Subject: payload.WorkerHostID,
			Audience: jwt.ClaimStrings{TokenAudience},
			IssuedAt: jwt.NewNumericDate(payload.IssuedAt), ExpiresAt: jwt.NewNumericDate(payload.ExpiresAt),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["typ"] = "JWT"
	signed, err := token.SignedString(signingKey)
	if err != nil {
		return "", fmt.Errorf("sign worker JWT: %w", err)
	}
	return signed, nil
}

func verifyToken(signingKey []byte, rawToken string, now time.Time) (tokenClaims, error) {
	if err := validateTokenSigningKey(signingKey); err != nil {
		return tokenClaims{}, err
	}
	if now.IsZero() {
		return tokenClaims{}, fmt.Errorf("%w: verification time is zero", errInvalidToken)
	}
	if rawToken == "" || strings.TrimSpace(rawToken) != rawToken {
		return tokenClaims{}, fmt.Errorf("%w: token is empty or non-canonical", errInvalidToken)
	}

	var claims jwtClaims
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(TokenIssuer), jwt.WithAudience(TokenAudience),
		jwt.WithExpirationRequired(), jwt.WithIssuedAt(), jwt.WithStrictDecoding(),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	token, err := parser.ParseWithClaims(rawToken, &claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("%w: unexpected signing method %s", errInvalidToken, token.Method.Alg())
		}
		return signingKey, nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return tokenClaims{}, fmt.Errorf("%w: %w", errExpiredToken, err)
		}
		return tokenClaims{}, fmt.Errorf("%w: %w", errInvalidToken, err)
	}
	if token == nil || !token.Valid {
		return tokenClaims{}, errInvalidToken
	}
	if typ, ok := token.Header["typ"].(string); !ok || typ != "JWT" {
		return tokenClaims{}, fmt.Errorf("%w: unexpected token type", errInvalidToken)
	}

	payload := tokenClaims{
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
	if err := validateTokenClaims(payload); err != nil {
		return tokenClaims{}, fmt.Errorf("%w: %w", errInvalidToken, err)
	}
	if claims.Subject != payload.WorkerHostID {
		return tokenClaims{}, fmt.Errorf("%w: subject does not match worker_host_id", errInvalidToken)
	}
	if claims.Issuer != TokenIssuer || len(claims.Audience) != 1 || claims.Audience[0] != TokenAudience {
		return tokenClaims{}, fmt.Errorf("%w: non-canonical issuer or audience", errInvalidToken)
	}
	return payload, nil
}

func validateTokenSigningKey(signingKey []byte) error {
	if len(signingKey) != TokenSigningKeySize {
		return errInvalidTokenSigningKey
	}
	return nil
}

func validateTokenClaims(payload tokenClaims) error {
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
