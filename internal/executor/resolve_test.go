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
		ID:                 "run-1",
		TaskID:             "deploy",
		Payload:            json.RawMessage(`{"env":"prod"}`),
		Secrets:            api.ResolvedSecrets{"API_KEY": []byte("secret")},
		DeploymentSource:   api.DeploymentSourceArtifact{Digest: validDeploymentSource().Digest},
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
	if resolved.DeploymentSource.Digest != validDeploymentSource().Digest {
		t.Fatalf("deployment source = %+v", resolved.DeploymentSource)
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
		"missing id":       {TaskID: "deploy", DeploymentSource: validDeploymentSource(), MaxDurationSeconds: 30},
		"missing task":     {ID: "run-1", DeploymentSource: validDeploymentSource(), MaxDurationSeconds: 30},
		"bad payload":      {ID: "run-1", TaskID: "deploy", Payload: json.RawMessage(`{`), DeploymentSource: validDeploymentSource(), MaxDurationSeconds: 30},
		"missing task src": {ID: "run-1", TaskID: "deploy", MaxDurationSeconds: 30},
		"bad task digest":  {ID: "run-1", TaskID: "deploy", DeploymentSource: api.DeploymentSourceArtifact{Digest: "sha256:bad"}, MaxDurationSeconds: 30},
		"missing duration": {ID: "run-1", TaskID: "deploy", DeploymentSource: validDeploymentSource()},
		"negative active":  {ID: "run-1", TaskID: "deploy", DeploymentSource: validDeploymentSource(), MaxDurationSeconds: 30, ActiveDurationMs: -1},
		"huge active":      {ID: "run-1", TaskID: "deploy", DeploymentSource: validDeploymentSource(), MaxDurationSeconds: 30, ActiveDurationMs: maxActiveDurationMilliseconds + 1},
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
		DeploymentSource:   validDeploymentSource(),
		DeploymentTask:     api.WorkerDeploymentTask{ID: "task-1", FilePath: "src/task.ts", ExportName: "deploy", BundleDigest: validTaskBundleDigest()},
		MaxDurationSeconds: 30,
	}
}

func validDeploymentSource() api.DeploymentSourceArtifact {
	return api.DeploymentSourceArtifact{
		Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		MediaType: api.DeploymentSourceArtifactMediaType,
	}
}

func validTaskBundleDigest() string {
	return "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
}
