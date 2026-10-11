package slack

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func interactionBody(t *testing.T, handler InteractionHandler, claims controlEnvelope, actor, actionTime string) []byte {
	t.Helper()
	value, err := encodeControl(handler.ControlKey, claims)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"type": "block_actions", "api_app_id": "app", "team": map[string]string{"id": "team"}, "user": map[string]string{"id": actor}, "actions": []any{map[string]string{"type": "button", "action_id": "helmr.stop", "value": value, "action_ts": actionTime}}})
	return []byte(url.Values{"payload": []string{string(payload)}}.Encode())
}
func serveSignedInteraction(handler InteractionHandler, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/slack/interactions", bytes.NewReader(body))
	request.Header = signedHeaders(handler.SigningSecret, body, time.Now())
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
func TestSlackStopInteractionReceiptsBeforeACKWithoutCoreEffects(t *testing.T) {
	f := newStatusFixture(t)
	handler := InteractionHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret"), ControlKey: []byte(strings.Repeat("k", 32))}
	claims := f.control("stop")
	timestamp := slackNow()
	body := interactionBody(t, handler, claims, "human", timestamp)
	for range 2 {
		if response := serveSignedInteraction(handler, body); response.Code != http.StatusOK {
			t.Fatalf("callback: %d", response.Code)
		}
	}
	var receipts, controls int
	if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM slack_requests WHERE status='received'),(SELECT count(*) FROM session_controls)`).Scan(&receipts, &controls); err != nil || receipts != 1 || controls != 0 {
		t.Fatalf("ACK boundary: receipts=%d controls=%d %v", receipts, controls, err)
	}
	for _, other := range [][]byte{interactionBody(t, handler, claims, "another-human", timestamp), interactionBody(t, handler, claims, "human", slackNow())} {
		if response := serveSignedInteraction(handler, other); response.Code != http.StatusOK {
			t.Fatalf("new gesture: %d", response.Code)
		}
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_requests`).Scan(&receipts); err != nil || receipts != 3 {
		t.Fatalf("different actor/click collapsed: %d %v", receipts, err)
	}
}
func TestSlackInteractionAuthenticatesRawFormAndSignedTargetBeforeSQL(t *testing.T) {
	handler := InteractionHandler{AppID: "app", SigningSecret: []byte("fixture-secret"), ControlKey: []byte(strings.Repeat("k", 32))}
	request := httptest.NewRequest(http.MethodPost, "/slack/interactions", strings.NewReader("payload={}"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned payload: %d", response.Code)
	}
	for _, body := range [][]byte{
		[]byte("payload=%7B%7D&payload=%7B%7D"),
		[]byte(url.Values{"payload": []string{`{"type":"block_actions","api_app_id":"app","team":{"id":"team"},"user":{"id":"human"},"actions":[{"type":"button","action_id":"helmr.stop","value":"forged.control","action_ts":"123.456"}]}`}}.Encode()),
	} {
		if response := serveSignedInteraction(handler, body); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid form/target reached SQL: %d", response.Code)
		}
	}
}
func TestSlackStopInteractionDoesNotReviveExpiredOrUnauthorizedControls(t *testing.T) {
	f := newStatusFixture(t)
	handler := InteractionHandler{Database: f.Pool, AppID: "app", SigningSecret: []byte("fixture-secret"), ControlKey: []byte(strings.Repeat("k", 32))}
	claims := f.control("stop")
	claims.ExpiresAt = time.Now().Add(-time.Minute).Unix()
	if response := serveSignedInteraction(handler, interactionBody(t, handler, claims, "human", slackNow())); response.Code != http.StatusOK {
		t.Fatalf("expired callback: %d", response.Code)
	}
	var rejected bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*)=1 FROM slack_requests WHERE status='rejected' AND error='gesture_expired'`).Scan(&rejected); err != nil || !rejected {
		t.Fatalf("expired token revived: %v %v", rejected, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorization_lost_at=clock_timestamp() WHERE id=$1`, f.installation)
	if response := serveSignedInteraction(handler, interactionBody(t, handler, f.control("stop"), "human", slackNow())); response.Code != http.StatusOK {
		t.Fatalf("unavailable callback: %d", response.Code)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*)=1 FROM slack_requests WHERE status='rejected' AND error='installation_unavailable'`).Scan(&rejected); err != nil || !rejected {
		t.Fatalf("unavailable token parked: %v %v", rejected, err)
	}
}
