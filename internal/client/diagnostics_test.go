package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestResourceReadsPreserveUnknownDiagnostics(t *testing.T) {
	message := "diagnosis"
	turn := api.AgentTurn{ID: testTurnID, SessionID: testSessionID, Status: "failed", Error: &api.AgentTurnError{Code: "future_turn_failure", Message: &message}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sessions/"+testSessionID+"/turns/"+testTurnID {
			_ = json.NewEncoder(w).Encode(turn)
		} else {
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.RetrieveSessionTurn(t.Context(), testSessionID, testTurnID, EnvironmentScopeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Error, turn.Error) {
		t.Fatalf("turn error=%+v", got.Error)
	}
}
