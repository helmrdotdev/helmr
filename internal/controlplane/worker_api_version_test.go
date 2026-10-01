package controlplane

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerConnectionAPIVersion(t *testing.T) {
	router, err := NewServer(completeServerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/worker/v1/enrollment", "/worker/v1/instance/credential"} {
		for _, body := range []string{`{}`, `{"api_version":"different"}`} {
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			out := httptest.NewRecorder()
			router.ServeHTTP(out, req)
			assertAdminError(t, out, http.StatusConflict, workerapi.APIVersionMismatchCode)
			if !strings.Contains(out.Body.String(), workerapi.APIVersion) {
				t.Fatal("missing expected version diagnostic")
			}
		}
	}
}

func TestWorkerOrdinaryRoutesDoNotRequireAPIVersion(t *testing.T) {
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
