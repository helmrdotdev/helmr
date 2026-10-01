package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestInitialComputerKeyAuthenticatedHTTP(t *testing.T) {
	f := newInitialPublicationFixture(t)
	handler := f.serve(t)
	hostSecret := seedHostSecret(t, f.Pool, f.worker.HostID)
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	client := hostSecret.client(t, httpServer.URL)
	hostCredential := hostSecret.issue(t, handler)
	request := workerapi.InitialComputerKeyRequest{ComputerInstanceID: pgvalue.UUIDString(f.instance), DesiredVersion: 1}
	first, err := client.InitialComputerKey(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(first.Key)
	second, err := client.InitialComputerKey(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(second.Key)
	if first.ID != second.ID || first.Scope != second.Scope || !bytes.Equal(first.Key, second.Key) {
		t.Fatal("lost-response retry changed material")
	}
	payload, _ := json.Marshal(request)
	call := func(bearer string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/worker/v1/run/computer-instances/initialization/key", bytes.NewReader(body))
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := call(hostCredential, payload); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("successful response status/cache policy: %d", w.Code)
	}
	// Tokens the exchange never issues: another host's, and an expired one.
	foreign := rawWorkerJWTClaims(uuid.NewV7().String(), f.worker.GroupID.String(), uuid.NewV7().String())
	expired := rawWorkerJWTClaims(f.worker.HostID.String(), f.worker.GroupID.String(), uuid.NewV7().String())
	expired["exp"] = time.Now().Add(-time.Second).Unix()
	for name, bearer := range map[string]string{"missing": "", "foreign worker": signRawWorkerJWT(t, foreign), "expired": signRawWorkerJWT(t, expired)} {
		t.Run(name, func(t *testing.T) {
			w := call(bearer, payload)
			if w.Code != 401 || strings.Contains(w.Body.String(), first.ID) {
				t.Fatalf("unauthorized key response status=%d", w.Code)
			}
		})
	}
	for name, body := range map[string][]byte{
		"caller key": []byte(`{"computer_instance_id":"` + request.ComputerInstanceID + `","desired_version":1,"key":"secret"}`),
		"oversized":  []byte(`{"computer_instance_id":"` + strings.Repeat("x", 1100) + `","desired_version":1}`),
	} {
		t.Run(name, func(t *testing.T) {
			w := call(hostCredential, body)
			if w.Code != 400 && w.Code != 413 {
				t.Fatalf("invalid input accepted status=%d", w.Code)
			}
			if strings.Contains(w.Body.String(), "secret") {
				t.Fatal("request body echoed")
			}
		})
	}
	wrongInstance := request
	wrongInstance.ComputerInstanceID = uuid.NewV7().String()
	wrongPayload, _ := json.Marshal(wrongInstance)
	if w := call(hostCredential, wrongPayload); w.Code != 409 {
		t.Fatalf("unowned runtime status=%d", w.Code)
	}
	// Draining the host advances its claim version: the host credential minted before
	// the drain no longer authenticates.
	drain := httptest.NewRequest("POST", "/worker/v1/instance/drain", nil)
	drain.Header.Set("Authorization", "Bearer "+hostCredential)
	drained := httptest.NewRecorder()
	handler.ServeHTTP(drained, drain)
	if drained.Code != 200 {
		t.Fatalf("drain status=%d body=%s", drained.Code, drained.Body.String())
	}
	if w := call(hostCredential, payload); w.Code != 401 || strings.Contains(w.Body.String(), first.ID) {
		t.Fatalf("pre-drain host credential key response status=%d", w.Code)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.instance)
	if material, err := client.InitialComputerKey(t.Context(), request); err == nil || len(material.Key) != 0 {
		t.Fatal("revoked runtime delivered key")
	}
}
