package controlplane

import (
	"encoding/json"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestTokenCreateResponseRemainsOriginalPendingProjection(t *testing.T) {
	createdAt := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	callbackURL := "https://api.example.test/api/token-callbacks/callback"
	for _, state := range []db.TokenStatus{
		db.TokenStatusCompleted,
		db.TokenStatusCancelled,
		db.TokenStatusExpired,
	} {
		t.Run(string(state), func(t *testing.T) {
			row := db.Token{
				ID:        pgvalue.UUID(uuid.NewV7()),
				Status:    state,
				Result:    json.RawMessage(`{"approved":true}`),
				Error:     json.RawMessage(`{"code":"terminal"}`),
				Metadata:  json.RawMessage(`{"review":true}`),
				Tags:      []string{"approval"},
				CreatedAt: pgvalue.Timestamptz(createdAt),
				UpdatedAt: pgvalue.Timestamptz(createdAt.Add(time.Minute)),
				ExpiresAt: pgvalue.Timestamptz(createdAt.Add(10 * time.Minute)),
				CompletedAt: pgvalue.Timestamptz(
					createdAt.Add(time.Minute),
				),
				ExpiredAt: pgvalue.Timestamptz(createdAt.Add(10 * time.Minute)),
				CancelledAt: pgvalue.Timestamptz(
					createdAt.Add(time.Minute),
				),
			}

			response, err := tokenCreateResponse(row, "hlmr_pub_secret", callbackURL)
			if err != nil {
				t.Fatal(err)
			}
			if response.Status != "pending" ||
				response.Result != nil ||
				response.CompletedAt != nil ||
				!response.UpdatedAt.Equal(createdAt) ||
				response.PublicAccessToken != "hlmr_pub_secret" ||
				response.CallbackURL != callbackURL {
				t.Fatalf("create response = %+v", response)
			}
		})
	}
}
