package guestd

import (
	"encoding/json"
	"strings"
	"testing"

	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
)

func TestAdapterTaskContextJSON(t *testing.T) {
	refKind := "branch"
	request := &runv0.RunTaskRequest{
		RunId:  "run-1",
		TaskId: "deploy",
		Source: &runv0.RunTaskSource{
			Kind: &runv0.RunTaskSource_Github{
				Github: &runv0.RunTaskGitHubSource{
					Repository:   "helmrdotdev/helmr",
					RequestedRef: "main",
					ResolvedSha:  "0123456789abcdef0123456789abcdef01234567",
					RefKind:      &refKind,
				},
			},
		},
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
	source := decoded["source"].(map[string]any)
	if source["kind"] != "github" || source["refKind"] != "branch" {
		t.Fatalf("source = %+v", source)
	}
	workspace := decoded["workspace"].(map[string]any)
	if workspace["path"] != "/workspace" || workspace["projectPath"] != "/workspace/sdk" {
		t.Fatalf("workspace = %+v", workspace)
	}
}

func TestAdapterTaskContextJSONRequiresGitHubSource(t *testing.T) {
	_, err := adapterTaskContextJSON(&runv0.RunTaskRequest{
		RunId:     "run-1",
		TaskId:    "deploy",
		Workspace: &runv0.RunTaskWorkspace{Path: "/workspace", ProjectPath: "/workspace"},
	})
	if err == nil || !strings.Contains(err.Error(), "task context source is required") {
		t.Fatalf("err = %v", err)
	}
}
