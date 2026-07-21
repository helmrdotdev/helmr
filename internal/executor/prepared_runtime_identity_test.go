package executor

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/runtime"
)

const goldenPreparedRuntimeKey = `{"runtime_id":"runtime-1","deployment_sandbox_id":"definition-1","image_digest":"sha256:image","image_format":"oci-tar","workspace_mount_path":"/workspace","sandbox_artifact_digest":"sha256:sandbox","sandbox_artifact_format":"oci-tar","substrate_key":"sha256:2bd820891796d3768c8b4b247c46882b9a86fd9be95dd321881985e62894fb73","network":{"internet":false,"deny":["10.0.0.0/8"]}}`
const goldenPreparedRuntimeKeyZeroNetwork = `{"runtime_id":"runtime-1","deployment_sandbox_id":"definition-1","image_digest":"sha256:image","image_format":"oci-tar","workspace_mount_path":"/workspace","sandbox_artifact_digest":"sha256:sandbox","sandbox_artifact_format":"oci-tar","substrate_key":"sha256:2bd820891796d3768c8b4b247c46882b9a86fd9be95dd321881985e62894fb73","network":{"internet":false}}`
const goldenPreparedRuntimeKeyInvalidSubstrate = `{"runtime_id":"runtime-1","deployment_sandbox_id":"definition-1","image_digest":"sha256:image","image_format":"oci-tar","workspace_mount_path":"/workspace","sandbox_artifact_digest":"","sandbox_artifact_format":"oci-tar","substrate_key":"","network":{"internet":false,"deny":["10.0.0.0/8"]}}`

func TestPreparedRuntimeKeyFromWorkspaceMountMatchesGolden(t *testing.T) {
	key := preparedRuntimeKeyFromWorkspaceMount(goldenWorkspaceMount(), compute.NetworkPolicy{Internet: false, Deny: []string{"10.0.0.0/8"}})
	if key != goldenPreparedRuntimeKey {
		t.Fatalf("key = %s, want %s", key, goldenPreparedRuntimeKey)
	}
	if got := runtime.Hash(key); got != "3e35a24c372709d97c3fedcfd5d2928392469df79fbcdec983b60c943aa83a04" {
		t.Fatalf("hash = %s", got)
	}
}

func TestPreparedRuntimeKeyFromWorkspaceMountMatchesZeroNetworkGolden(t *testing.T) {
	key := preparedRuntimeKeyFromWorkspaceMount(goldenWorkspaceMount(), compute.NetworkPolicy{})
	if key != goldenPreparedRuntimeKeyZeroNetwork {
		t.Fatalf("key = %s, want %s", key, goldenPreparedRuntimeKeyZeroNetwork)
	}
	if got := runtime.Hash(key); got != "640060f7cd8f388ca20e888207e7e3eaa8f17da34798bfd748683b609cdbcaf1" {
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
	if got := runtime.Hash(key); got != "2e43a3a198c953b202ec08204c9550d54cd9a2fdae5bb5ac04bb1c54db94ee02" {
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
		DeploymentDefinitionID:     "definition-1",
		ImageDigest:                "sha256:image",
		ImageFormat:                "oci-tar",
		RootfsDigest:               "sha256:rootfs",
		RuntimeABI:                 "runtime-abi",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: "sha256:sandbox", SizeBytes: 1, MediaType: api.SandboxImageArtifactMediaType},
		SandboxImageArtifactFormat: "oci-tar",
	}
}
