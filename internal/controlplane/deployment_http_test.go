package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/deployment"
)

func TestDeploymentErrorMapsHTTPContract(t *testing.T) {
	input := deployment.InputError{Err: errors.New(`scheduled Computer Secret "REPORT_TOKEN" is unavailable`)}
	for _, test := range []struct {
		err     error
		status  int
		code    string
		message string
	}{
		{input, http.StatusBadRequest, "bad_request", input.Error()},
		{deployment.ErrPermissionRequired, http.StatusForbidden, "forbidden", "permission is required"},
		{deployment.ErrNotFound, http.StatusNotFound, "not_found", "deployment not found"},
		{deployment.ErrNotDeployable, http.StatusNotFound, "not_found", "deployment not found or is not deployable"},
		{deployment.ErrDefinitionNotFound, http.StatusNotFound, "not_found", "definition not found"},
		{deployment.ErrNoCurrentDeployment, http.StatusNotFound, "no_current_deployment", "no current deployment"},
		{deployment.ErrNoCurrentDefinitions, http.StatusNotFound, "no_current_deployment", "Environment has no current Deployment"},
		{deployment.ErrSelectedDeploymentNotFound, http.StatusNotFound, "deployment_not_found", "Deployment was not found"},
		{fmt.Errorf("promote deployment: %w", errors.New("connection reset")), http.StatusInternalServerError, "internal_error", "internal server error"},
	} {
		t.Run(test.err.Error(), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeError(recorder, deploymentError(test.err))
			var body api.HTTPErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != test.status || body.Error.Code != test.code || body.Error.Message != test.message {
				t.Fatalf("response = %d %+v, want %d %s %q", recorder.Code, body.Error, test.status, test.code, test.message)
			}
		})
	}
}
