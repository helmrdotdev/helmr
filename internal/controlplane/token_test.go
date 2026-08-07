package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
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
		PublicAccessToken: "hlmr_pat_secret",
	}
	for _, state := range []db.TokenState{
		db.TokenStateCompleted,
		db.TokenStateCancelled,
		db.TokenStateExpired,
	} {
		t.Run(string(state), func(t *testing.T) {
			row := db.Token{
				ID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
				State:     state,
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

func TestExpiredTokenOperationCommitsTerminalTransition(t *testing.T) {
	tokenRow := db.Token{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ProjectID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
	}

	t.Run("complete", func(t *testing.T) {
		store := &expiredTokenOperationStore{token: tokenRow}
		server := &Server{db: store}
		if _, err := server.completeTokenRecord(
			t.Context(),
			tokenRow,
			[]byte(`{"approved":true}`),
			"",
			nil,
		); !errors.Is(err, errTokenExpired) {
			t.Fatalf("completion error = %v", err)
		}
		if store.completions != 1 || store.cancellations != 0 ||
			store.commits != 1 || store.rollbacks != 0 {
			t.Fatalf(
				"completion calls = complete %d cancel %d commit %d rollback %d",
				store.completions,
				store.cancellations,
				store.commits,
				store.rollbacks,
			)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		store := &expiredTokenOperationStore{token: tokenRow}
		server := &Server{db: store}
		if _, err := server.cancelTokenRecord(
			t.Context(),
			tokenRow,
			"",
		); !errors.Is(err, errTokenExpired) {
			t.Fatalf("cancellation error = %v", err)
		}
		if store.completions != 0 || store.cancellations != 1 ||
			store.commits != 1 || store.rollbacks != 0 {
			t.Fatalf(
				"cancellation calls = complete %d cancel %d commit %d rollback %d",
				store.completions,
				store.cancellations,
				store.commits,
				store.rollbacks,
			)
		}
	})
}

type expiredTokenOperationStore struct {
	db.Querier
	token         db.Token
	completions   int
	cancellations int
	commits       int
	rollbacks     int
}

func (s *expiredTokenOperationStore) BeginQuerier(
	context.Context,
) (db.Querier, transaction, error) {
	return s, expiredTokenOperationTransaction{store: s}, nil
}

func (s *expiredTokenOperationStore) CompleteToken(
	context.Context,
	db.CompleteTokenParams,
) (db.CompleteTokenRow, error) {
	s.completions++
	return db.CompleteTokenRow{
		ID:    s.token.ID,
		OrgID: s.token.OrgID, ProjectID: s.token.ProjectID,
		EnvironmentID:          s.token.EnvironmentID,
		State:                  db.TokenStateExpired,
		CompletionExpired:      true,
		ReconciliationEnqueued: true,
	}, nil
}

func (s *expiredTokenOperationStore) CancelToken(
	context.Context,
	db.CancelTokenParams,
) (db.CancelTokenRow, error) {
	s.cancellations++
	return db.CancelTokenRow{
		ID:    s.token.ID,
		OrgID: s.token.OrgID, ProjectID: s.token.ProjectID,
		EnvironmentID:          s.token.EnvironmentID,
		State:                  db.TokenStateExpired,
		CancellationExpired:    true,
		ReconciliationEnqueued: true,
	}, nil
}

type expiredTokenOperationTransaction struct {
	store *expiredTokenOperationStore
}

func (tx expiredTokenOperationTransaction) Commit(context.Context) error {
	tx.store.commits++
	return nil
}

func (tx expiredTokenOperationTransaction) Rollback(context.Context) error {
	tx.store.rollbacks++
	return nil
}
