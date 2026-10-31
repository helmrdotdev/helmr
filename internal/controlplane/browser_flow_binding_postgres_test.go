package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
)

// rotateSession replaces a fixture browser's token the way a sign-in
// confirmation does: the session stays, its token changes.
func (f httpPostgresFixture) rotateSession(t *testing.T, token string) string {
	t.Helper()
	old, err := auth.HashToken(f.keys.Session, token)
	if err != nil {
		t.Fatal(err)
	}
	next, err := auth.GenerateOpaque(32)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := auth.HashToken(f.keys.Session, next)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := f.pool.Exec(t.Context(), `UPDATE login_sessions SET token_hash=$2,authenticated_at=clock_timestamp() WHERE token_hash=$1`, old, hash)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatal("rotate session", err)
	}
	f.browserOrganizations[next] = f.browserOrganizations[token]
	return next
}

// A browser sign-in flow belongs to the session that started it, or to a
// signed-out browser. Confirming the sign-in mid-flow keeps the session, so the
// flow still finishes; any other session, or a sign-in made in between, cannot.
func TestBrowserFlowFinishesOnlyInTheSessionThatStartedIt(t *testing.T) {
	for _, scenario := range []string{"link-after-confirmation", "link-from-other-session", "login-signed-in-mid-flow"} {
		t.Run(scenario, func(t *testing.T) {
			f := newHTTPPostgresFixture(t, func(cfg *ServerConfig) { cfg.AuthProvider = continuationAuthProvider{} })
			serve := func(path string, body any, token string, cookies []*http.Cookie) *httptest.ResponseRecorder {
				t.Helper()
				data, _ := json.Marshal(body)
				r := httptest.NewRequest(http.MethodPost, f.origin+path, bytes.NewReader(data))
				r.Header.Set("Origin", f.origin)
				if token != "" {
					r.AddCookie(&http.Cookie{Name: sessionCookieName(r), Value: token})
				}
				for _, cookie := range cookies {
					r.AddCookie(cookie)
				}
				w := httptest.NewRecorder()
				f.handler.ServeHTTP(w, r)
				return w
			}
			start := func(path, token string) ([]*http.Cookie, string) {
				t.Helper()
				w := serve(path, api.GitHubAuthStartRequest{}, token, nil)
				var result api.GitHubAuthStartResponse
				if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil {
					t.Fatal(w.Code, w.Body)
				}
				u, err := url.Parse(result.RedirectURL)
				if err != nil {
					t.Fatal(err)
				}
				return w.Result().Cookies(), u.Query().Get("state")
			}
			finish := func(token string, cookies []*http.Cookie, state string) *httptest.ResponseRecorder {
				return serve("/api/auth/github/finish", api.GitHubAuthFinishRequest{State: state, Code: "fixture"}, token, cookies)
			}
			user := f.user(t, "Owner")
			session := f.session(t, user, uuid.Nil())
			linked := func() bool {
				t.Helper()
				var found bool
				if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM auth_identities WHERE user_id=$1 AND provider='github' AND revoked_at IS NULL)`, user).Scan(&found); err != nil {
					t.Fatal(err)
				}
				return found
			}
			var completed *httptest.ResponseRecorder
			switch scenario {
			case "link-after-confirmation":
				cookies, state := start("/api/account/providers/github/link", session)
				completed = finish(f.rotateSession(t, session), cookies, state)
				if completed.Code != http.StatusOK || !linked() {
					t.Fatal("confirmation mid-flow broke the link", completed.Code, completed.Body)
				}
				return
			case "link-from-other-session":
				cookies, state := start("/api/account/providers/github/link", session)
				completed = finish(f.session(t, user, uuid.Nil()), cookies, state)
				if linked() {
					t.Fatal("another session finished the link")
				}
			case "login-signed-in-mid-flow":
				cookies, state := start("/api/auth/github/start", "")
				completed = finish(session, cookies, state)
			}
			if completed.Code != http.StatusBadRequest || !strings.Contains(completed.Body.String(), "wrong_account") {
				t.Fatal("flow finished in another session", completed.Code, completed.Body)
			}
		})
	}
}
