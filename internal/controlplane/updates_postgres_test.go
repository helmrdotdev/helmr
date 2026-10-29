package controlplane

import (
	"bufio"
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestConsoleUpdateStreamRequiresCurrentSessionAuthority(t *testing.T) {
	for _, mode := range []string{"membership-with-notification", "source-without-notification", "logout-without-notification"} {
		t.Run(mode, func(t *testing.T) {
			f := sessiontest.New(t)
			queries := db.New(f.Pool)
			cfg := completeServerConfig(t)
			cfg.AppOrigin = &url.URL{Scheme: "http", Host: "localhost"}
			cfg.DB, cfg.TX, cfg.Auth = queries, f.Pool, identity.NewAPIKeyAuthenticator(f.Pool)
			handler, err := NewServer(cfg)
			if err != nil {
				t.Fatal(err)
			}
			keys, err := auth.NewKeys(cfg.AuthKey)
			if err != nil {
				t.Fatal(err)
			}
			var org uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT org_id FROM environments WHERE id=$1`, f.Environment).Scan(&org); err != nil {
				t.Fatal(err)
			}
			fixture := httpPostgresFixture{pool: f.Pool, queries: queries, handler: handler, keys: keys, browserOrganizations: map[string]uuid.UUID{}, operatorTokens: map[string]bool{}, origin: cfg.AppOrigin.String()}
			token := fixture.session(t, f.User, org)
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			ready, unsubscribe := cfg.Changes.Subscribe("ready")
			defer unsubscribe()
			listener := make(chan error, 1)
			go func() {
				listener <- cfg.Changes.Run(ctx, f.Pool.Config().ConnConfig, slog.New(slog.NewTextHandler(io.Discard, nil)))
			}()
			defer func() { cancel(); <-listener }()
			select {
			case <-ready:
			case <-ctx.Done():
				t.Fatal("listener did not start")
			}
			server := httptest.NewServer(handler)
			defer server.Close()
			path := server.URL + "/api/projects/project/environments/test/sessions/" + f.Session.String() + "/updates"
			request := func(cookie bool) *http.Response {
				t.Helper()
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
				if cookie {
					req.AddCookie(&http.Cookie{Name: sessionCookieName(req), Value: token})
					req.Header.Set("X-Helmr-Organization", org.String())
				}
				response, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				return response
			}
			unauthenticated := request(false)
			unauthenticated.Body.Close()
			if unauthenticated.StatusCode != http.StatusUnauthorized {
				t.Fatal("anonymous stream", unauthenticated.StatusCode)
			}
			response := request(true)
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				raw, _ := io.ReadAll(response.Body)
				t.Fatal(response.StatusCode, string(raw))
			}
			scanner := bufio.NewScanner(response.Body)
			if !scanner.Scan() || scanner.Text() != "event: change" {
				t.Fatal("initial durable refresh missing", scanner.Text(), scanner.Err())
			}
			// Rotation changes the bearer, not this connected stream's admitted session/source.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE login_sessions SET token_hash=sha256(uuid_send(id)) WHERE user_id=$1`, f.User)
			// A connected stream refreshes from a real operation's committed notification.
			if _, err := session.ControlSession(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.UserCaller(f.Principal(t)), session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "interrupt", IdempotencyKey: "stream-control"}); err != nil {
				t.Fatal(err)
			}
			refreshed := false
			for scanner.Scan() {
				if scanner.Text() == "event: change" {
					refreshed = true
					break
				}
			}
			if !refreshed {
				t.Fatalf("committed control did not refresh connected stream: %v", scanner.Err())
			}
			revokedAt := time.Now()
			if mode == "membership-with-notification" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET is_owner=false,removed_at=clock_timestamp() WHERE org_id=$1 AND user_id=$2`, org, f.User)
				if _, err := f.Pool.Exec(t.Context(), `SELECT pg_notify('helmr_changes',$1)`, "session:"+f.Session.String()); err != nil {
					t.Fatal(err)
				}
			} else if mode == "logout-without-notification" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE login_sessions SET revoked_at=clock_timestamp() WHERE user_id=$1`, f.User)
			} else {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE auth_identities SET revoked_at=clock_timestamp() WHERE id IN (SELECT identity_id FROM login_sessions WHERE user_id=$1)`, f.User)
			}
			for scanner.Scan() {
				if scanner.Text() == "event: change" {
					t.Fatal("revoked membership received data")
				}
			}
			if err := scanner.Err(); err != nil && !(mode != "membership-with-notification" && errors.Is(err, io.ErrUnexpectedEOF)) {
				t.Fatal("stream did not terminate", err)
			}
			// Idle authority loss can close the transport after the prior write
			// deadline. The boundary is no further data and a denied reconnect,
			// not a guaranteed graceful final chunk.
			elapsed := time.Since(revokedAt)
			t.Logf("%s stream closed after revocation in %s", mode, elapsed)
			if elapsed > 20*time.Second {
				t.Fatal("stream exceeded heartbeat plus scheduling allowance", elapsed)
			}
			denied := request(true)
			denied.Body.Close()
			if denied.StatusCode == http.StatusOK {
				t.Fatal("revoked user reconnected")
			}
		})
	}
}

func TestCapacityUpdateStreamUsesDedicatedAuthorityAndCommittedDemand(t *testing.T) {
	f := sessiontest.New(t)
	cfg := completeServerConfig(t)
	cfg.DB, cfg.TX = db.New(f.Pool), f.Pool
	cfg.CapacityToken = capacityTestToken()
	handler, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	ready, unsubscribe := cfg.Changes.Subscribe("ready")
	defer unsubscribe()
	joined := make(chan error, 1)
	go func() {
		joined <- cfg.Changes.Run(ctx, f.Pool.Config().ConnConfig, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	defer func() { cancel(); <-joined }()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("listener did not start")
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	for _, token := range []string{"", "hlmr_product_token", capacityTestToken()} {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/capacity/v1/updates", nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if token != capacityTestToken() {
			response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("invalid token status=%d", response.StatusCode)
			}
			continue
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("stream status=%d", response.StatusCode)
		}
		scanner := bufio.NewScanner(response.Body)
		readChange := func() {
			t.Helper()
			for scanner.Scan() {
				if scanner.Text() == "event: change" {
					return
				}
			}
			t.Fatalf("missing change: %v", scanner.Err())
		}
		readChange()
		if _, err := session.ControlSession(ctx, auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, session.UserCaller(f.Principal(t)), session.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "interrupt", IdempotencyKey: "capacity-stream-control"}); err != nil {
			t.Fatal(err)
		}
		readChange()
	}
}
