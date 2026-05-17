package executor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestResolveRun(t *testing.T) {
	run := api.WorkerRun{
		ID:         "run-1",
		TaskID:     "deploy",
		Payload:    json.RawMessage(`{"env":"prod"}`),
		Secrets:    api.ResolvedSecrets{"API_KEY": []byte("secret")},
		TaskSource: api.TaskSourceArtifact{Digest: validTaskSource().Digest},
		Workspace: api.GitHubSource{
			Repository: "helmrdotdev/helmr",
			Ref:        "0123456789abcdef0123456789abcdef01234567",
			SHA:        "0123456789abcdef0123456789abcdef01234567",
			Subpath:    "packages/console",
		},
		MaxDurationSeconds: 30,
	}

	resolved, err := Resolve(run)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.TaskID != "deploy" {
		t.Fatalf("task id = %s", resolved.TaskID)
	}
	if string(resolved.Payload) != `{"env":"prod"}` {
		t.Fatalf("payload = %s", resolved.Payload)
	}
	if resolved.TaskSource.Digest != validTaskSource().Digest {
		t.Fatalf("task source = %+v", resolved.TaskSource)
	}
	if resolved.Workspace.Repository != "helmrdotdev/helmr" || resolved.Workspace.SHA == "" {
		t.Fatalf("workspace = %+v", resolved.Workspace)
	}
	if resolved.MaxDuration != 30*time.Second {
		t.Fatalf("max duration = %s", resolved.MaxDuration)
	}
}

func TestResolveDefaultsJSON(t *testing.T) {
	resolved, err := Resolve(validRun())
	if err != nil {
		t.Fatal(err)
	}
	if string(resolved.Payload) != `{}` || len(resolved.Secrets) != 0 {
		t.Fatalf("payload=%s secrets=%+v", resolved.Payload, resolved.Secrets)
	}
}

func TestResolveRestoreDoesNotRequireSources(t *testing.T) {
	_, err := Resolve(api.WorkerRun{
		ID:                 "run-1",
		TaskID:             "deploy",
		MaxDurationSeconds: 30,
		Restore:            &api.WorkerRestore{CheckpointID: "checkpoint-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestResolveRejectsInvalidRun(t *testing.T) {
	tests := map[string]api.WorkerRun{
		"missing id":        {TaskID: "deploy", TaskSource: validTaskSource(), Workspace: validSource(), MaxDurationSeconds: 30},
		"missing task":      {ID: "run-1", TaskSource: validTaskSource(), Workspace: validSource(), MaxDurationSeconds: 30},
		"bad payload":       {ID: "run-1", TaskID: "deploy", Payload: json.RawMessage(`{`), TaskSource: validTaskSource(), Workspace: validSource(), MaxDurationSeconds: 30},
		"missing task src":  {ID: "run-1", TaskID: "deploy", Workspace: validSource(), MaxDurationSeconds: 30},
		"missing workspace": {ID: "run-1", TaskID: "deploy", TaskSource: validTaskSource(), MaxDurationSeconds: 30},
		"bad task digest":   {ID: "run-1", TaskID: "deploy", TaskSource: api.TaskSourceArtifact{Digest: "sha256:bad"}, Workspace: validSource(), MaxDurationSeconds: 30},
		"missing duration":  {ID: "run-1", TaskID: "deploy", TaskSource: validTaskSource(), Workspace: validSource()},
		"negative active":   {ID: "run-1", TaskID: "deploy", TaskSource: validTaskSource(), Workspace: validSource(), MaxDurationSeconds: 30, ActiveDurationMs: -1},
		"huge active":       {ID: "run-1", TaskID: "deploy", TaskSource: validTaskSource(), Workspace: validSource(), MaxDurationSeconds: 30, ActiveDurationMs: maxActiveDurationMilliseconds + 1},
	}

	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Resolve(run)
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.TrimSpace(err.Error()) == "" {
				t.Fatal("empty error")
			}
		})
	}
}

func validRun() api.WorkerRun {
	return api.WorkerRun{
		ID:                 "run-1",
		TaskID:             "deploy",
		TaskSource:         validTaskSource(),
		Workspace:          validSource(),
		MaxDurationSeconds: 30,
	}
}

func validTaskSource() api.TaskSourceArtifact {
	return api.TaskSourceArtifact{
		Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		MediaType: api.TaskSourceArtifactMediaType,
	}
}

func validSource() api.GitHubSource {
	return api.GitHubSource{
		Repository: "helmrdotdev/helmr",
		Ref:        "0123456789abcdef0123456789abcdef01234567",
		SHA:        "0123456789abcdef0123456789abcdef01234567",
	}
}
