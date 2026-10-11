package slack

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestDedicatedCallbackAuthenticatesSelectedRegistrationBeforeInstallation(t *testing.T) {
	f, store, org := credentialFixture(t)
	registration := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO slack_app_registrations(id,organization_id,created_by_user_id) VALUES($1,$2,$3)`, registration, org, f.User)
	credentials := AppCredentials{ClientID: "client", ClientSecret: "private-client", SigningSecret: "selected-signing-secret"}
	if err := store.StoreAppCredentials(t.Context(), org, f.User, registration, credentials); err != nil {
		t.Fatal(err)
	}
	handler := store.CallbackHandler(registration, EventHandler{Database: f.Pool}, nil)
	call := func(handler http.Handler, body, secret string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/events", bytes.NewBufferString(body))
		req.Header = signedHeaders([]byte(secret), []byte(body), time.Now())
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, req)
		return result
	}
	challenge := `{"type":"url_verification","api_app_id":"dedicated-app","challenge":"verified-challenge"}`
	if got := call(handler, challenge, credentials.SigningSecret); got.Code != 200 {
		t.Fatal("pre-install challenge rejected", got.Code)
	}
	if got := call(handler, challenge, "another-app-signing-secret"); got.Code != 401 {
		t.Fatal("wrong signing secret accepted", got.Code)
	}
	unknown := store.CallbackHandler(uuid.NewV7(), EventHandler{Database: f.Pool}, nil)
	if got := call(unknown, challenge, credentials.SigningSecret); got.Code != 401 {
		t.Fatal("unknown registration searched other secrets", got.Code)
	}
	callback := `{"type":"event_callback","api_app_id":"dedicated-app","team_id":"team","event_id":"event","event":{"type":"app_mention","user":"human","channel":"C1","ts":"1.0","text":"hello"}}`
	if got := call(handler, callback, credentials.SigningSecret); got.Code != 401 {
		t.Fatal("pending registration admitted input", got.Code)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_app_registrations SET app_id='dedicated-app' WHERE id=$1`, registration)
	if got := call(handler, `{"type":"url_verification","api_app_id":"different-app","challenge":"substituted"}`, credentials.SigningSecret); got.Code != 400 {
		t.Fatal("verified app substituted", got.Code)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_app_registrations SET retired_at=clock_timestamp(),credential_ciphertext=NULL,credential_nonce=NULL WHERE id=$1`, registration)
	if got := call(handler, challenge, credentials.SigningSecret); got.Code != 401 {
		t.Fatal("retired registration accepted callback", got.Code)
	}
}
