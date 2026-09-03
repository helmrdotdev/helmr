package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerWorkspaceExecFailureDoesNotClassifyUnknownInfrastructureError(t *testing.T) {
	failure, ok := workerWorkspaceExecFailure(errors.New("database connection lost"))
	if ok || failure.Code != "" {
		t.Fatalf("failure = %+v classified = %t", failure, ok)
	}
}

func TestParseWorkspaceWorkerIDsRequiresCanonicalUUIDv7(t *testing.T) {
	valid := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31"
	for _, value := range []string{
		"8fa3431e-c649-4ea0-bf12-b8e9fcdf1d8d",
		"019C10D5-A6F7-7AF1-8F5F-BB97BCC0DC31",
		" " + valid,
	} {
		if _, _, err := parseWorkspaceWorkerIDs(value, valid); err == nil {
			t.Fatalf("parseWorkspaceWorkerIDs accepted org_id %q", value)
		}
		if _, _, err := parseWorkspaceWorkerIDs(valid, value); err == nil {
			t.Fatalf("parseWorkspaceWorkerIDs accepted workspace_mount_id %q", value)
		}
	}
}

func TestWorkerCompleteWorkspaceExecRejectsNonCanonicalUUIDv7(t *testing.T) {
	body, err := json.Marshal(workerapi.WorkspaceExecCompleteRequest{
		OrgID: " 019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/worker/workspaces/execs/complete", bytes.NewReader(body))
	response := httptest.NewRecorder()

	(&Server{}).workerCompleteWorkspaceExec(response, request)

	if response.Code != 400 {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
}
