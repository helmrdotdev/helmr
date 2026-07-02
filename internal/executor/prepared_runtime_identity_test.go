package executor

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/runtime"
)

const goldenPreparedRuntimeKey = `{"runtime_id":"runtime-1","deployment_sandbox_id":"sandbox-1","image_digest":"sha256:image","image_format":"oci-tar","rootfs_digest":"sha256:rootfs","runtime_abi":"runtime-abi","guestd_abi":"guestd-abi","adapter_abi":"adapter-abi","workspace_mount_path":"/workspace","sandbox_artifact_digest":"sha256:sandbox","sandbox_artifact_format":"oci-tar","substrate_key":"sha256:9d06f1ce620cdfa34be30058524cfb49331aeb7451524c5358c4154a2bfb381c","network":{"internet":false,"deny":["10.0.0.0/8"]}}`

func TestPreparedRuntimeKeyFromWorkspaceMountMatchesGolden(t *testing.T) {
	key := preparedRuntimeKeyFromWorkspaceMount(api.WorkerWorkspaceMount{
		RuntimeID:                  "runtime-1",
		DeploymentSandboxID:        "sandbox-1",
		ImageDigest:                "sha256:image",
		ImageFormat:                "oci-tar",
		RootfsDigest:               "sha256:rootfs",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: "sha256:sandbox", SizeBytes: 1, MediaType: api.SandboxImageArtifactMediaType},
		SandboxImageArtifactFormat: "oci-tar",
	}, compute.NetworkPolicy{Internet: false, Deny: []string{"10.0.0.0/8"}})
	if key != goldenPreparedRuntimeKey {
		t.Fatalf("key = %s, want %s", key, goldenPreparedRuntimeKey)
	}
	if got := runtime.Hash(key); got != "46b507b394ef59614c5991d6952196851dd6e95d9a558fc40b25b48e4f782aff" {
		t.Fatalf("hash = %s", got)
	}
}
