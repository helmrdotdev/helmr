package runtime

import (
	"encoding/json"
	"testing"
)

const goldenRuntimeIdentityKey = `{"runtime_id":"runtime-1","deployment_sandbox_id":"sandbox-1","image_digest":"sha256:image","image_format":"oci-tar","rootfs_digest":"sha256:rootfs","runtime_abi":"runtime-abi","guestd_abi":"guestd-abi","adapter_abi":"adapter-abi","workspace_mount_path":"/workspace","sandbox_artifact_digest":"sha256:sandbox","sandbox_artifact_format":"oci-tar","substrate_key":"sha256:9d06f1ce620cdfa34be30058524cfb49331aeb7451524c5358c4154a2bfb381c","network":{"internet":false,"deny":["10.0.0.0/8"]}}`

func TestKeyMatchesGoldenIdentity(t *testing.T) {
	key := Key(Identity{
		RuntimeID:                  "runtime-1",
		DeploymentSandboxID:        "sandbox-1",
		ImageDigest:                "sha256:image",
		ImageFormat:                "oci-tar",
		RootfsDigest:               "sha256:rootfs",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifactDigest: "sha256:sandbox",
		SandboxImageArtifactFormat: "oci-tar",
		RuntimeSubstrateCacheKey:   "sha256:9d06f1ce620cdfa34be30058524cfb49331aeb7451524c5358c4154a2bfb381c",
		Network:                    json.RawMessage(`{"internet":false,"deny":["10.0.0.0/8"]}`),
	})
	if key != goldenRuntimeIdentityKey {
		t.Fatalf("key = %s, want %s", key, goldenRuntimeIdentityKey)
	}
	if got := Hash(key); got != "46b507b394ef59614c5991d6952196851dd6e95d9a558fc40b25b48e4f782aff" {
		t.Fatalf("hash = %s", got)
	}
	if got := ID(key); got != "46b507b394ef5961" {
		t.Fatalf("id = %s", got)
	}
}

func TestKeyNormalizesEmptyNetworkToObject(t *testing.T) {
	key := Key(Identity{RuntimeID: "runtime-1"})
	want := `{"runtime_id":"runtime-1","deployment_sandbox_id":"","image_digest":"","image_format":"","rootfs_digest":"","runtime_abi":"","guestd_abi":"","adapter_abi":"","workspace_mount_path":"","sandbox_artifact_digest":"","sandbox_artifact_format":"","substrate_key":"","network":{}}`
	if key != want {
		t.Fatalf("key = %s, want %s", key, want)
	}
}

func TestKeyNormalizesIdentityFields(t *testing.T) {
	identity := Identity{
		DeploymentSandboxID:        "sandbox-1",
		RuntimeID:                  "runtime-1",
		ImageDigest:                "sha256:image",
		ImageFormat:                "oci-tar",
		RootfsDigest:               "sha256:rootfs",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifactDigest: "sha256:sandbox",
		SandboxImageArtifactFormat: "oci-tar",
		RuntimeSubstrateCacheKey:   "sha256:substrate",
		Network:                    json.RawMessage(`{"internet":false}`),
	}
	trimmed := Key(identity)
	identity.RuntimeID = " " + identity.RuntimeID + " "
	identity.ImageDigest = identity.ImageDigest + " "
	identity.WorkspaceMountPath = " " + identity.WorkspaceMountPath
	identity.SandboxImageArtifactDigest = " " + identity.SandboxImageArtifactDigest + " "
	withWhitespace := Key(identity)
	if withWhitespace != trimmed {
		t.Fatalf("runtime prep key changed after whitespace normalization:\ntrimmed=%s\nwithWhitespace=%s", trimmed, withWhitespace)
	}
}
