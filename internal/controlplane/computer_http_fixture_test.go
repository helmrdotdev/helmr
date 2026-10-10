package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"uuid"

	"github.com/jackc/pgx/v5/pgxpool"
)

// workerHTTPClient calls /worker/v1 routes of a NewServer handler with an
// host credential issued for a seeded worker host's secret.
type workerHTTPClient struct {
	handler        http.Handler
	hostCredential string
}

func newWorkerHTTPClient(t *testing.T, handler http.Handler, pool *pgxpool.Pool, hostID uuid.UUID) workerHTTPClient {
	t.Helper()
	hostSecret := seedHostSecret(t, pool, hostID)
	return workerHTTPClient{handler: handler, hostCredential: hostSecret.issue(t, handler)}
}

// post sends body as JSON, requires the status and decodes a 200 response
// into out when out is non-nil.
func (c workerHTTPClient) post(t *testing.T, path string, body any, want int, out any) *httptest.ResponseRecorder {
	t.Helper()
	response := c.send(t, path, body)
	if response.Code != want {
		t.Fatalf("POST %s status = %d, want %d: %s", path, response.Code, want, response.Body.String())
	}
	if out != nil && response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	return response
}

// send sends body as JSON and returns the response whatever its status.
func (c workerHTTPClient) send(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer "+c.hostCredential)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	c.handler.ServeHTTP(response, request)
	return response
}
