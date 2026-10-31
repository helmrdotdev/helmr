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
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

func TestComputerExecutionHTTPUsesCurrentAuthorityAndPreviewCount(t *testing.T) {
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
	hf := httpPostgresFixture{pool: f.Pool, queries: queries, handler: handler, keys: keys, browserOrganizations: map[string]uuid.UUID{}, operatorTokens: map[string]bool{}, origin: cfg.AppOrigin.String()}
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
	var preview struct {
		Sessions int64 `json:"sessions"`
	}
	if err = json.Unmarshal(call("GET", base+"/hibernate-preview", "", 200), &preview); err != nil || preview.Sessions != 1 {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	call("POST", base+"/hibernate", `{"idempotency_key":"sleep","expected_scope":"stale"}`, http.StatusBadRequest)
	request := `{"idempotency_key":"sleep"}`
	first := call("POST", base+"/hibernate", request, http.StatusAccepted)
	if again := call("POST", base+"/hibernate", request, http.StatusAccepted); string(first) != string(again) {
		t.Fatalf("duplicate operation changed: %s / %s", first, again)
	}
	read := call("GET", base, "", 200)
	if !strings.Contains(string(read), `"execution_control"`) || !strings.Contains(string(read), `"hibernate"`) {
		t.Fatalf("missing operation state: %s", read)
	}
	f.SetPermissions(t, "sessions.read")
	call("POST", base+"/hibernate", request, http.StatusForbidden)
	call("POST", base+"/wake", `{}`, http.StatusForbidden)
	call("GET", base+"/hibernate-preview", "", http.StatusForbidden)
	var turns int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE session_id=$1`, f.Session).Scan(&turns); err != nil || turns != 0 {
		t.Fatalf("lifecycle created Turn: %d %v", turns, err)
	}
}
