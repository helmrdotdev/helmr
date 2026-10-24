package controlplane

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestComputerExecutionHTTPUsesCurrentAuthorityAndConfirmedScope(t *testing.T) {
	f := sessiontest.New(t)
	queries := db.New(f.Pool)
	cfg := completeServerConfig(t)
	cfg.AppOrigin = &url.URL{Scheme: "https", Host: "console.example.test"}
	cfg.DB, cfg.TX, cfg.Auth = queries, f.Pool, identity.NewAPIKeyAuthenticator(f.Pool)
	handler, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := auth.NewKeys(cfg.AuthKey)
	if err != nil {
		t.Fatal(err)
	}
	hf := httpPostgresFixture{pool: f.Pool, queries: queries, handler: handler, keys: keys}
	var org uuid.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT org_id FROM environments WHERE id=$1`, f.Environment).Scan(&org); err != nil {
		t.Fatal(err)
	}
	token := hf.session(t, f.User, org)
	base := "/api/projects/project/environments/test/computers/" + f.Computer.String()
	call := func(method, path, body string, want int) []byte {
		t.Helper()
		r := hf.request(t, method, path, token, body)
		if r.Code != want {
			t.Fatalf("%s %s: %d %s", method, path, r.Code, r.Body)
		}
		return r.Body.Bytes()
	}
	var scope session.ExecutionScope
	if err = json.Unmarshal(call("GET", base+"/execution-scope", "", 200), &scope); err != nil || len(scope.Sessions) != 1 {
		t.Fatalf("scope: %+v %v", scope, err)
	}
	call("POST", base+"/hibernate", `{"idempotency_key":"sleep","expected_scope":"stale"}`, http.StatusConflict)
	request, _ := json.Marshal(map[string]string{"idempotency_key": "sleep", "expected_scope": scope.Digest})
	first := call("POST", base+"/hibernate", string(request), http.StatusAccepted)
	if again := call("POST", base+"/hibernate", string(request), http.StatusAccepted); string(first) != string(again) {
		t.Fatalf("duplicate operation changed: %s / %s", first, again)
	}
	read := call("GET", base, "", 200)
	if !strings.Contains(string(read), `"execution_control"`) || !strings.Contains(string(read), `"hibernate"`) {
		t.Fatalf("missing operation state: %s", read)
	}
	f.SetPermissions(t, "sessions.view")
	call("POST", base+"/hibernate", string(request), http.StatusForbidden)
	call("POST", base+"/wake", `{}`, http.StatusForbidden)
	call("GET", base+"/execution-scope", "", http.StatusForbidden)
	var turns int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE session_id=$1`, f.Session).Scan(&turns); err != nil || turns != 0 {
		t.Fatalf("lifecycle created Turn: %d %v", turns, err)
	}
}
