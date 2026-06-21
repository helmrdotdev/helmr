package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/workspaceop"
	"github.com/jackc/pgx/v5/pgtype"
)

type runIdempotency struct {
	key       pgtype.Text
	expiresAt pgtype.Timestamptz
}

func normalizeRunIdempotency(options api.CreateRunOptions) (runIdempotency, error) {
	rawKey := strings.TrimSpace(options.IdempotencyKey)
	if rawKey == "" {
		if strings.TrimSpace(options.IdempotencyKeyTTL) != "" {
			return runIdempotency{}, errors.New("idempotency_key is required when idempotency_key_ttl is set")
		}
		return runIdempotency{}, nil
	}
	if len(rawKey) > maxIdempotencyKeyLength {
		return runIdempotency{}, fmt.Errorf("idempotency_key must be at most %d characters", maxIdempotencyKeyLength)
	}

	key := canonicalIdempotencyKey(rawKey)
	ttl, err := parseIdempotencyKeyTTL(options.IdempotencyKeyTTL)
	if err != nil {
		return runIdempotency{}, err
	}
	if ttl <= 0 {
		return runIdempotency{}, errors.New("idempotency_key_ttl must be positive")
	}
	return runIdempotency{
		key: pgtype.Text{
			String: key,
			Valid:  true,
		},
		expiresAt: pgtype.Timestamptz{
			Time:  time.Now().Add(ttl),
			Valid: true,
		},
	}, nil
}

func canonicalIdempotencyKey(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}

func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	canonical, err := workspaceop.CanonicalJSON(raw)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(canonical), nil
}

func parseIdempotencyKeyTTL(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultIdempotencyKeyTTL, nil
	}
	return api.ParsePositiveDuration(raw, "idempotency_key_ttl")
}
