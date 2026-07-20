package executor

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/runtime"
)

const goldenPreparedRuntimeKey = `{"runtime_id":"runtime-1","deployment_sandbox_id":"sandbox-1","image_digest":"sha256:image","image_format":"oci-tar","workspace_mount_path":"/workspace","sandbox_artifact_digest":"sha256:sandbox","sandbox_artifact_format":"oci-tar","substrate_key":"sha256:2bd820891796d3768c8b4b247c46882b9a86fd9be95dd321881985e62894fb73","network":{"internet":false,"deny":["10.0.0.0/8"]}}`
const goldenPreparedRuntimeKeyZeroNetwork = `{"runtime_id":"runtime-1","deployment_sandbox_id":"sandbox-1","image_digest":"sha256:image","image_format":"oci-tar","workspace_mount_path":"/workspace","sandbox_artifact_digest":"sha256:sandbox","sandbox_artifact_format":"oci-tar","substrate_key":"sha256:2bd820891796d3768c8b4b247c46882b9a86fd9be95dd321881985e62894fb73","network":{"internet":false}}`
const goldenPreparedRuntimeKeyInvalidSubstrate = `{"runtime_id":"runtime-1","deployment_sandbox_id":"sandbox-1","image_digest":"sha256:image","image_format":"oci-tar","workspace_mount_path":"/workspace","sandbox_artifact_digest":"","sandbox_artifact_format":"oci-tar","substrate_key":"","network":{"internet":false,"deny":["10.0.0.0/8"]}}`

func TestPreparedRuntimeKeyFromWorkspaceMountMatchesGolden(t *testing.T) {
	key := preparedRuntimeKeyFromWorkspaceMount(goldenWorkspaceMount(), compute.NetworkPolicy{Internet: false, Deny: []string{"10.0.0.0/8"}})
	if key != goldenPreparedRuntimeKey {
		t.Fatalf("key = %s, want %s", key, goldenPreparedRuntimeKey)
	}
	if got := runtime.Hash(key); got != "133b7ed15d4038ace194880d86e31aecd530353974869882a289a922b6c4c22e" {
		t.Fatalf("hash = %s", got)
	}
}

func TestPreparedRuntimeKeyFromWorkspaceMountMatchesZeroNetworkGolden(t *testing.T) {
	key := preparedRuntimeKeyFromWorkspaceMount(goldenWorkspaceMount(), compute.NetworkPolicy{})
	if key != goldenPreparedRuntimeKeyZeroNetwork {
		t.Fatalf("key = %s, want %s", key, goldenPreparedRuntimeKeyZeroNetwork)
	}
	if got := runtime.Hash(key); got != "c13484739f83886eb827250efd1b8482aa9cc3d0ede85396273103cc3296b7b6" {
		t.Fatalf("hash = %s", got)
	}
}

func TestPreparedRuntimeKeyFromWorkspaceMountSwallowsSubstrateKeyError(t *testing.T) {
	mount := goldenWorkspaceMount()
	mount.SandboxImageArtifact.Digest = ""
	key := preparedRuntimeKeyFromWorkspaceMount(mount, compute.NetworkPolicy{Internet: false, Deny: []string{"10.0.0.0/8"}})
	if key != goldenPreparedRuntimeKeyInvalidSubstrate {
		t.Fatalf("key = %s, want %s", key, goldenPreparedRuntimeKeyInvalidSubstrate)
	}
	if got := runtime.Hash(key); got != "7f8e78f6a488c87cb0bbc787b167e5531bdadc1f25897cc92f1a92adc17e36f5" {
		t.Fatalf("hash = %s", got)
	}
}

func TestPreparedRuntimeSeparatesPhysicalAndWorkspaceImageIdentity(t *testing.T) {
	mount := goldenWorkspaceMount()
	substrateKey := preparedRuntimeSubstrateCacheKey(mount)
	runtimeKey := preparedRuntimeKeyFromWorkspaceMount(mount, compute.NetworkPolicy{})

	mount.RootfsDigest = "sha256:other-rootfs"
	mount.RuntimeABI = "other-runtime-abi"
	if got := preparedRuntimeSubstrateCacheKey(mount); got != substrateKey {
		t.Fatalf("physical profile changed workspace substrate key: %s != %s", got, substrateKey)
	}
	if got := preparedRuntimeKeyFromWorkspaceMount(mount, compute.NetworkPolicy{}); got != runtimeKey {
		t.Fatalf("redundant physical profile fields changed prepared runtime key: %s != %s", got, runtimeKey)
	}

	mount.RuntimeID = "runtime-2"
	if got := preparedRuntimeKeyFromWorkspaceMount(mount, compute.NetworkPolicy{}); got == runtimeKey {
		t.Fatal("certified runtime identity did not change prepared runtime key")
	}
}

func goldenWorkspaceMount() api.WorkerWorkspaceMount {
	return api.WorkerWorkspaceMount{
		RuntimeID:                  "runtime-1",
		DeploymentSandboxID:        "sandbox-1",
		ImageDigest:                "sha256:image",
		ImageFormat:                "oci-tar",
		RootfsDigest:               "sha256:rootfs",
		RuntimeABI:                 "runtime-abi",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: "sha256:sandbox", SizeBytes: 1, MediaType: api.SandboxImageArtifactMediaType},
		SandboxImageArtifactFormat: "oci-tar",
	}
}
