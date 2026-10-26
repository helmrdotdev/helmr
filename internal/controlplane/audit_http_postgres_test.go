package controlplane

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"net/http/httptest"
	"strings"
	"uuid"

	"net/http"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestAuditHTTPAuthenticationPermissionAndAdminRefusals(t *testing.T) {
	f := newHTTPPostgresFixture(t)
	org, owner := f.organizationOwner(t, "audit-http")
	user := f.user(t, "Reader")
	f.member(t, org, user, "member")
	token := f.session(t, user, org)
	var count int
	countFacts := func() int {
		t.Helper()
		if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	before := countFacts()
	if r := f.request(t, http.MethodGet, "/api/audit-events", "", ""); r.Code != http.StatusUnauthorized {
		t.Fatal(r.Code, r.Body.String())
	}
	if countFacts() != before {
		t.Fatal("missing credential created a security fact")
	}
	if r := f.request(t, http.MethodGet, "/api/audit-events", "malformed", ""); r.Code != http.StatusUnauthorized {
		t.Fatal(r.Code, r.Body.String())
	}
	if countFacts() != before+1 {
		t.Fatal("presented invalid credential not recorded")
	}
	var authentication bool
	if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM audit_events WHERE action='audit.searched' AND authentication_result='failed' AND actor_user_id IS NULL AND credential_id IS NULL AND authorization_evidence='{"decision":"not_evaluated","checks":[]}'::jsonb)`).Scan(&authentication); err != nil || !authentication {
		t.Fatal("authentication refusal invented authority", authentication, err)
	}
	if r := f.request(t, http.MethodPost, "/api/roles", token, `{"name":"forbidden","permissions":["audit_events.read"]}`); r.Code != http.StatusForbidden {
		t.Fatal(r.Code, r.Body.String())
	}
	var permission bool
	if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM audit_events WHERE action='role.created' AND outcome='denied' AND org_id=$1 AND actor_user_id=$2 AND authentication_result='verified' AND authorization_evidence->>'decision'='denied' AND authorization_evidence->'checks' @> '[{"type":"permission","permission":"roles.create","result":"failed","reason":"no_matching_grant"}]'::jsonb AND NOT (authorization_evidence->'checks'->0 ? 'evidence') AND details->>'reason'='permission_denied')`, org, user).Scan(&permission); err != nil || !permission {
		t.Fatal(permission, err)
	}
	if r := f.request(t, http.MethodGet, "/admin/api/v1/audit-events", owner, ""); r.Code != http.StatusForbidden {
		t.Fatal(r.Code, r.Body.String())
	}
	var admin bool
	if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM audit_events WHERE action='audit.searched' AND actor_user_id IS NOT NULL AND authorization_evidence->>'decision'='denied' AND authorization_evidence->'checks' @> '[{"type":"user_attribute","attribute":"admin","observed":false,"scope":{"kind":"installation"},"result":"failed"}]'::jsonb AND details->>'reason'='installation_authority_required')`).Scan(&admin); err != nil || !admin {
		t.Fatal(admin, err)
	}
	if r := f.request(t, http.MethodPost, "/admin/api/v1/worker-groups", owner, `{}`); r.Code != http.StatusForbidden {
		t.Fatal(r.Code, r.Body.String())
	}
	var platformScope bool
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(org_id IS NULL AND outcome='denied') FROM audit_events WHERE action='worker_group.created'`).Scan(&platformScope); err != nil || !platformScope {
		t.Fatal("platform refusal acquired organization custody", platformScope, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `ALTER TABLE audit_events ADD CONSTRAINT reject_denials CHECK(outcome<>'denied') NOT VALID`)
	if r := f.request(t, http.MethodGet, "/api/audit-events", "invalid", ""); r.Code != http.StatusServiceUnavailable {
		t.Fatal("ordinary denial hid capture failure", r.Code, r.Body.String())
	}
}

// Change the real credential immediately after transport authentication, before
// the handler and operation owner consume the admitted principal snapshot.
type changedAfterAuthentication struct {
	auth.Authenticator
	change func(auth.Principal)
}

