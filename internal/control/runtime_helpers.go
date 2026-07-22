package control

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const maxIdempotencyKeyLength = 512

func normalizedJSONObject(raw json.RawMessage, label string) ([]byte, error) {
	if len(raw) == 0 {
		return []byte(`{}`), nil
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("%s must be a JSON object: %w", label, err)
	}
	return json.Marshal(value)
}

func normalizeIdempotencyKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if len(key) > maxIdempotencyKeyLength {
		return "", fmt.Errorf("idempotency_key must be at most %d characters", maxIdempotencyKeyLength)
	}
	return key, nil
}

func parseRequiredWorkspaceUUID(field, raw string) (pgtype.UUID, error) {
	if strings.TrimSpace(raw) == "" {
		return pgtype.UUID{}, fmt.Errorf("%s is required", field)
	}
	return parseOptionalWorkspaceUUID(field, raw)
}

func parseOptionalWorkspaceUUID(field, raw string) (pgtype.UUID, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return pgtype.UUID{}, nil
	}
	id, err := uuid.Parse(trimmed)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%s must be a UUID", field)
	}
	return pgvalue.UUID(id), nil
}

func parseWorkspaceUUID(field, raw string) (pgtype.UUID, error) {
	return parseRequiredWorkspaceUUID(field, raw)
}

func (e terminalPayloadError) Error() string { return e.err.Error() }

func (e terminalPayloadError) Unwrap() error { return e.err }
