package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestPublicLifecycleProjectionsRejectUnknownInternalValues(t *testing.T) {
	tests := []struct {
		name    string
		project func() error
	}{
		{name: "run", project: func() error { _, err := runPublicStatus("future"); return err }},
		{name: "schedule", project: func() error { _, err := schedulePublicStatus("future"); return err }},
		{name: "workspace", project: func() error { _, err := workspacePublicStatus("future"); return err }},
		{name: "session", project: func() error { _, err := sessionStatus("future"); return err }},
		{name: "worker group", project: func() error { _, err := workerGroupPublicStatus("future"); return err }},
		{name: "capacity worker", project: func() error { _, err := workerInstancePublicStatus("future"); return err }},
		{name: "worker", project: func() error { _, err := workerPublicStatus("future"); return err }},
		{name: "secret", project: func() error { _, err := secretPublicStatus("future"); return err }},
		{name: "token", project: func() error { _, err := tokenPublicStatus("future"); return err }},
		{name: "deployment", project: func() error { _, err := deploymentPublicStatus("future"); return err }},
		{name: "region", project: func() error { _, err := regionPublicStatus("future"); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.project(); err == nil {
				t.Fatal("unknown internal lifecycle value was projected")
			}
		})
	}
}

func TestPublicLifecycleTokensUseCanonicalSnakeCase(t *testing.T) {
	if status, err := runPublicStatus(db.RunStatusSystemFailed); err != nil || status != api.RunStatusSystemFailed {
		t.Fatalf("run status = %q, err = %v", status, err)
	}
	if status, err := schedulePublicStatus("pending_workspace"); err != nil || status != api.ScheduleStatusPendingWorkspace {
		t.Fatalf("schedule status = %q, err = %v", status, err)
	}
	if status, err := workspacePublicStatus(db.WorkspaceStateRecoveryRequired); err != nil || status != api.WorkspaceStatusRecoveryRequired {
		t.Fatalf("workspace status = %q, err = %v", status, err)
	}
}

func TestRunStatusFilterUsesCanonicalToken(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1/runs?status=system_failed", nil)
	statuses, err := parseRunStatusFilter(request)
	if err != nil || len(statuses) != 1 || statuses[0] != db.RunStatusSystemFailed {
		t.Fatalf("statuses = %v, err = %v", statuses, err)
	}
}

func TestWorkerDeliveryFailureUsesCanonicalFieldName(t *testing.T) {
	payload, err := json.Marshal(workerapi.DeploymentBuildDeliveryFailureRequest{
		ReasonCode: workerapi.DeploymentBuildDeliveryBuildGuestFailed,
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["reason_code"]) != `"build_guest_failed"` {
		t.Fatalf("payload = %s", payload)
	}
}