func (a changedAfterAuthentication) Authenticate(ctx context.Context, token string) (auth.Principal, error) {
	p, err := a.Authenticator.Authenticate(ctx, token)
	if err == nil {
		a.change(p)
	}
	return p, err
}
func TestAuditHTTPTransactionCredentialRefusalsKeepAdmissionIdentity(t *testing.T) {
	for _, operation := range []string{"secret", "session"} {
		t.Run(operation, func(t *testing.T) {
			f := sessiontest.New(t)
			p := dbtest.SecretManager(t, f.Pool, f.Environment)
			dbtest.SetAPIKeyRole(t, f.Pool, p.APIKeyID, "secrets.create", "agents.use", "sessions.read")
			token := auth.APIKeyPrefix + uuid.NewV7().String()
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE api_keys SET token_hash=$2 WHERE id=$1`, p.APIKeyID, auth.HashAPIKey(token))
			store, err := secret.New(db.New(f.Pool), f.Pool, make([]byte, 32), auditpolicy.Policy{RetentionSeconds: 3600})
			if err != nil {
				t.Fatal(err)
			}
			handler := newPostgresServer(t, f.Pool, func(cfg *ServerConfig) {
				cfg.Secrets = store
				cfg.Auth = changedAfterAuthentication{Authenticator: cfg.Auth, change: func(admitted auth.Principal) {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE api_keys SET credential_version=credential_version+1 WHERE id=$1`, admitted.APIKeyID)
				}}
			})
			method, path, body, action := http.MethodPost, "/v1/secrets", `{"secret":"TOKEN","value":"private","idempotency_key":"create"}`, "secret.created"
			if operation == "session" {
				method, path, body, action = http.MethodGet, "/v1/sessions/"+f.Session.String(), "", "session.content_read"
			}
			request := httptest.NewRequest(method, path, strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatal(response.Code, response.Body.String())
			}
			var captured bool
			err = f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM audit_events WHERE action=$1 AND actor_api_key_id=$2 AND credential_id=$2 AND credential_version=1 AND authentication_result='verified' AND authorization_evidence->>'decision'='denied' AND authorization_evidence->'checks' @> '[{"type":"principal_currency","result":"failed","reason":"credential_version_changed","presented_version":1,"observed_version":2}]'::jsonb)`, action, p.APIKeyID).Scan(&captured)
			if err != nil || !captured {
				t.Fatal("HTTP refusal discarded observed credential comparison", captured, err)
			}
		})
	}
}

func TestAuditHTTPControlRefusalActionsAndTargets(t *testing.T) {
	f := sessiontest.New(t)
	p := dbtest.SecretManager(t, f.Pool, f.Environment)
	dbtest.SetAPIKeyRole(t, f.Pool, p.APIKeyID, "agents.read")
	token := auth.APIKeyPrefix + uuid.NewV7().String()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE api_keys SET token_hash=$2 WHERE id=$1`, p.APIKeyID, auth.HashAPIKey(token))
	handler := newPostgresServer(t, f.Pool)
	turnID := uuid.NewV7()
	for _, kind := range []string{"close", "cancel", "interrupt", "resume", "turn_interrupt"} {
		action, targetKind := "session."+kind+"_requested", "session"
		path := "/v1/sessions/" + f.Session.String() + "/" + kind
		if kind == "turn_interrupt" {
			action, targetKind = "turn.interrupt_requested", "turn"
			path = "/v1/sessions/" + f.Session.String() + "/turns/" + turnID.String() + "/interrupt"
		}
		body := `{"idempotency_key":"refused"}`
		if kind == "resume" {
			body = `{"idempotency_key":"refused","hold_id":"` + uuid.NewV7().String() + `"}`
		}
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatal(kind, response.Code, response.Body.String())
		}
		var valid bool
		if err := f.Pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(target_kind=$2 AND target_id IS NULL AND target_resolution='unresolved' AND outcome='denied') FROM audit_events WHERE action=$1 AND actor_api_key_id=$3`, action, targetKind, p.APIKeyID).Scan(&valid); err != nil || !valid {
			t.Fatal(kind, "refusal action/unresolved target", valid, err)
		}
	}
}

func TestAuditHTTPComputerRefusalsIdentifyOperation(t *testing.T) {
	f := sessiontest.New(t)
	p := dbtest.SecretManager(t, f.Pool, f.Environment)
	dbtest.SetAPIKeyRole(t, f.Pool, p.APIKeyID, "agents.read")
	token := auth.APIKeyPrefix + uuid.NewV7().String()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE api_keys SET token_hash=$2 WHERE id=$1`, p.APIKeyID, auth.HashAPIKey(token))
	handler := newPostgresServer(t, f.Pool)
	for _, kind := range []string{"wake", "hibernate"} {
		request := httptest.NewRequest(http.MethodPost, "/v1/computers/"+f.Computer.String()+"/"+kind, strings.NewReader(`{"idempotency_key":"refused"}`))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatal(kind, response.Code, response.Body.String())
		}
		var valid bool
		if err := f.Pool.QueryRow(t.Context(), `SELECT count(*)=1 AND bool_and(outcome='denied' AND target_kind='computer' AND details->>'reason'='permission_denied')
 FROM audit_events WHERE action=$1 AND actor_api_key_id=$2`, "computer."+kind+"_requested", p.APIKeyID).Scan(&valid); err != nil || !valid {
			t.Fatalf("%s refusal: %v %v", kind, valid, err)
		}
	}
}
