package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
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
	OrgID        string
	WorkerID     string
	CredentialID string
	IssuedAt     time.Time
	ExpiresAt    time.Time
}

type workerJWTClaims struct {
	OrgID        string `json:"org_id"`
	WorkerID     string `json:"worker_id"`
	CredentialID string `json:"credential_id"`
	jwt.RegisteredClaims
}

func IssueWorkerToken(secret []byte, payload WorkerClaims) (string, error) {
	if err := validateWorkerTokenSecret(secret); err != nil {
		return "", err
	}
	if err := validateWorkerClaims(payload); err != nil {
		return "", err
	}
	claims := workerJWTClaims{
		OrgID:        strings.TrimSpace(payload.OrgID),
		WorkerID:     strings.TrimSpace(payload.WorkerID),
		CredentialID: strings.TrimSpace(payload.CredentialID),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    WorkerTokenIssuer,
			Subject:   strings.TrimSpace(payload.WorkerID),
			Audience:  jwt.ClaimStrings{WorkerTokenAudience},
			IssuedAt:  jwt.NewNumericDate(payload.IssuedAt),
			ExpiresAt: jwt.NewNumericDate(payload.ExpiresAt),
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
	if err := validateWorkerTokenSecret(secret); err != nil {
		return WorkerClaims{}, err
	}
	if now.IsZero() {
		return WorkerClaims{}, fmt.Errorf("%w: verification time is zero", ErrInvalidWorkerToken)
	}
	var claims workerJWTClaims
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(WorkerTokenIssuer),
		jwt.WithAudience(WorkerTokenAudience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	token, err := parser.ParseWithClaims(strings.TrimSpace(rawToken), &claims, func(token *jwt.Token) (any, error) {
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
	payload := WorkerClaims{
		OrgID:        strings.TrimSpace(claims.OrgID),
		WorkerID:     strings.TrimSpace(claims.WorkerID),
		CredentialID: strings.TrimSpace(claims.CredentialID),
	}
	if claims.IssuedAt != nil {
		payload.IssuedAt = claims.IssuedAt.Time
	}
	if claims.ExpiresAt != nil {
		payload.ExpiresAt = claims.ExpiresAt.Time
	}
	if err := validateWorkerClaims(payload); err != nil {
		return WorkerClaims{}, fmt.Errorf("%w: %w", ErrInvalidWorkerToken, err)
	}
	if claims.Subject != payload.WorkerID {
		return WorkerClaims{}, fmt.Errorf("%w: subject does not match worker_id", ErrInvalidWorkerToken)
	}
	return payload, nil
}

func validateWorkerTokenSecret(secret []byte) error {
	if len(secret) < minWorkerTokenSecretBytes {
		return ErrWeakWorkerTokenSecret
	}
	return nil
}

func ValidateWorkerTokenSecret(secret []byte) error {
	return validateWorkerTokenSecret(secret)
}

func validateWorkerClaims(payload WorkerClaims) error {
	if strings.TrimSpace(payload.OrgID) == "" {
		return errors.New("org_id is empty")
	}
	if strings.TrimSpace(payload.WorkerID) == "" {
		return errors.New("worker_id is empty")
	}
	if strings.TrimSpace(payload.CredentialID) == "" {
		return errors.New("credential_id is empty")
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
