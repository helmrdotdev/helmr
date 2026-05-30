package deployment

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	bundlev0 "github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
)

func TestValidateWorkerDeploymentBuildResultRequiresReportedArtifacts(t *testing.T) {
	result := api.WorkerDeploymentBuildResult{
		BuildManifestDigest:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DeploymentManifestDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Tasks: []api.WorkerDeploymentBuildTask{{
			TaskID:             "deploy",
			FilePath:           "src/task.ts",
			ExportName:         "deploy",
			HandlerEntrypoint:  "src/task.ts#deploy",
			BundleDigest:       "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			RequestedMilliCPU:  1000,
			RequestedMemoryMiB: 1024,
			QueueName:          "task/deploy",
			MaxDurationSeconds: 300,
		}},
		CASObjects: []api.CASObject{{
			Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SizeBytes: 1,
			MediaType: api.BuildManifestArtifactMediaType,
		}, {
			Digest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			SizeBytes: 1,
			MediaType: api.DeploymentManifestArtifactMediaType,
		}},
	}

	_, err := ValidateBuildResult(result)
	if err == nil || !strings.Contains(err.Error(), `task "deploy" bundle_digest must be included`) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeploymentTaskMaxDurationSecondsUsesBundleTask(t *testing.T) {
	value, err := deploymentTaskMaxDurationSeconds(&bundlev0.Bundle{
		Task: &bundlev0.TaskSpec{MaxDurationSeconds: 1800},
	})
	if err != nil {
		t.Fatal(err)
	}
	if value != 1800 {
		t.Fatalf("max duration = %d", value)
	}
}

func TestDeploymentTaskMaxDurationSecondsRequiresBundleTaskValue(t *testing.T) {
	_, err := deploymentTaskMaxDurationSeconds(&bundlev0.Bundle{
		Task: &bundlev0.TaskSpec{},
	})
	if err == nil || !strings.Contains(err.Error(), "max_duration_seconds is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeploymentTaskPayloadSchemaUsesBundleTask(t *testing.T) {
	schema := deploymentTaskPayloadSchema(&bundlev0.Bundle{
		Task: &bundlev0.TaskSpec{PayloadSchemaJson: `{"type":"object"}`},
	})
	if string(schema) != `{"type":"object"}` {
		t.Fatalf("payload schema = %s", schema)
	}
}

func TestValidateWorkerDeploymentBuildResultRejectsInvalidPayloadSchema(t *testing.T) {
	tests := []struct {
		name    string
		schema  []byte
		message string
	}{
		{name: "malformed", schema: []byte(`{"type":`), message: "must be valid JSON"},
		{name: "null", schema: []byte(`null`), message: "must be a JSON Schema object or boolean"},
		{name: "number", schema: []byte(`1`), message: "must be a JSON Schema object or boolean"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := validBuildResult()
			result.Tasks[0].PayloadSchema = tt.schema
			_, err := ValidateBuildResult(result)
			if err == nil || !strings.Contains(err.Error(), tt.message) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestValidateWorkerDeploymentBuildResultAcceptsBooleanPayloadSchema(t *testing.T) {
	result := validBuildResult()
	result.Tasks[0].PayloadSchema = []byte(`true`)
	if _, err := ValidateBuildResult(result); err != nil {
		t.Fatal(err)
	}
}

func TestValidateWorkerDeploymentBuildResultAcceptsDefaultQueueFromDottedTaskID(t *testing.T) {
	result := validBuildResult()
	result.Tasks[0].TaskID = "build.test"
	result.Tasks[0].QueueName = "task/build.test"
	if _, err := ValidateBuildResult(result); err != nil {
		t.Fatal(err)
	}
}

func TestValidateWorkerDeploymentBuildResultChecksMediaTypes(t *testing.T) {
	result := api.WorkerDeploymentBuildResult{
		BuildManifestDigest:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DeploymentManifestDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Tasks: []api.WorkerDeploymentBuildTask{{
			TaskID:             "deploy",
			FilePath:           "src/task.ts",
			ExportName:         "deploy",
			HandlerEntrypoint:  "src/task.ts#deploy",
			BundleDigest:       "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			RequestedMilliCPU:  1000,
			RequestedMemoryMiB: 1024,
			QueueName:          "task/deploy",
			MaxDurationSeconds: 300,
		}},
		CASObjects: []api.CASObject{{
			Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SizeBytes: 1,
			MediaType: api.BuildManifestArtifactMediaType,
		}, {
			Digest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			SizeBytes: 1,
			MediaType: api.DeploymentManifestArtifactMediaType,
		}, {
			Digest:    "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			SizeBytes: 1,
			MediaType: "application/octet-stream",
		}},
	}

	_, err := ValidateBuildResult(result)
	if err == nil || !strings.Contains(err.Error(), api.TaskBundleArtifactMediaType) {
		t.Fatalf("err = %v", err)
	}
}

func validBuildResultCASObjects() []api.CASObject {
	return []api.CASObject{{
		Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SizeBytes: 1,
		MediaType: api.BuildManifestArtifactMediaType,
	}, {
		Digest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		SizeBytes: 1,
		MediaType: api.DeploymentManifestArtifactMediaType,
	}, {
		Digest:    "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		SizeBytes: 1,
		MediaType: api.TaskBundleArtifactMediaType,
	}}
}

func validBuildResult() api.WorkerDeploymentBuildResult {
	return api.WorkerDeploymentBuildResult{
		BuildManifestDigest:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DeploymentManifestDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Tasks: []api.WorkerDeploymentBuildTask{{
			TaskID:             "deploy",
			FilePath:           "src/task.ts",
			ExportName:         "deploy",
			HandlerEntrypoint:  "src/task.ts#deploy",
			BundleDigest:       "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			RequestedMilliCPU:  1000,
			RequestedMemoryMiB: 1024,
			QueueName:          "task/deploy",
			MaxDurationSeconds: 300,
		}},
		CASObjects: validBuildResultCASObjects(),
	}
}

func TestValidateWorkerDeploymentBuildResultRejectsConflictingCASObjectMetadata(t *testing.T) {
	_, _, err := NormalizeBuildCASObjects([]api.CASObject{{
		Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SizeBytes: 1,
		MediaType: api.TaskBundleArtifactMediaType,
	}, {
		Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SizeBytes: 2,
		MediaType: api.TaskBundleArtifactMediaType,
	}})
	if err == nil || !strings.Contains(err.Error(), "conflicting metadata") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateWorkerDeploymentBuildResultRejectsInvalidCASObjectDigest(t *testing.T) {
	_, _, err := NormalizeBuildCASObjects([]api.CASObject{{
		Digest:    "sha256:bad",
		SizeBytes: 1,
		MediaType: api.TaskBundleArtifactMediaType,
	}})
	if err == nil || !strings.Contains(err.Error(), "digest is invalid") {
		t.Fatalf("err = %v", err)
	}
}
