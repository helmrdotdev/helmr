package client

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/api"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestPromoteDeploymentObservesPreparationBeforeRetry(t *testing.T) {
	for _, state := range []string{"ready", "failed"} {
		t.Run(state, func(t *testing.T) {
			var posts, gets atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				response := api.DeploymentResponse{ID: testTriggerID}
				switch r.Method {
				case http.MethodPost:
					if posts.Add(1) == 1 {
						response.PreparationState = "preparing"
						w.WriteHeader(http.StatusAccepted)
					}
				case http.MethodGet:
					gets.Add(1)
					response.PreparationState = state
				default:
					t.Errorf("unexpected method %s", r.Method)
				}
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			client, err := New(server.URL, WithHTTPClient(server.Client()))
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.PromoteDeployment(t.Context(), testTriggerID, EnvironmentScopeOptions{})
			if gets.Load() != 1 {
				t.Fatalf("observations %d", gets.Load())
			}
			if state == "failed" {
				if err == nil || posts.Load() != 1 {
					t.Fatalf("failed preparation retried: posts=%d err=%v", posts.Load(), err)
				}
			} else if err != nil || posts.Load() != 2 || got.ID != testTriggerID {
				t.Fatalf("ready promotion: posts=%d response=%+v err=%v", posts.Load(), got, err)
			}
		})
	}
}
