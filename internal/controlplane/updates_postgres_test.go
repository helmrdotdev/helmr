package controlplane

import (
	"bufio"
	"context"
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
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestConsoleUpdateStreamRequiresCurrentSessionAuthority(t *testing.T) {
	f := sessiontest.New(t)
	queries := db.New(f.Pool)
	cfg := completeServerConfig(t)
	cfg.PublicURL = &url.URL{Scheme: "http", Host: "127.0.0.1"}
	cfg.DB, cfg.TX, cfg.Auth = queries, f.Pool, identity.NewAPIKeyAuthenticator(queries)
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
	fixture := httpPostgresFixture{pool: f.Pool, queries: queries, handler: handler, keys: keys}
	token := fixture.session(t, f.User, org)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
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
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM org_members WHERE org_id=$1 AND user_id=$2`, org, f.User)
	if _, err := f.Pool.Exec(t.Context(), `SELECT pg_notify('helmr_changes',$1)`, "session:"+f.Session.String()); err != nil {
		t.Fatal(err)
	}
	for scanner.Scan() {
		if scanner.Text() == "event: change" {
			t.Fatal("revoked membership received data")
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal("stream did not close cleanly", err)
	}
	denied := request(true)
	denied.Body.Close()
	if denied.StatusCode == http.StatusOK {
		t.Fatal("revoked user reconnected")
	}
}
