package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/org"
)

func TestOrgErrorMapsHTTPContract(t *testing.T) {
	_, input := org.NormalizeEmail("")
	var inputError org.InputError
	if !errors.As(input, &inputError) {
		t.Fatalf("invalid email error = %v, want org.InputError", input)
	}
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{input, http.StatusBadRequest, "bad_request"},
		{org.ErrOrganizationSlugInUse, http.StatusBadRequest, "bad_request"},
		{org.ErrProjectSlugInUse, http.StatusBadRequest, "bad_request"},
		{org.ErrEnvironmentSlugInUse, http.StatusBadRequest, "bad_request"},
		{org.ErrNoRegion, http.StatusBadRequest, "bad_request"},
		{org.ErrDefaultRegionNotFound, http.StatusBadRequest, "bad_request"},
		{org.ErrMemberManagementRequired, http.StatusForbidden, "forbidden"},
		{org.ErrOwnerRoleRequired, http.StatusForbidden, "forbidden"},
		{org.ErrLastActiveOwner, http.StatusForbidden, "forbidden"},
		{org.ErrSelfMemberManagement, http.StatusForbidden, "forbidden"},
		{org.ErrSelfMemberRemoval, http.StatusForbidden, "forbidden"},
		{org.ErrProjectNotFound, http.StatusNotFound, "not_found"},
		{org.ErrEnvironmentNotFound, http.StatusNotFound, "not_found"},
		{org.ErrMemberNotFound, http.StatusNotFound, "not_found"},
		{org.ErrInvitationNotFound, http.StatusNotFound, "not_found"},
		{org.ErrOrganizationExists, http.StatusConflict, "conflict"},
		{org.ErrMemberRoleChanged, http.StatusConflict, "conflict"},
		{org.ErrInvitationPending, http.StatusConflict, "conflict"},
		{org.ErrInvitationActiveMember, http.StatusConflict, "conflict"},
		{fmt.Errorf("create project: %w", errors.New("connection reset")), http.StatusInternalServerError, "internal_error"},
	} {
		t.Run(test.err.Error(), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeError(recorder, orgError(test.err))
			var body api.HTTPErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			wantMessage := test.err.Error()
			if test.status == http.StatusInternalServerError {
				wantMessage = "internal server error"
			}
			if recorder.Code != test.status || body.Error.Code != test.code || body.Error.Message != wantMessage {
				t.Fatalf("response = %d %+v, want %d %s %q", recorder.Code, body.Error, test.status, test.code, wantMessage)
			}
		})
	}
}
