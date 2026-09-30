package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

func TestInitialComputerKeyAuthenticatedHTTP(t *testing.T) {
	f, broker, fence := initialKeyFixture(t)
	handler := serveComputerKeys(t, f, broker)
	credential := seedHostCredential(t, f.Pool, f.worker.HostID)
	httpServer := httptest.NewServer(handler)
	defer httpServer.Close()
	client := credential.client(t, httpServer.URL)
	token := credential.token(t, handler)
	request := workerapi.InitialComputerKeyRequest{ComputerInstanceID: pgvalue.UUIDString(fence.RuntimeID), DesiredVersion: fence.DesiredVersion}
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
	if w := call(token, payload); w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("successful response status/cache policy: %d", w.Code)
	}
	// Tokens the exchange never issues: another host's, and an expired one.
	foreign := rawWorkerJWTClaims(uuid.NewV7().String(), pgvalue.UUIDString(fence.WorkerGroupID), uuid.NewV7().String())
	expired := rawWorkerJWTClaims(pgvalue.UUIDString(fence.WorkerID), pgvalue.UUIDString(fence.WorkerGroupID), uuid.NewV7().String())
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
			w := call(token, body)
			if w.Code != 400 && w.Code != 413 {
				t.Fatalf("invalid input accepted status=%d", w.Code)
			}
			if strings.Contains(w.Body.String(), "secret") {
				t.Fatal("request body echoed")
			}
		})
	}
	wrongRuntime := request
	wrongRuntime.ComputerInstanceID = uuid.NewV7().String()
	wrongPayload, _ := json.Marshal(wrongRuntime)
	if w := call(token, wrongPayload); w.Code != 409 {
		t.Fatalf("unowned runtime status=%d", w.Code)
	}
	// Draining the host advances its claim version: the token minted before
	// the drain no longer authenticates.
	drain := httptest.NewRequest("POST", "/worker/v1/instance/drain", nil)
	drain.Header.Set("Authorization", "Bearer "+token)
	drained := httptest.NewRecorder()
	handler.ServeHTTP(drained, drain)
	if drained.Code != 200 {
		t.Fatalf("drain status=%d body=%s", drained.Code, drained.Body.String())
	}
	if w := call(token, payload); w.Code != 401 || strings.Contains(w.Body.String(), first.ID) {
		t.Fatalf("pre-drain token key response status=%d", w.Code)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtime)
	if material, err := client.InitialComputerKey(t.Context(), request); err == nil || len(material.Key) != 0 {
		t.Fatal("revoked runtime delivered key")
	}
}

// serveComputerKeys serves the control plane over the fixture's database with
// the broker's Computer key wrapper and the fixture's CAS.
func serveComputerKeys(t *testing.T, f initialPublicationFixture, broker *computerKeyBroker) http.Handler {
	t.Helper()
	return newPostgresServer(t, f.Pool, func(cfg *ServerConfig) {
		cfg.ComputerKeys = broker.wrapper
		cfg.CAS = f.server.cas
	})
}

func sourceKeyHTTPClient(t *testing.T, f initialPublicationFixture, broker *computerKeyBroker) *workerclient.Client {
	t.Helper()
	credential := seedHostCredential(t, f.Pool, f.worker.HostID)
	httpServer := httptest.NewServer(serveComputerKeys(t, f, broker))
	t.Cleanup(httpServer.Close)
	return credential.client(t, httpServer.URL)
}
