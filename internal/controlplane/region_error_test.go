package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/region"
)

func TestRegionErrorMapsHTTPContract(t *testing.T) {
	var input region.InputError
	for _, test := range []struct {
		name    string
		err     error
		status  int
		code    string
		message string
	}{
		{name: "input", err: input, status: http.StatusBadRequest, code: "bad_request"},
		{name: "not found", err: region.ErrNotFound, status: http.StatusNotFound, code: "not_found", message: "region not found"},
		{name: "exists", err: region.ErrExists, status: http.StatusConflict, code: "conflict", message: "region identity is already in use"},
		{name: "wrapped", err: fmt.Errorf("update region: %w", region.ErrNotFound), status: http.StatusNotFound, code: "not_found", message: "update region: region not found"},
		{name: "unmapped", err: errors.New("database is down"), status: http.StatusInternalServerError, code: "internal_error", message: "internal server error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			(&Server{log: discardTestLogger()}).writeRegionError(recorder, test.err)
			if recorder.Code != test.status {
				t.Fatalf("status = %d, want %d", recorder.Code, test.status)
			}
			var body api.HTTPErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != test.code || (test.message != "" && body.Error.Message != test.message) {
				t.Fatalf("error = %+v, want %s %q", body.Error, test.code, test.message)
			}
		})
	}
}
