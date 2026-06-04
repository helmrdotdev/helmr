package executor

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/checkout"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func TestTaskContextJSON(t *testing.T) {
	workspaceProto, err := runTaskWorkspaceProto("/workspace", checkout.WorkspaceArtifact{
		Digest:     "sha256:workspace",
		MediaType:  workspace.ArtifactMediaType,
		Encoding:   workspace.ArtifactEncoding,
		VolumeKind: workspace.VolumeKind,
		SizeBytes:  1024,
		EntryCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	jsonPayload, err := taskContextJSON("run-1", "deploy", workspaceProto)
	if err != nil {
		t.Fatal(err)
	}
	if !containsAll(jsonPayload,
		`"id":"run-1"`,
		`"id":"deploy"`,
		`"path":"/workspace"`,
		`"projectPath":"/workspace"`,
	) {
		t.Fatalf("json = %s", jsonPayload)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !contains(value, part) {
			return false
		}
	}
	return true
}

func contains(value, part string) bool {
	return len(part) == 0 || (len(value) >= len(part) && indexOf(value, part) >= 0)
}

func indexOf(value, part string) int {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return i
		}
	}
	return -1
}
