package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"
)

func TestBrowserMutationBindsExpectedActorToActualCookie(t *testing.T) {
	f := newHTTPPostgresFixture(t)
	org, first := f.organizationOwner(t, "actor-expectation")
	firstMe := f.request(t, http.MethodGet, "/api/me", first, "")
	var firstAccount struct {
		ID uuid.UUID `json:"user_id"`
	}
	if err := json.Unmarshal(firstMe.Body.Bytes(), &firstAccount); err != nil {
		t.Fatal(err)
	}
	secondUser := f.user(t, "Second account")
	f.member(t, org, secondUser, "member")
	second := f.session(t, secondUser, org)
	for _, headers := range [][]string{{firstAccount.ID.String()}, {"malformed"}, {secondUser.String(), secondUser.String()}} {
		r := httptest.NewRequest(http.MethodPatch, "/api/account", strings.NewReader(`{"display_name":"Changed by stale form"}`))
		r.Header.Set("Origin", f.origin)
		r.Header.Set("Content-Type", "application/json")
		r.Header["X-Helmr-Actor"] = headers
		r.AddCookie(&http.Cookie{Name: sessionCookieName(r), Value: second})
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"account_changed"`) {
			t.Fatalf("expected actor: %d %s", w.Code, w.Body.String())
		}
	}
	unchanged := f.request(t, http.MethodGet, "/api/me", second, "")
	if !strings.Contains(unchanged.Body.String(), `"display_name":"Second account"`) {
		t.Fatalf("stale form mutated account: %s", unchanged.Body.String())
	}
	r := httptest.NewRequest(http.MethodPatch, "/api/account", strings.NewReader(`{"display_name":"Explicit same account"}`))
	r.Header.Set("Origin", f.origin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Helmr-Actor", secondUser.String())
	r.AddCookie(&http.Cookie{Name: sessionCookieName(r), Value: second})
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
		t.Fatalf("same actor denied: %d %s", w.Code, w.Body.String())
	}
}
