package api

import "time"

type CreateSecretRequest struct {
	Name           string `json:"name"`
	Value          string `json:"value"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type RotateSecretRequest struct {
	Value          string `json:"value"`
	IdempotencyKey string `json:"idempotency_key"`
}

type RevokeSecretRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
}

type SecretResponse struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Status    SecretStatus `json:"status"`
	CreatedAt time.Time    `json:"created_at"`
	RotatedAt *time.Time   `json:"rotated_at,omitempty"`
	RevokedAt *time.Time   `json:"revoked_at,omitempty"`
}

type SecretStatus string

const (
	SecretStatusActive  SecretStatus = "active"
	SecretStatusRevoked SecretStatus = "revoked"
)

type ListSecretsResponse struct {
	Secrets    []SecretResponse `json:"secrets"`
	NextCursor string           `json:"next_cursor,omitempty"`
}
