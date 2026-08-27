package workerapi

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestRuntimeReconcileWorkspaceTargetWireContract(t *testing.T) {
	tests := []struct {
		name       string
		action     string
		target     *WorkspaceResetTarget
		wantTarget bool
	}{
		{
			name:   "prepare",
			action: RuntimeReconcilePrepare,
			target: &WorkspaceResetTarget{
				BaseWorkspaceVersionID: "019c10d5-a6f7-7af1-8f5f-000000000001",
				Tree: WorkspaceTreeIdentity{
					Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				},
				Empty: &EmptyWorkspace{},
			},
			wantTarget: true,
		},
		{name: "close", action: RuntimeReconcileClose},
		{name: "reclaim", action: RuntimeReconcileReclaim},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := RuntimeReconcileResponse{Items: []RuntimeReconcileTarget{{
				ID:     "019c10d5-a6f7-7af1-8f5f-000000000002",
				Action: test.action,
				Source: RuntimeSource{WorkspaceTarget: test.target},
			}}}
			raw, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			hasTarget := bytes.Contains(raw, []byte(`"workspace_target"`))
			if hasTarget != test.wantTarget {
				t.Fatalf("workspace_target presence = %t, want %t: %s", hasTarget, test.wantTarget, raw)
			}
			var decoded RuntimeReconcileResponse
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("decode produced %s: %v", raw, err)
			}
			if len(decoded.Items) != 1 || (decoded.Items[0].Source.WorkspaceTarget != nil) != test.wantTarget {
				t.Fatalf("decoded items = %#v", decoded.Items)
			}
		})
	}
}

func TestRuntimeReconcileRejectsPresentInvalidWorkspaceTarget(t *testing.T) {
	var response RuntimeReconcileResponse
	err := json.Unmarshal([]byte(`{
		"items": [{
			"id": "019c10d5-a6f7-7af1-8f5f-000000000002",
			"action": "prepare",
			"source": {"workspace_target": {}}
		}]
	}`), &response)
	if err == nil {
		t.Fatal("present invalid workspace_target was accepted")
	}
}
