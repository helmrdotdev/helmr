package controlplane

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestTokenCreateResponseRemainsOriginalPendingProjection(t *testing.T) {
	createdAt := time.Date(2026, time.July, 24, 12, 0, 0, 0, time.UTC)
	server := &Server{
		publicURL: &url.URL{Scheme: "https", Host: "console.example.test"},
		apiOrigin: &url.URL{Scheme: "https", Host: "api.example.test"},
	}
	credentials := auth.Credentials{
		CallbackSecret:    "callback-secret",
		PublicAccessToken: "hlmr_pub_secret",
	}
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

			response, err := server.tokenCreateResponse(row, credentials)
			if err != nil {
				t.Fatal(err)
			}
			if response.Status != "pending" ||
				response.Result != nil ||
				response.CompletedAt != nil ||
				!response.UpdatedAt.Equal(createdAt) ||
				response.PublicAccessToken != credentials.PublicAccessToken ||
				response.CallbackURL == "" {
				t.Fatalf("create response = %+v", response)
			}
			if got, want := response.CallbackURL, "https://api.example.test/api/token-callbacks/"+pgvalue.UUIDString(row.ID)+"/callback-secret"; got != want {
				t.Fatalf("callback URL = %q, want %q", got, want)
			}
		})
	}
}
