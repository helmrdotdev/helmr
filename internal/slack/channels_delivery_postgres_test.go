package slack

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestChannelReadFailureDoesNotFailUnsentOpening(t *testing.T) {
	for _, kind := range []string{"transport", "http", "invalid-json", "rate-limit", "rate-limit-json", "credentials", "credential-race", "not-member", "auth-revoked"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET thread_ts=NULL WHERE id=$1`, f.thread)
			opening := f.opening(t)
			failedRead := true
			mutations := 0
			client := NewWebClient(credentialsFunc(func(_ context.Context, id uuid.UUID, revision int64) (string, error) {
				if id != f.installation {
					t.Fatal("wrong installation")
				}
				if failedRead && kind == "credentials" {
					return "", errors.New("credential refresh in progress")
				}
				return "token", nil
			}), roundTripFunc(func(r *http.Request) (*http.Response, error) {
				status, body := 200, `{"ok":true,"channel":{"id":"C1","name":"work","context_team_id":"team","is_channel":true,"is_member":true}}`
				headers := http.Header{}
				if r.Method == http.MethodPost {
					mutations++
					if failedRead {
						t.Fatal("unverified channel received content")
					}
					body = `{"ok":true,"channel":"C1","ts":"123.999"}`
				} else if failedRead {
					switch kind {
					case "transport":
						return nil, errors.New("read connection lost")
					case "http":
						status = 503
					case "invalid-json":
						body = `{`
					case "rate-limit":
						status = 429
						headers.Set("Retry-After", "17")
					case "rate-limit-json":
						body = `{"ok":false,"error":"ratelimited"}`
						headers.Set("Retry-After", "17")
					case "credential-race":
						dbtest.MustExec(t, r.Context(), f.Pool, `UPDATE slack_installations SET credential_revision=credential_revision+1 WHERE id=$1`, f.installation)
						body = `{"ok":false,"error":"invalid_auth"}`
					case "not-member":
						body = `{"ok":true,"channel":{"id":"C1","name":"work","context_team_id":"team","is_channel":true,"is_member":false}}`
					case "auth-revoked":
						body = `{"ok":false,"error":"token_revoked"}`
					}
				}
				return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			if sent, err := ReconcilePost(t.Context(), f.Pool, client, opening); err != nil || sent || mutations != 0 {
				t.Fatal(sent, mutations, err)
			}
			retryable := kind != "not-member" && kind != "auth-revoked"
			var pending, frozen, rootUnknown bool
			var attempt *uuid.UUID
			var payload []byte
			if err := f.Pool.QueryRow(t.Context(), `SELECT p.status='pending',p.inflight_attempt_id IS NOT NULL,t.thread_ts IS NULL,p.inflight_attempt_id,p.inflight_payload FROM slack_posts p JOIN slack_threads t ON t.id=p.thread_id WHERE p.id=$1`, opening).Scan(&pending, &frozen, &rootUnknown, &attempt, &payload); err != nil || pending != retryable || frozen != retryable || !rootUnknown {
				t.Fatal(pending, frozen, rootUnknown, err)
			}
			if !retryable {
				return
			}
			if strings.HasPrefix(kind, "rate-limit") {
				var paced bool
				if err := f.Pool.QueryRow(t.Context(), `SELECT delivery_next_at>clock_timestamp()+interval '15 seconds' FROM slack_installations WHERE id=$1`, f.installation).Scan(&paced); err != nil || !paced {
					t.Fatal("lost Retry-After", paced, err)
				}
			}
			failedRead = false
			f.due(t)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_posts SET next_attempt_at=clock_timestamp() WHERE id=$1`, opening)
			if sent, err := ReconcilePost(t.Context(), f.Pool, client, opening); err != nil || !sent || mutations != 1 {
				t.Fatal("read retry lost accepted opening", sent, mutations, err)
			}
			var bound bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT p.status='posted' AND t.thread_ts='123.999' AND (SELECT count(*) FROM slack_posts WHERE thread_id=t.id AND role='opening')=1 FROM slack_posts p JOIN slack_threads t ON t.id=p.thread_id WHERE p.id=$1`, opening).Scan(&bound); err != nil || !bound {
				t.Fatal(bound, err)
			}
		})
	}
}

func TestHumanMentionRejectsConfirmedChannelDenialWithoutExpiring(t *testing.T) {
	for _, kind := range []string{"not_in_channel", "archived", "unsupported", "transport", "rate_limited"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			f.link(t)
			id := f.reply(t, "channel-verification", false)
			client := NewWebClient(admissionCredentials{}, roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if kind == "transport" {
					return nil, errors.New("temporary transport failure")
				}
				status := 200
				body := `{"ok":false,"error":"not_in_channel"}`
				if kind == "archived" {
					body = `{"ok":false,"error":"is_archived"}`
				}
				if kind == "unsupported" {
					body = `{"ok":true,"channel":{"id":"C1","name":"shared","context_team_id":"team","is_channel":true,"is_member":true,"is_shared":true}}`
				}
				if kind == "rate_limited" {
					status = 429
					body = `{"ok":false,"error":"ratelimited"}`
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			err := admitMessage(t.Context(), f.Pool, nil, client, id)
			transient := kind == "transport" || kind == "rate_limited"
			if transient {
				if !errors.Is(err, ErrChannelVerification) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			var state string
			var code *string
			var turns int
			if err = f.Pool.QueryRow(t.Context(), `SELECT status,error,(SELECT count(*) FROM turns) FROM slack_requests WHERE id=$1`, id).Scan(&state, &code, &turns); err != nil {
				t.Fatal(err)
			}
			if transient {
				if state != "received" || code != nil {
					t.Fatal("transient read rejected gesture", state, code)
				}
			} else if state != "rejected" || code == nil || *code != "binding_unavailable" {
				t.Fatal("confirmed denial waited for expiry", state, code)
			}
			if turns != 0 {
				t.Fatal("denied channel admitted work", turns)
			}
		})
	}
}
