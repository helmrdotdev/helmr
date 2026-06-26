package guestd

import (
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/proto/run/v0"
)

func TestAdapterTaskContextJSON(t *testing.T) {
	request := &runv0.RunTaskRequest{
		RunId:     "run-1",
		TaskId:    "deploy",
		SessionId: "session-1",
		Workspace: &runv0.RunTaskWorkspace{
			Path:        "/workspace",
			ProjectPath: "/workspace/sdk",
		},
	}
	payload, err := adapterTaskContextJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["source"]; ok {
		t.Fatalf("source should be absent: %+v", decoded["source"])
	}
	workspace := decoded["workspace"].(map[string]any)
	if workspace["path"] != "/workspace" || workspace["projectPath"] != "/workspace/sdk" {
		t.Fatalf("workspace = %+v", workspace)
	}
	session := decoded["session"].(map[string]any)
	if session["id"] != "session-1" {
		t.Fatalf("session = %+v", session)
	}
	if _, ok := session["workspace"]; ok {
		t.Fatalf("session workspace should be absent: %+v", session)
	}
}
