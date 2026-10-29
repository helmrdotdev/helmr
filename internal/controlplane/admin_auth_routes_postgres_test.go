package controlplane

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestAdminAuthenticationRouteBoundaries(t *testing.T) {
	f := newSupplyHTTPFixture(t)
	_, ordinary := f.organizationOwner(t, "ordinary")
	operator := f.operatorSession(t)
	const id = "00000000-0000-7000-8000-000000000999"
	protected := []struct{ method, path string }{
		{"GET", "/me"}, {"GET", "/setup"}, {"GET", "/credentials"},
		{"POST", "/credentials/enroll"}, {"DELETE", "/credentials/" + id},
		{"POST", "/auth/renew/options"}, {"POST", "/auth/renew/verify"},
		{"POST", "/account-repairs/redeem"},
		{"POST", "/account-repairs/" + id + "/options"},
		{"POST", "/account-repairs/" + id + "/complete"},
		{"POST", "/account-repairs/" + id + "/assert/options"},
		{"POST", "/account-repairs/" + id + "/assert/verify"},
		{"GET", "/regions"}, {"POST", "/regions"},
	}
	for _, route := range protected {
		for name, token := range map[string]string{"anonymous": "", "ordinary": ordinary} {
			t.Run(name+" "+route.method+" "+route.path, func(t *testing.T) {
				got := f.request(t, route.method, "/admin/api/v1"+route.path, token, "{}")
				if got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), "authentication_required") {
					t.Fatalf("operator guard missing: %d %s", got.Code, got.Body)
				}
			})
		}
	}
	// Logging in must be possible without an existing operator session.
	if got := f.request(t, "POST", "/admin/api/v1/auth/options", "", "{}"); got.Code != http.StatusOK {
		t.Fatalf("anonymous challenge: %d %s", got.Code, got.Body)
	}
	for _, path := range []string{"/me", "/setup", "/credentials", "/regions"} {
		if got := f.request(t, "GET", "/admin/api/v1"+path, operator, ""); got.Code != http.StatusOK {
			t.Fatalf("authenticated %s: %d %s", path, got.Code, got.Body)
		}
	}
	// Old confirmation must block infrastructure mutation but permit the
	// ceremony that obtains fresh proof and renews the session.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE login_sessions SET privileged_login_at=clock_timestamp()-interval '2 hours',privileged_confirmed_at=clock_timestamp()-interval '2 hours',expires_at=clock_timestamp()+interval '9 hours' WHERE channel='operator'`)
	if got := f.request(t, "POST", "/admin/api/v1/regions", operator, `{"id":"blocked","display_name":"Blocked"}`); got.Code != http.StatusForbidden || !strings.Contains(got.Body.String(), "recent_authentication_required") {
		t.Fatalf("stale mutation: %d %s", got.Code, got.Body)
	}
	if got := f.request(t, "POST", "/admin/api/v1/auth/renew/options", operator, "{}"); got.Code != http.StatusOK {
		t.Fatalf("stale renewal: %d %s", got.Code, got.Body)
	}
	// Both anonymous ceremonies and authenticated mutations keep origin checks.
	for _, path := range []string{"/auth/options", "/enrollment/redeem", "/auth/renew/options", "/account-repairs/redeem", "/regions"} {
		r := httptest.NewRequest("POST", "/admin/api/v1"+path, strings.NewReader("{}"))
		f.authenticateRequest(r, operator)
		r.Header.Set("Origin", "https://other.example.test")
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "same-origin") {
			t.Fatalf("cross-origin %s: %d %s", path, w.Code, w.Body)
		}
	}
}
