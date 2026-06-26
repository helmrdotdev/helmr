package deployment

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
)

func TestValidateWorkerDeploymentBuildResultRequiresReportedArtifacts(t *testing.T) {
	result := api.WorkerDeploymentBuildResult{
		BuildManifestDigest:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DeploymentManifestDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Tasks: []api.WorkerDeploymentBuildTask{{
			TaskID:                     "deploy",
			SandboxID:                  "default",
			SandboxFingerprint:         "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
			SandboxImageArtifact:       api.CASObject{Digest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", SizeBytes: 1, MediaType: api.SandboxImageArtifactMediaType},
			SandboxImageArtifactFormat: "oci-tar",
			SandboxImageDigest:         "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
			SandboxImageFormat:         "oci-tar",
			WorkspaceMountPath:         "/workspace",
			FilesystemFormat:           "tar",
			FilePath:                   "src/task.ts",
			ExportName:                 "deploy",
			HandlerEntrypoint:          "src/task.ts#deploy",
			BundleDigest:               "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			RequestedMilliCPU:          1000,
			RequestedMemoryMiB:         1024,
			QueueName:                  "task/deploy",
			MaxDurationSeconds:         300,
		}},
		Queues: []api.WorkerDeploymentQueue{{Name: "task/deploy"}},
		CASObjects: []api.CASObject{{
			Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SizeBytes: 1,
			MediaType: api.BuildManifestArtifactMediaType,
		}, {
			Digest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			SizeBytes: 1,
			MediaType: api.DeploymentManifestArtifactMediaType,
		}, {
			Digest:    "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
			SizeBytes: 1,
			MediaType: api.SandboxImageArtifactMediaType,
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

func TestDeploymentTaskRetryPolicyReadsBundleTask(t *testing.T) {
	retryPolicy := deploymentTaskRetryPolicy(&bundlev0.Bundle{
		Task: &bundlev0.TaskSpec{RetryPolicyJson: `{"maxAttempts":3,"backoff":{"minMs":1000,"maxMs":30000,"jitter":"full"}}`},
	})
	if string(retryPolicy) != `{"maxAttempts":3,"backoff":{"minMs":1000,"maxMs":30000,"jitter":"full"}}` {
		t.Fatalf("retry policy = %s", retryPolicy)
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

func TestDeploymentTaskNetworkReadsDenyCIDRs(t *testing.T) {
	network, err := deploymentTaskNetwork(&bundlev0.Bundle{
		Sandbox: &bundlev0.SandboxSpec{
			Network: &bundlev0.NetworkPolicy{
				Internet: true,
				Deny:     []string{"10.0.0.0/8"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !network.Internet || len(network.Deny) != 1 || network.Deny[0] != "10.0.0.0/8" {
		t.Fatalf("network = %+v", network)
	}
}

func TestSandboxContractFingerprintIgnoresNonContractBundleFields(t *testing.T) {
	base := sandboxFingerprintTestBundle()
	first, err := sandboxContractFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Task = &bundlev0.TaskSpec{Id: "renamed-task", MaxDurationSeconds: 900}
	base.Image = &bundlev0.ImageSpec{FormatVersion: 99}
	second, err := sandboxContractFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("non-contract bundle fields changed fingerprint: %s != %s", second, first)
	}
}

func TestSandboxContractFingerprintChangesForContractFields(t *testing.T) {
	base := sandboxFingerprintTestBundle()
	first, err := sandboxContractFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Sandbox.Workspace.MountPath = "/workspace/project"
	second, err := sandboxContractFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatalf("workspace mount path did not change fingerprint: %s", second)
	}
}

func TestSandboxContractFingerprintCanonicalizesNetworkLists(t *testing.T) {
	firstBundle := sandboxFingerprintTestBundle()
	firstBundle.Sandbox.Network.Allow = []string{"b.example", "a.example"}
	firstBundle.Sandbox.Network.Deny = []string{"10.0.0.0/8", "192.168.0.0/16"}
	secondBundle := sandboxFingerprintTestBundle()
	secondBundle.Sandbox.Network.Allow = []string{"a.example", "b.example"}
	secondBundle.Sandbox.Network.Deny = []string{"192.168.0.0/16", "10.0.0.0/8"}
	first, err := sandboxContractFingerprint(firstBundle)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sandboxContractFingerprint(secondBundle)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("network set ordering changed fingerprint: %s != %s", second, first)
	}
}

func TestSandboxContractFingerprintIgnoresResourceFloors(t *testing.T) {
	base := sandboxFingerprintTestBundle()
	first, err := sandboxContractFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Sandbox.Resources.Cpu = 8
	base.Sandbox.Resources.Memory = "16Gi"
	base.Sandbox.Resources.Disk = "128Gi"
	second, err := sandboxContractFingerprint(base)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("resource floors changed fingerprint: %s != %s", second, first)
	}
}

func sandboxFingerprintTestBundle() *bundlev0.Bundle {
	return &bundlev0.Bundle{
		Sandbox: &bundlev0.SandboxSpec{
			Id: "default",
			Workspace: &bundlev0.WorkspaceRuntimeBinding{
				MountPath: "/workspace",
			},
			Resources: &bundlev0.Resources{
				Cpu:    2,
				Memory: "4Gi",
				Disk:   "32Gi",
			},
			Network: &bundlev0.NetworkPolicy{
				Internet: true,
			},
		},
		Task: &bundlev0.TaskSpec{Id: "task", MaxDurationSeconds: 300},
	}
}

func TestDeploymentTaskNetworkRejectsAllowRules(t *testing.T) {
	_, err := deploymentTaskNetwork(&bundlev0.Bundle{
		Sandbox: &bundlev0.SandboxSpec{
			Network: &bundlev0.NetworkPolicy{
				Internet: true,
				Allow:    []string{"203.0.113.0/24"},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "network allow rules are not supported yet") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateWorkerDeploymentBuildResultAcceptsDefaultQueueFromDottedTaskID(t *testing.T) {
	result := validBuildResult()
	result.Tasks[0].TaskID = "build.test"
	result.Tasks[0].QueueName = "task/build.test"
	result.Queues[0].Name = "task/build.test"
	if _, err := ValidateBuildResult(result); err != nil {
		t.Fatal(err)
	}
}

func TestValidateWorkerDeploymentBuildResultAcceptsSharedSandboxDefinition(t *testing.T) {
	result := validBuildResult()
	second := result.Tasks[0]
	second.TaskID = "deploy.more"
	second.FilePath = "src/more.ts"
	second.HandlerEntrypoint = "src/more.ts#deploy"
	second.QueueName = "task/deploy.more"
	result.Tasks = append(result.Tasks, second)
	result.Queues = append(result.Queues, api.WorkerDeploymentQueue{Name: "task/deploy.more"})

	if _, err := ValidateBuildResult(result); err != nil {
		t.Fatal(err)
	}
}

func TestValidateWorkerDeploymentBuildResultRejectsConflictingSandboxDefinition(t *testing.T) {
	result := validBuildResult()
	second := result.Tasks[0]
	second.TaskID = "deploy.more"
	second.FilePath = "src/more.ts"
	second.HandlerEntrypoint = "src/more.ts#deploy"
	second.QueueName = "task/deploy.more"
	second.SandboxFingerprint = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	result.Tasks = append(result.Tasks, second)
	result.Queues = append(result.Queues, api.WorkerDeploymentQueue{Name: "task/deploy.more"})

	_, err := ValidateBuildResult(result)
	if err == nil || !strings.Contains(err.Error(), `sandbox_id "default" has conflicting definitions`) {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateWorkerDeploymentBuildResultRejectsUnsupportedBundleFormat(t *testing.T) {
	result := validBuildResult()
	result.Tasks[0].BundleFormatVersion = 99
	_, err := ValidateBuildResult(result)
	if err == nil || !strings.Contains(err.Error(), "bundle_format_version 99 is not supported") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateWorkerDeploymentBuildResultRejectsZeroConcurrencyLimit(t *testing.T) {
	result := validBuildResult()
	limit := int32(0)
	result.Queues[0].ConcurrencyLimit = &limit
	_, err := ValidateBuildResult(result)
	if err == nil || !strings.Contains(err.Error(), "concurrency_limit must be positive") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateWorkerDeploymentBuildResultRejectsDuplicateQueue(t *testing.T) {
	result := validBuildResult()
	result.Queues = append(result.Queues, result.Queues[0])
	_, err := ValidateBuildResult(result)
	if err == nil || !strings.Contains(err.Error(), `duplicate queue "task/deploy"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateWorkerDeploymentBuildResultRequiresQueueCatalog(t *testing.T) {
	result := validBuildResult()
	result.Queues = nil
	_, err := ValidateBuildResult(result)
	if err == nil || !strings.Contains(err.Error(), "deployment build must include queue catalog") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateWorkerDeploymentBuildResultRejectsUndefinedTaskQueue(t *testing.T) {
	result := validBuildResult()
	result.Tasks[0].QueueName = "review/pr"
	_, err := ValidateBuildResult(result)
	if err == nil || !strings.Contains(err.Error(), `task "deploy" references undefined queue "review/pr"`) {
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

func TestValidateWorkerDeploymentBuildResultValidatesTaskSecrets(t *testing.T) {
	t.Run("accepts one placement per secret", func(t *testing.T) {
		result := validBuildResult()
		result.Tasks[0].Secrets = []api.SecretDeclaration{
			{Name: "API_TOKEN", Env: "API_TOKEN"},
			{Name: "ssh-key", File: "/run/secrets/ssh_key", Mode: "0400", Owner: "1000:1000"},
			{Name: "certs", Dir: "/run/secrets/certs", Mode: "0700"},
		}
		if _, err := ValidateBuildResult(result); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("rejects duplicate names", func(t *testing.T) {
		result := validBuildResult()
		result.Tasks[0].Secrets = []api.SecretDeclaration{
			{Name: "API_TOKEN", Env: "API_TOKEN"},
			{Name: "API_TOKEN", File: "/run/secrets/token"},
		}
		_, err := ValidateBuildResult(result)
		if err == nil || !strings.Contains(err.Error(), `duplicate secret "API_TOKEN"`) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("rejects missing placement", func(t *testing.T) {
		result := validBuildResult()
		result.Tasks[0].Secrets = []api.SecretDeclaration{{Name: "API_TOKEN"}}
		_, err := ValidateBuildResult(result)
		if err == nil || !strings.Contains(err.Error(), `must declare exactly one placement`) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("rejects multiple placements", func(t *testing.T) {
		result := validBuildResult()
		result.Tasks[0].Secrets = []api.SecretDeclaration{{Name: "API_TOKEN", Env: "API_TOKEN", File: "/run/secrets/token"}}
		_, err := ValidateBuildResult(result)
		if err == nil || !strings.Contains(err.Error(), `must declare exactly one placement`) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestDeploymentTaskSecretsMapsBundlePlacements(t *testing.T) {
	mode := "0400"
	owner := "1000:1000"
	secrets := deploymentTaskSecrets(&bundlev0.Bundle{
		Task: &bundlev0.TaskSpec{
			Secrets: []*bundlev0.SecretPlacement{
				{
					Name: "API_TOKEN",
					Placement: &bundlev0.Placement{
						Kind: &bundlev0.Placement_Env{Env: &bundlev0.EnvPlacement{Name: "API_TOKEN"}},
					},
				},
				{
					Name: "ssh-key",
					Placement: &bundlev0.Placement{
						Kind: &bundlev0.Placement_File{File: &bundlev0.FilePlacement{Path: "/run/secrets/ssh_key", Mode: &mode, Owner: &owner}},
					},
				},
				{
					Name: "certs",
					Placement: &bundlev0.Placement{
						Kind: &bundlev0.Placement_Dir{Dir: &bundlev0.DirPlacement{Path: "/run/secrets/certs", Mode: &mode}},
					},
				},
			},
		},
	})
	if len(secrets) != 3 {
		t.Fatalf("secrets = %+v", secrets)
	}
	if secrets[0] != (api.SecretDeclaration{Name: "API_TOKEN", Env: "API_TOKEN"}) {
		t.Fatalf("env secret = %+v", secrets[0])
	}
	if secrets[1] != (api.SecretDeclaration{Name: "ssh-key", File: "/run/secrets/ssh_key", Mode: mode, Owner: owner}) {
		t.Fatalf("file secret = %+v", secrets[1])
	}
	if secrets[2] != (api.SecretDeclaration{Name: "certs", Dir: "/run/secrets/certs", Mode: mode}) {
		t.Fatalf("dir secret = %+v", secrets[2])
	}
}

func TestValidateWorkerDeploymentBuildResultValidatesDeploymentStreams(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		result := validBuildResult()
		result.Streams = []api.WorkerDeploymentStream{
			{Name: "approval", Direction: "input", SchemaFingerprint: "sha256:schema", SchemaJSON: []byte(`{"kind":"standard-schema-v1"}`)},
			{Name: "events", Direction: "output", SchemaJSON: []byte(`null`)},
		}
		if _, err := ValidateBuildResult(result); err != nil {
			t.Fatalf("ValidateBuildResult() error = %v", err)
		}
	})

	t.Run("invalid direction", func(t *testing.T) {
		result := validBuildResult()
		result.Streams = []api.WorkerDeploymentStream{{Name: "approval", Direction: "sideways", SchemaJSON: []byte(`null`)}}
		_, err := ValidateBuildResult(result)
		if err == nil || !strings.Contains(err.Error(), "direction must be input or output") {
			t.Fatalf("ValidateBuildResult() error = %v", err)
		}
	})

	t.Run("duplicate", func(t *testing.T) {
		result := validBuildResult()
		result.Streams = []api.WorkerDeploymentStream{
			{Name: "approval", Direction: "input", SchemaJSON: []byte(`null`)},
			{Name: "approval", Direction: "input", SchemaJSON: []byte(`null`)},
		}
		_, err := ValidateBuildResult(result)
		if err == nil || !strings.Contains(err.Error(), "duplicate input stream") {
			t.Fatalf("ValidateBuildResult() error = %v", err)
		}
	})

	t.Run("invalid schema json", func(t *testing.T) {
		result := validBuildResult()
		result.Streams = []api.WorkerDeploymentStream{{Name: "approval", Direction: "input", SchemaJSON: []byte(`{`)}}
		_, err := ValidateBuildResult(result)
		if err == nil || !strings.Contains(err.Error(), "schema_json must be valid JSON") {
			t.Fatalf("ValidateBuildResult() error = %v", err)
		}
	})
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
			TaskID:                     "deploy",
			SandboxID:                  "default",
			SandboxFingerprint:         "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
			SandboxImageArtifact:       api.CASObject{Digest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", SizeBytes: 1, MediaType: api.SandboxImageArtifactMediaType},
			SandboxImageArtifactFormat: "oci-tar",
			SandboxImageDigest:         "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
			SandboxImageFormat:         "oci-tar",
			WorkspaceMountPath:         "/workspace",
			FilesystemFormat:           "tar",
			FilePath:                   "src/task.ts",
			ExportName:                 "deploy",
			HandlerEntrypoint:          "src/task.ts#deploy",
			BundleDigest:               "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			RequestedMilliCPU:          1000,
			RequestedMemoryMiB:         1024,
			QueueName:                  "task/deploy",
			MaxDurationSeconds:         300,
		}},
		Queues: []api.WorkerDeploymentQueue{{Name: "task/deploy"}},
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
		}, {
			Digest:    "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
			SizeBytes: 1,
			MediaType: api.SandboxImageArtifactMediaType,
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
	}, {
		Digest:    "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		SizeBytes: 1,
		MediaType: api.SandboxImageArtifactMediaType,
	}}
}

func validBuildResult() api.WorkerDeploymentBuildResult {
	return api.WorkerDeploymentBuildResult{
		BuildManifestDigest:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DeploymentManifestDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Tasks: []api.WorkerDeploymentBuildTask{{
			TaskID:                     "deploy",
			SandboxID:                  "default",
			SandboxFingerprint:         "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
			SandboxImageArtifact:       api.CASObject{Digest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", SizeBytes: 1, MediaType: api.SandboxImageArtifactMediaType},
			SandboxImageArtifactFormat: "oci-tar",
			SandboxImageDigest:         "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
			SandboxImageFormat:         "oci-tar",
			WorkspaceMountPath:         "/workspace",
			FilesystemFormat:           "tar",
			FilePath:                   "src/task.ts",
			ExportName:                 "deploy",
			HandlerEntrypoint:          "src/task.ts#deploy",
			BundleDigest:               "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			RequestedMilliCPU:          1000,
			RequestedMemoryMiB:         1024,
			QueueName:                  "task/deploy",
			MaxDurationSeconds:         300,
		}},
		Queues:     []api.WorkerDeploymentQueue{{Name: "task/deploy"}},
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
