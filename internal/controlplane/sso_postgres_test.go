package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/sso"
	"github.com/helmrdotdev/helmr/internal/sso/ssotest"
	"github.com/jackc/pgx/v5"
)

// ssoBrowser carries cookies between requests the way a browser would, over
// HTTPS so the production cookie names and attributes apply.
type ssoBrowser struct {
	f       httpPostgresFixture
	cookies map[string]*http.Cookie
}

func (b *ssoBrowser) do(t *testing.T, method, path, body string, origin string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("Origin", origin)
	if strings.HasPrefix(body, "{") {
		request.Header.Set("Content-Type", "application/json")
	} else if body != "" {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	for _, c := range b.cookies {
		request.AddCookie(c)
	}
	recorder := httptest.NewRecorder()
	b.f.handler.ServeHTTP(recorder, request)
	for _, c := range recorder.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return recorder
}

// samlSignIn starts a SAML sign-in, has the IdP answer it and posts the answer
// cross-site to the ACS, returning the ACS response.
func (b *ssoBrowser) samlSignIn(t *testing.T, idp ssotest.IdP, connection uuid.UUID, subject string, start string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	return b.samlAnswer(t, idp, connection, subject, start, `{"organization":"acme"}`, nil)
}

// samlAnswer posts start with body, has the IdP answer the SAML request and
// posts the answer cross-site to the ACS.
func (b *ssoBrowser) samlAnswer(t *testing.T, idp ssotest.IdP, connection uuid.UUID, subject, start, body string, headers map[string]string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	console := b.f.origin
	started := b.do(t, http.MethodPost, start, body, console, headers)
	if started.Code != http.StatusOK {
		t.Fatalf("start = %d %s", started.Code, started.Body)
	}
	flow := b.cookies["__Host-helmr_saml_flow"]
	if flow == nil || flow.SameSite != http.SameSiteNoneMode || !flow.Secure {
		t.Fatalf("SAML flow cookie = %+v", flow)
	}
	payload, _, _ := strings.Cut(flow.Value, ".")
	decoded, _ := base64.RawURLEncoding.DecodeString(payload)
	var envelope browserAuthEnvelope
	if err := json.Unmarshal(decoded, &envelope); err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse(console)
	entityID, acsURL := sso.ServiceProviderURLs(origin, connection)
	now := time.Now().UTC()
	response := ssotest.Encode(t, idp.Response(t, ssotest.ResponseOptions{
		Sign: ssotest.SignAssertion, NameID: subject, NameIDFormat: "urn:oasis:names:tc:SAML:2.0:nameid-format:persistent",
		RequestID: envelope.Flow.SSORequestID, Audience: entityID, Recipient: acsURL,
		NotBefore: now.Add(-time.Minute), NotOnOrAfter: now.Add(5 * time.Minute),
		AuthnInstant: now.Add(-10 * time.Second).Format(time.RFC3339), AssertionID: "_a" + uuid.NewV7().String(),
		Attributes: map[string]string{"email": subject + "@example.test"},
	}))
	form := url.Values{"SAMLResponse": {response}}.Encode()
	return b.do(t, http.MethodPost, "/api/auth/sso/saml/"+connection.String()+"/acs", form, "https://idp.example.test", nil), form
}

func TestSAMLSignInThroughTheConsole(t *testing.T) {
	f := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		store, err := sso.NewStore(make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		cfg.SSO = store
	})
	orgID, _ := f.organizationOwner(t, "acme")
	var owner uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM org_members WHERE org_id=$1`, orgID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	idp := ssotest.NewIdP(t)
	connection := uuid.NewV7()
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO sso_connections(id,org_id,protocol,state,saml_idp_entity_id,saml_sso_url,saml_certificates,created_by_member_id,activated_at)
 VALUES($1,$2,'saml','active',$3,'https://idp.example.test/sso',$4,$5,clock_timestamp())`, connection, orgID, idp.EntityID, []string{idp.CertPEM}, owner); err != nil {
		t.Fatal(err)
	}
	alice := f.user(t, "Alice")
	if _, err := f.pool.Exec(t.Context(), `UPDATE users SET primary_email='alice@example.test' WHERE id=$1`, alice); err != nil {
		t.Fatal(err)
	}
	f.member(t, orgID, alice, "member")
	console := f.origin

	// First association: the IdP vouches for a subject Helmr has not seen.
	first := &ssoBrowser{f: f, cookies: map[string]*http.Cookie{}}
	acs, form := first.samlSignIn(t, idp, connection, "alice", "/api/auth/sso/start")
	if acs.Code != http.StatusSeeOther || acs.Header().Get("Location") != "/sso/callback" {
		t.Fatalf("ACS = %d %s", acs.Code, acs.Header().Get("Location"))
	}
	if finished := first.do(t, http.MethodPost, "/api/auth/sso/saml/finish", "", "https://idp.example.test", nil); finished.Code != http.StatusForbidden {
		t.Fatalf("cross-site completion = %d %s", finished.Code, finished.Body)
	}
	finished := first.do(t, http.MethodPost, "/api/auth/sso/saml/finish", "", console, nil)
	if finished.Code != http.StatusOK || !strings.Contains(finished.Body.String(), `"/sso/link"`) || first.cookies["__Host-helmr_sso_link"] == nil {
		t.Fatalf("finish = %d %s", finished.Code, finished.Body)
	}
	if replay := first.do(t, http.MethodPost, "/api/auth/sso/saml/"+connection.String()+"/acs", form, "https://idp.example.test", nil); !strings.Contains(replay.Header().Get("Location"), "sso_error=authentication_invalid") {
		t.Fatalf("replayed response without a flow = %s", replay.Header().Get("Location"))
	}
	link := first.do(t, http.MethodGet, "/api/auth/sso/link", "", console, nil)
	if link.Code != http.StatusOK || !strings.Contains(link.Body.String(), `"asserted_email":"alice@example.test"`) {
		t.Fatalf("link = %d %s", link.Code, link.Body)
	}
	if refused := first.do(t, http.MethodPost, "/api/auth/sso/link/confirm", "", console, nil); refused.Code != http.StatusForbidden {
		t.Fatalf("confirm without an ordinary sign-in = %d %s", refused.Code, refused.Body)
	}
	// The Console runs the email-code sign-in for the asserted address first.
	ordinary := f.session(t, alice, uuid.Nil())
	first.cookies["__Host-helmr_session"] = &http.Cookie{Name: "__Host-helmr_session", Value: ordinary}
	confirmed := first.do(t, http.MethodPost, "/api/auth/sso/link/confirm", "", console, nil)
	if confirmed.Code != http.StatusOK || !strings.Contains(confirmed.Body.String(), orgID.String()) {
		t.Fatalf("confirm = %d %s", confirmed.Code, confirmed.Body)
	}

	// A returning identity signs a signed-out browser in without an email step.
	returning := &ssoBrowser{f: f, cookies: map[string]*http.Cookie{}}
	if acs, _ := returning.samlSignIn(t, idp, connection, "alice", "/api/auth/sso/start"); acs.Header().Get("Location") != "/sso/callback" {
		t.Fatalf("ACS = %s", acs.Header().Get("Location"))
	}
	finished = returning.do(t, http.MethodPost, "/api/auth/sso/saml/finish", "", console, nil)
	if finished.Code != http.StatusOK || returning.cookies["__Host-helmr_session"] == nil {
		t.Fatalf("returning finish = %d %s", finished.Code, finished.Body)
	}
	selected := map[string]string{"X-Helmr-Organization": orgID.String()}
	if got := returning.do(t, http.MethodGet, "/api/organization", "", console, selected); got.Code != http.StatusOK {
		t.Fatalf("organization with an SSO proof = %d %s", got.Code, got.Body)
	}
	if got := returning.do(t, http.MethodGet, "/api/me", "", console, nil); got.Code != http.StatusOK {
		t.Fatalf("shell account read = %d %s", got.Code, got.Body)
	}
	if got := returning.do(t, http.MethodGet, "/api/account/credentials", "", console, nil); got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), "ordinary_sign_in_required") {
		t.Fatalf("account settings from an SSO-only session = %d %s", got.Code, got.Body)
	}
	bindings := first.do(t, http.MethodGet, "/api/account/sso/bindings", "", console, nil)
	if bindings.Code != http.StatusOK || !strings.Contains(bindings.Body.String(), `"subject":"alice"`) {
		t.Fatalf("bindings = %d %s", bindings.Code, bindings.Body)
	}

	stray := &ssoBrowser{f: f, cookies: map[string]*http.Cookie{}}
	var refusedBefore int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='identity.sign_in' AND outcome='denied'`).Scan(&refusedBefore); err != nil {
		t.Fatal(err)
	}
	if got := stray.do(t, http.MethodPost, "/api/auth/sso/saml/"+connection.String()+"/acs", form, "https://idp.example.test", nil); !strings.Contains(got.Header().Get("Location"), "sso_error=authentication_invalid") {
		t.Fatalf("unsolicited response = %s", got.Header().Get("Location"))
	}
	// A response that does not validate against this browser's own flow is a
	// refused sign-in; an unsolicited post above is not recorded.
	if got := stray.do(t, http.MethodPost, "/api/auth/sso/start", `{"organization":"acme"}`, console, nil); got.Code != http.StatusOK {
		t.Fatalf("start = %d %s", got.Code, got.Body)
	}
	if got := stray.do(t, http.MethodPost, "/api/auth/sso/saml/"+connection.String()+"/acs", form, "https://idp.example.test", nil); !strings.Contains(got.Header().Get("Location"), "sso_error=authentication_invalid") {
		t.Fatalf("response to another request = %s", got.Header().Get("Location"))
	}
	var refusedAfter int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action='identity.sign_in' AND outcome='denied'`).Scan(&refusedAfter); err != nil || refusedAfter != refusedBefore+1 {
		t.Fatalf("refused SAML responses audited = %d, want %d (%v)", refusedAfter, refusedBefore+1, err)
	}
	if got := stray.do(t, http.MethodPost, "/api/auth/sso/start", `{"organization":"unknown"}`, console, nil); got.Code != http.StatusNotFound || !strings.Contains(got.Body.String(), "sso_not_available") {
		t.Fatalf("unknown organization = %d %s", got.Code, got.Body)
	}
}

