package slack

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"github.com/jackc/pgx/v5"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSlackDeliveryRunProgressesPeerWhileAnotherRequestIsInFlight(t *testing.T) {
	f := newStatusFixture(t)
	first := f.post(t, 1, "slow", "lifecycle", nil)
	peer := f
	peer.Session, peer.thread, peer.participant = uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth,origin_user_id,origin_api_key_id,origin_activation_id) SELECT environment_id,$2,agent_id,deployment_id,computer_id,$2,0,origin_user_id,origin_api_key_id,origin_activation_id FROM sessions WHERE id=$1;
 INSERT INTO slack_threads(id,environment_id,front_session_id,slack_channel_binding_id,org_id,team_id,slack_channel_id,thread_ts,opening_post_key) SELECT $3,$6,$2,$4,org_id,team_id,slack_channel_id,'456.789','peer-opening' FROM slack_channel_bindings WHERE id=$4;
 INSERT INTO slack_thread_sources(id,thread_id,environment_id,session_id) VALUES($5,$3,$6,$2);
 UPDATE slack_threads SET next_attempt_at=clock_timestamp()+interval '1 hour'`, pgx.QueryExecModeSimpleProtocol, f.Session, peer.Session, peer.thread, f.channel, peer.participant, f.Environment)
	entered := make(chan struct{}, 1)
	progressed := make(chan struct{}, 1)
	client := NewWebClient(credentialsFunc(func(context.Context, uuid.UUID, int64) (string, error) { return "fixture", nil }), roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/conversations.info" {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true,"channel":{"id":"C1","name":"work","context_team_id":"team","is_channel":true,"is_member":true}}`))}, nil
		}
		var payload struct {
			Metadata struct {
				Payload struct {
					ID uuid.UUID `json:"post_id"`
				} `json:"event_payload"`
			} `json:"metadata"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			return nil, err
		}
		if payload.Metadata.Payload.ID == first {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		select {
		case progressed <- struct{}{}:
		default:
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true,"ts":"456.790","channel":"C1"}`))}, nil
	}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunDelivery(ctx, f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, client, slog.New(slog.NewTextHandler(io.Discard, nil)), dbnotify.New())
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request not issued")
	}
	peer.post(t, 1, "peer", "lifecycle", nil)
	// Advance only the durable pacing gate; the first claim remains live.
	dbtest.MustExec(t, t.Context(), f.Pool, `WITH due AS (UPDATE slack_delivery_limits SET next_post_at=clock_timestamp(),next_status_at=clock_timestamp() WHERE installation_id=$1) UPDATE slack_method_limits SET retry_after=clock_timestamp() WHERE installation_id=$1`, f.installation)
	select {
	case <-progressed:
	case <-time.After(5 * time.Second):
		t.Fatal("live request blocked independent publication")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("wrong shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delivery workers not joined")
	}
}

func TestSlackCredentialRunJoinsReplacementPersistenceDuringShutdown(t *testing.T) {
	f, store, _ := credentialFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	oauth, err := NewOAuthClient("client", "secret", roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true,"token_type":"bot","access_token":"new-access","refresh_token":"new-refresh","expires_in":43200}`))}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- store.RunCredentials(ctx, oauth.http.Transport, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("refresh worker did not issue exchange")
	}
	cancel()
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("wrong shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh worker did not join")
	}
	if token, err := store.BotToken(t.Context(), f.installation, 2); err != nil || token != "new-access" {
		t.Fatal("shutdown discarded replacement", err)
	}
}
