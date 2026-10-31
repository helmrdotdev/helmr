package controlplane

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/identity"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

// A Turn message carries its content in "message".
func TestTurnMessageHTTPBody(t *testing.T) {
	f := sessiontest.New(t)
	var orgID uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id FROM environments WHERE id=$1`, f.Environment).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
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
	httpFixture := httpPostgresFixture{pool: f.Pool, queries: queries, handler: handler, keys: keys, browserOrganizations: map[string]uuid.UUID{}, operatorTokens: map[string]bool{}, origin: cfg.AppOrigin.String()}
	token := httpFixture.session(t, f.User, orgID)
	queued, err := session.Enqueue(t.Context(), auditpolicy.Policy{RetentionSeconds: 3600}, f.Pool, nil, session.UserCaller(f.Principal(t)), session.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, IdempotencyKey: "queued", Input: json.RawMessage(`[{"type":"text","text":"1"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/projects/project/environments/test/sessions/" + f.Session.String() + "/turns/" + queued.TurnID.String() + "/messages"
	// Non-empty content reaches admission, which rejects a Turn that is not
	// running; empty or undecoded content would fail earlier with 400.
	response := httpFixture.request(t, http.MethodPost, path, token, `{"message":[{"type":"text","text":"steer"}],"idempotency_key":"message"}`)
	var body api.HTTPErrorResponse
	if response.Code != http.StatusConflict || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.Error.Code != "message_closed" {
		t.Fatalf("response = %d %s, want 409 message_closed", response.Code, response.Body.String())
	}
	var messages int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM turn_messages WHERE environment_id=$1`, f.Environment).Scan(&messages); err != nil || messages != 0 {
		t.Fatalf("messages admitted: %d %v", messages, err)
	}
}