// newOIDCFixture serves an organization "acme" whose active connection is the
// local OIDC issuer.
func newOIDCFixture(t *testing.T) (httpPostgresFixture, *ssotest.OIDCIssuer, uuid.UUID) {
	t.Helper()
	issuer := ssotest.NewOIDCIssuer(t)
	store, err := sso.NewStore(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	f := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		cfg.SSO = store
		cfg.SSOHTTPClient = issuer.Server.Client()
	})
	orgID, _ := f.organizationOwner(t, "acme")
	var owner uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM org_members WHERE org_id=$1`, orgID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if err := db.RunTx(t.Context(), f.pool, func(tx pgx.Tx) error {
		c, err := store.CreateOIDC(t.Context(), tx, orgID, owner, sso.OIDCConfig{Issuer: issuer.Server.URL, ClientID: "helmr", ClientSecret: "secret"})
		if err != nil {
			return err
		}
		_, err = tx.Exec(t.Context(), `UPDATE sso_connections SET state='active',activated_at=clock_timestamp() WHERE id=$1`, c.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return f, issuer, orgID
}

func TestOIDCSelfLinkThroughTheConsole(t *testing.T) {
	f, issuer, orgID := newOIDCFixture(t)
	alice := f.user(t, "Alice")
	f.member(t, orgID, alice, "member")
	console := f.origin
	browser := &ssoBrowser{f: f, cookies: map[string]*http.Cookie{"__Host-helmr_session": {Name: "__Host-helmr_session", Value: f.session(t, alice, uuid.Nil())}}}

	forced := browser.do(t, http.MethodPost, "/api/account/sso/link", `{"organization_id":"`+orgID.String()+`","reauthenticate":true}`, console, nil)
	if q := oidcAnswer(t, issuer, forced.Body.Bytes(), "alice-oidc", time.Now()); q.Get("prompt") != "login" {
		t.Fatalf("forced self-link query = %v", q)
	}
	started := browser.do(t, http.MethodPost, "/api/account/sso/link", `{"organization_id":"`+orgID.String()+`"}`, console, nil)
	if started.Code != http.StatusOK {
		t.Fatalf("link start = %d %s", started.Code, started.Body)
	}
	var redirect struct {
		RedirectURL string `json:"redirect_url"`
	}
	if err := json.Unmarshal(started.Body.Bytes(), &redirect); err != nil {
		t.Fatal(err)
	}
	authorize, err := url.Parse(redirect.RedirectURL)
	if err != nil || !strings.HasPrefix(redirect.RedirectURL, issuer.Server.URL+"/authorize") {
		t.Fatalf("redirect = %q", redirect.RedirectURL)
	}
	query := authorize.Query()
	if query.Get("redirect_uri") != console+"/sso/oidc/callback" {
		t.Fatalf("redirect_uri = %q", query.Get("redirect_uri"))
	}
	now := time.Now()
	issuer.Nonce = query.Get("nonce")
	issuer.Claims = func(nonce string) map[string]any {
		return map[string]any{"iss": issuer.Server.URL, "aud": "helmr", "sub": "alice-oidc", "nonce": nonce,
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "email": "alice@corp.test"}
	}
	if got := browser.do(t, http.MethodPost, "/api/auth/sso/oidc/finish", `{"state":"forged","code":"good-code"}`, console, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("forged state = %d %s", got.Code, got.Body)
	}
	// A refused state ends the flow; start again.
	started = browser.do(t, http.MethodPost, "/api/account/sso/link", `{"organization_id":"`+orgID.String()+`"}`, console, nil)
	if err := json.Unmarshal(started.Body.Bytes(), &redirect); err != nil {
		t.Fatal(err)
	}
	authorize, _ = url.Parse(redirect.RedirectURL)
	issuer.Nonce = authorize.Query().Get("nonce")
	finished := browser.do(t, http.MethodPost, "/api/auth/sso/oidc/finish", `{"state":"`+authorize.Query().Get("state")+`","code":"good-code"}`, console, nil)
	if finished.Code != http.StatusOK || !strings.Contains(finished.Body.String(), "/settings/security") {
		t.Fatalf("finish = %d %s", finished.Code, finished.Body)
	}
	bindings := browser.do(t, http.MethodGet, "/api/account/sso/bindings", "", console, nil)
	if !strings.Contains(bindings.Body.String(), `"subject":"alice-oidc"`) || !strings.Contains(bindings.Body.String(), `"protocol":"oidc"`) {
		t.Fatalf("bindings = %d %s", bindings.Code, bindings.Body)
	}
}

// oidcAnswer has the issuer answer the authorization request in redirect for
// subject, and returns the state to finish with.
func oidcAnswer(t *testing.T, issuer *ssotest.OIDCIssuer, body []byte, subject string, authTime time.Time) url.Values {
	t.Helper()
	var redirect struct {
		RedirectURL string `json:"redirect_url"`
	}
	if err := json.Unmarshal(body, &redirect); err != nil {
		t.Fatal(err)
	}
	authorize, err := url.Parse(redirect.RedirectURL)
	if err != nil {
		t.Fatal(err)
	}
	query := authorize.Query()
	issuer.Nonce = query.Get("nonce")
	issuer.Claims = func(nonce string) map[string]any {
		return map[string]any{"iss": issuer.Server.URL, "aud": "helmr", "sub": subject, "nonce": nonce,
			"iat": time.Now().Unix(), "auth_time": authTime.Unix(), "exp": time.Now().Add(time.Hour).Unix(), "email": subject + "@example.test"}
	}
	return query
}

func TestOIDCSignInRefusesStaleAuthenticationUntilForced(t *testing.T) {
	f, issuer, orgID := newOIDCFixture(t)
	alice := f.user(t, "Alice")
	member := f.member(t, orgID, alice, "member")
	identity, binding := uuid.NewV7(), uuid.NewV7()
	var connection uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM sso_connections WHERE org_id=$1`, orgID).Scan(&connection); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO sso_identities(id,org_id,connection_id,subject,user_id) VALUES($1,$2,$3,'alice',$4)`, identity, orgID, connection, alice)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO sso_member_bindings(id,org_id,identity_id,member_id,user_id) VALUES($1,$2,$3,$4,$5)`, binding, orgID, identity, member, alice)
	console := f.origin
	browser := &ssoBrowser{f: f, cookies: map[string]*http.Cookie{}}

	started := browser.do(t, http.MethodPost, "/api/auth/sso/start", `{"organization":"acme"}`, console, nil)
	query := oidcAnswer(t, issuer, started.Body.Bytes(), "alice", time.Now().Add(-48*time.Hour))
	if query.Get("max_age") != "86400" || query.Get("prompt") != "" {
		t.Fatalf("authorization query = %v", query)
	}
	stale := browser.do(t, http.MethodPost, "/api/auth/sso/oidc/finish", `{"state":"`+query.Get("state")+`","code":"good-code"}`, console, nil)
	if stale.Code != http.StatusForbidden || !strings.Contains(stale.Body.String(), "sso_reauthentication_required") {
		t.Fatalf("stale authentication = %d %s", stale.Code, stale.Body)
	}
	started = browser.do(t, http.MethodPost, "/api/auth/sso/start", `{"organization":"acme","reauthenticate":true}`, console, nil)
	query = oidcAnswer(t, issuer, started.Body.Bytes(), "alice", time.Now())
	if query.Get("prompt") != "login" || query.Get("max_age") != "0" {
		t.Fatalf("forced authorization query = %v", query)
	}
	fresh := browser.do(t, http.MethodPost, "/api/auth/sso/oidc/finish", `{"state":"`+query.Get("state")+`","code":"good-code"}`, console, nil)
	if fresh.Code != http.StatusOK || browser.cookies["__Host-helmr_session"] == nil {
		t.Fatalf("fresh authentication = %d %s", fresh.Code, fresh.Body)
	}
	selected := map[string]string{"X-Helmr-Organization": orgID.String()}
	context := browser.do(t, http.MethodGet, "/api/organization", "", console, selected)
	if context.Code != http.StatusOK || !strings.Contains(context.Body.String(), `"sso":true`) {
		t.Fatalf("organization context = %d %s", context.Code, context.Body)
	}
	for _, path := range []string{"/api/organizations", "/api/auth/invitations/accept"} {
		if got := browser.do(t, http.MethodPost, path, `{"name":"Other","slug":"other","token":"x"}`, console, nil); got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), "ordinary_sign_in_required") {
			t.Fatalf("%s from an SSO-only session = %d %s", path, got.Code, got.Body)
		}
	}
}

