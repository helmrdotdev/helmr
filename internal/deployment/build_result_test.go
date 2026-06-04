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

func TestDeploymentTaskResourcesReadsDisk(t *testing.T) {
	resources, err := deploymentTaskResources(&bundlev0.Bundle{
		Sandbox: &bundlev0.SandboxSpec{
			Resources: &bundlev0.Resources{
				Cpu:    2,
				Memory: "4Gi",
				Disk:   "32Gi",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resources.MilliCPU != 2000 || resources.MemoryMiB != 4096 || resources.DiskMiB != 32768 {
		t.Fatalf("resources = %+v", resources)
	}
}

func TestDeploymentTaskResourcesRejectsInvalidDisk(t *testing.T) {
	_, err := deploymentTaskResources(&bundlev0.Bundle{
		Sandbox: &bundlev0.SandboxSpec{
			Resources: &bundlev0.Resources{Disk: "half"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), `disk "half" must use MiB or GiB units`) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeploymentTaskResourcesRejectsOversizedDisk(t *testing.T) {
	_, err := deploymentTaskResources(&bundlev0.Bundle{
		Sandbox: &bundlev0.SandboxSpec{
			Resources: &bundlev0.Resources{Disk: "2147483648Mi"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), `disk "2147483648Mi" exceeds max 2147483647 MiB`) {
		t.Fatalf("err = %v", err)
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

func TestValidateWorkerDeploymentBuildResultRejectsZeroConcurrencyLimit(t *testing.T) {
	result := validBuildResult()
	limit := int32(0)
	result.Tasks[0].ConcurrencyLimit = &limit
	_, err := ValidateBuildResult(result)
	if err == nil || !strings.Contains(err.Error(), "concurrency_limit must be positive") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateWorkerDeploymentBuildResultRejectsOversizedDisk(t *testing.T) {
	result := validBuildResult()
	result.Tasks[0].RequestedDiskMiB = 2147483648
	_, err := ValidateBuildResult(result)
	if err == nil || !strings.Contains(err.Error(), "requested_disk_mib exceeds max 2147483647") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateWorkerDeploymentBuildResultRejectsInvalidTTL(t *testing.T) {
	result := validBuildResult()
	result.Tasks[0].TTL = "10minutes"
	_, err := ValidateBuildResult(result)
	if err == nil || !strings.Contains(err.Error(), "ttl must be a positive duration") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateWorkerDeploymentBuildResultAcceptsDeclarativeSchedule(t *testing.T) {
	result := validBuildResult()
	active := false
	result.Tasks[0].Schedules = []api.WorkerDeploymentTaskSchedule{{
		ID:       "nightly",
		Cron:     "0 2 * * *",
		Timezone: "Asia/Tokyo",
		Active:   &active,
	}}
	if _, err := ValidateBuildResult(result); err != nil {
		t.Fatal(err)
	}
}

func TestValidateWorkerDeploymentBuildResultRejectsInvalidDeclarativeSchedule(t *testing.T) {
	result := validBuildResult()
	result.Tasks[0].Schedules = []api.WorkerDeploymentTaskSchedule{{
		ID:   "bad",
		Cron: "not cron",
	}}
	_, err := ValidateBuildResult(result)
	if err == nil || !strings.Contains(err.Error(), "valid 5-field expression") {
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
