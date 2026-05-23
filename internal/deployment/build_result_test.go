package deployment

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
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