func TestOwnerConfiguresTestsAndRequiresSAML(t *testing.T) {
	f := newHTTPPostgresFixture(t, func(cfg *ServerConfig) {
		store, err := sso.NewStore(make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		cfg.SSO = store
	})
	orgID, ownerToken := f.organizationOwner(t, "acme")
	console := f.origin
	selected := map[string]string{"X-Helmr-Organization": orgID.String()}
	owner := &ssoBrowser{f: f, cookies: map[string]*http.Cookie{"__Host-helmr_session": {Name: "__Host-helmr_session", Value: ownerToken}}}
	idp := ssotest.NewIdP(t)

	body, _ := json.Marshal(map[string]any{"protocol": "saml", "entity_id": idp.EntityID, "sso_url": "https://idp.example.test/sso", "certificates": []string{idp.CertPEM}})
	created := owner.do(t, http.MethodPost, "/api/organization/sso/connections", string(body), console, selected)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body)
	}
	var connection struct {
		ID    string `json:"id"`
		ACS   string `json:"service_provider_acs_url"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &connection); err != nil || connection.State != "candidate" || !strings.HasSuffix(connection.ACS, "/acs") {
		t.Fatalf("connection = %+v, %v", connection, err)
	}
	id := uuid.MustParse(connection.ID)
	if got := owner.do(t, http.MethodPost, "/api/organization/sso/connections/"+connection.ID+"/activate", "", console, selected); got.Code != http.StatusConflict {
		t.Fatalf("activate untested = %d %s", got.Code, got.Body)
	}
	acs, _ := owner.samlAnswer(t, idp, id, "owner", "/api/organization/sso/connections/"+connection.ID+"/test", "", selected)
	if acs.Header().Get("Location") != "/sso/callback" {
		t.Fatalf("test ACS = %s", acs.Header().Get("Location"))
	}
	if tested := owner.do(t, http.MethodPost, "/api/auth/sso/saml/finish", "", console, nil); tested.Code != http.StatusOK || !strings.Contains(tested.Body.String(), "tested=") {
		t.Fatalf("test finish = %d %s", tested.Code, tested.Body)
	}
	if got := owner.do(t, http.MethodPost, "/api/organization/sso/connections/"+connection.ID+"/activate", "", console, selected); got.Code != http.StatusNoContent {
		t.Fatalf("activate = %d %s", got.Code, got.Body)
	}
	if got := owner.do(t, http.MethodPatch, "/api/organization/sso", `{"required":true}`, console, selected); got.Code != http.StatusConflict {
		t.Fatalf("require by an owner who has not signed in through the connection = %d %s", got.Code, got.Body)
	}
	// The owner links their own company account, then requires SSO.
	if acs, _ := owner.samlAnswer(t, idp, id, "owner", "/api/account/sso/link", `{"organization_id":"`+orgID.String()+`"}`, nil); acs.Header().Get("Location") != "/sso/callback" {
		t.Fatalf("self-link ACS = %s", acs.Header().Get("Location"))
	}
	if linked := owner.do(t, http.MethodPost, "/api/auth/sso/saml/finish", "", console, nil); linked.Code != http.StatusOK {
		t.Fatalf("self-link finish = %d %s", linked.Code, linked.Body)
	}
	if got := owner.do(t, http.MethodPatch, "/api/organization/sso", `{"required":true,"session_seconds":172800}`, console, selected); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"session_seconds":172800`) {
		t.Fatalf("require = %d %s", got.Code, got.Body)
	}
	// Another browser of the owner holds no SSO proof; owners keep their
	// ordinary sign-in, so it still enters.
	other := &ssoBrowser{f: f, cookies: map[string]*http.Cookie{"__Host-helmr_session": {Name: "__Host-helmr_session", Value: f.session(t, ownerUser(t, f, orgID), uuid.Nil())}}}
	if got := other.do(t, http.MethodGet, "/api/organization", "", console, selected); got.Code != http.StatusOK {
		t.Fatalf("owner's ordinary browser under required SSO = %d %s", got.Code, got.Body)
	}
	status := other.do(t, http.MethodGet, "/api/organization/sso", "", console, selected)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"required":true`) {
		t.Fatalf("status = %d %s", status.Code, status.Body)
	}
	member := f.user(t, "Member")
	f.member(t, orgID, member, "member")
	memberBrowser := &ssoBrowser{f: f, cookies: map[string]*http.Cookie{"__Host-helmr_session": {Name: "__Host-helmr_session", Value: f.session(t, member, uuid.Nil())}}}
	if got := memberBrowser.do(t, http.MethodGet, "/api/organization", "", console, selected); got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), "sso_required") {
		t.Fatalf("member's ordinary browser under required SSO = %d %s", got.Code, got.Body)
	}
}

func ownerUser(t *testing.T, f httpPostgresFixture, orgID uuid.UUID) uuid.UUID {
	t.Helper()
	var user uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT user_id FROM org_members WHERE org_id=$1 AND is_owner`, orgID).Scan(&user); err != nil {
		t.Fatal(err)
	}
	return user
}
