package controlplane

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/token"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestWorkerSourceErrorMappersRefreshClaimsBeforeDomainErrors(t *testing.T) {
	server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for name, write := range map[string]func(http.ResponseWriter, error){
		"token":      server.writeTokenError,
		"child task": func(w http.ResponseWriter, err error) { server.writeChildTaskInvokeError(w, "test", err) },
		"actor":      func(w http.ResponseWriter, err error) { server.writeWorkerActorSourceError(w, "start", "test", err) },
		"computer": func(w http.ResponseWriter, err error) {
			server.writeWorkerComputerSourceError(w, "create", "test", err)
		},
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			write(response, errors.Join(workergroup.ErrStaleClaims, run.ErrStaleSource, run.ErrChildInvokeStale, token.ErrCreateAuthority))
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("claims response status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
