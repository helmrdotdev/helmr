package workerclient

import (
	"bytes"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"
)

func TestPreparationSensitiveResponsesAreBoundedAndSanitized(t *testing.T) {
	for _, endpoint := range []string{"key", "secrets"} {
		for _, mode := range []string{"valid", "partial", "trailing", "oversized", "error"} {
			t.Run(endpoint+"/"+mode, func(t *testing.T) {
				key := workerapi.ComputerKeyMaterial{Scope: "scope", ID: uuid.NewV7().String(), Key: bytes.Repeat([]byte{7}, 32)}
				var material any = key
				if endpoint == "secrets" {
					material = workerapi.PreparationSecrets{Secrets: []workerapi.SecretDelivery{{Env: &workerapi.SecretEnv{Name: "LARGE"}, Value: bytes.Repeat([]byte{65}, 80<<10)}}}
				}
				body, err := json.Marshal(material)
				if err != nil {
					t.Fatal(err)
				}
				switch mode {
				case "partial":
					body = append(body[:len(body)-1], []byte(",SECRET-MARKER")...)
				case "trailing":
					body = append(body, []byte("SECRET-MARKER")...)
				case "oversized":
					body = append(body, bytes.Repeat([]byte{' '}, 2*wire.PreparationControlFrameBytes+1)...)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/worker/v1/instance/credential" {
						_ = json.NewEncoder(w).Encode(workerapi.HostCredentialResponse{WorkerEpoch: 7, Credential: "credential", ExpiresInSeconds: 3600})
						return
					}
					if mode == "error" {
						w.WriteHeader(503)
						_, _ = w.Write([]byte("SECRET-MARKER"))
						return
					}
					_, _ = w.Write(body)
				}))
				defer server.Close()
				c, err := New(server.URL, WithAuth(uuid.NewV7().String(), "fixture"), WithService(uuid.NewV7().String()))
				if err != nil {
					t.Fatal(err)
				}
				var resultBytes int
				if endpoint == "key" {
					result, callErr := c.PreparationWriteKey(t.Context(), workerapi.PreparationExecutor{})
					err = callErr
					resultBytes = len(result.Key)
					clear(result.Key)
				} else {
					result, callErr := c.PreparationSecrets(t.Context(), workerapi.PreparationExecutor{})
					err = callErr
					for _, secret := range result.Secrets {
						resultBytes += len(secret.Value)
						clear(secret.Value)
					}
				}
				if mode == "valid" {
					if err != nil || resultBytes == 0 {
						t.Fatalf("valid material: %v", err)
					}
				} else if err == nil || resultBytes != 0 || strings.Contains(err.Error(), "SECRET-MARKER") {
					t.Fatalf("sensitive failure escaped: %v", err)
				}
			})
		}
	}
}
