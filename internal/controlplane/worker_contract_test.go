package controlplane

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerConnectionContract(t *testing.T) {
	router, err := NewServer(completeServerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/worker/v1/enrollment", "/worker/v1/instance/token"} {
		for _, body := range []string{`{}`, `{"contract":"different"}`} {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			out := httptest.NewRecorder()
			router.ServeHTTP(out, req)
			assertAdminError(t, out, http.StatusConflict, workerapi.ContractMismatchCode)
			if !strings.Contains(out.Body.String(), workerapi.Contract) {
				t.Fatal("missing expected contract diagnostic")
			}
		}
	}
}

func TestWorkerOrdinaryRoutesDoNotRequireContract(t *testing.T) {
	router, err := NewServer(completeServerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"", "different"} {
		req := httptest.NewRequest(http.MethodPost, "/worker/v1/instance/observations", strings.NewReader(`{}`))
		if header != "" {
			req.Header.Set("Helmr-Worker-Contract", header)
		}
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		assertAdminError(t, out, http.StatusUnauthorized, "unauthorized")
	}
}
