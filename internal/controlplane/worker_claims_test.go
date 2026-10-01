package controlplane

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func TestWorkerClaimsSurviveStaleErrorTranslation(t *testing.T) {
	for name, translate := range map[string]func(error) error{
		"actor":        staleActorCompletion,
		"actor turn":   staleActorTurnCommit,
		"actor output": staleActorOutputAppend,
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			if !writeStaleWorkerClaims(response, translate(workergroup.ErrStaleClaims)) || response.Code != http.StatusUnauthorized {
				t.Fatalf("claims error did not request authentication: status=%d", response.Code)
			}
		})
	}
	response := httptest.NewRecorder()
	if writeStaleWorkerClaims(response, errStaleRunLeaseClaim) || response.Body.Len() != 0 {
		t.Fatal("a stale lease was translated into an authentication refresh")
	}
}

func TestWorkerSourceErrorMappersRefreshClaimsBeforeDomainErrors(t *testing.T) {
	server := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for name, write := range map[string]func(http.ResponseWriter, error){
		"token":      server.writeTokenError,
		"child task": func(w http.ResponseWriter, err error) { server.writeChildTaskInvokeError(w, "test", "call", err) },
		"actor":      func(w http.ResponseWriter, err error) { server.writeWorkerActorSourceError(w, "start", "test", err) },
		"computer": func(w http.ResponseWriter, err error) {
			server.writeWorkerComputerSourceError(w, "create", "test", err)
		},
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			write(response, errors.Join(workergroup.ErrStaleClaims, run.ErrStaleSource, errChildTaskInvokeStale, errTokenCreateAuthority))
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("claims response status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
