package runtime

import (
	"encoding/json"
	"testing"
)

const goldenRuntimeIdentityKey = `{"runtime_id":"runtime-1","deployment_sandbox_id":"sandbox-1","image_digest":"sha256:image","image_format":"oci-tar","workspace_mount_path":"/workspace","sandbox_artifact_digest":"sha256:sandbox","sandbox_artifact_format":"oci-tar","substrate_key":"sha256:64ac7d1a22a09bd0f1765f2b62d2dcec9f089aaf5703c8ac560e47d75182ed9e","network":{"internet":false,"deny":["10.0.0.0/8"]}}`

func TestKeyMatchesGoldenIdentity(t *testing.T) {
	key := Key(Identity{
		RuntimeID:                  "runtime-1",
		DeploymentSandboxID:        "sandbox-1",
		ImageDigest:                "sha256:image",
		ImageFormat:                "oci-tar",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifactDigest: "sha256:sandbox",
		SandboxImageArtifactFormat: "oci-tar",
		RuntimeSubstrateCacheKey:   "sha256:64ac7d1a22a09bd0f1765f2b62d2dcec9f089aaf5703c8ac560e47d75182ed9e",
		Network:                    json.RawMessage(`{"internet":false,"deny":["10.0.0.0/8"]}`),
	})
	if key != goldenRuntimeIdentityKey {
		t.Fatalf("key = %s, want %s", key, goldenRuntimeIdentityKey)
	}
	if got := Hash(key); got != "aaecef8bfeefdda802b6e7b4df8d384768f4982e269894f5674dd488b1e072ae" {
		t.Fatalf("hash = %s", got)
	}
	if got := ID(key); got != "aaecef8bfeefdda8" {
		t.Fatalf("id = %s", got)
	}
}

func TestKeyNormalizesEmptyNetworkToObject(t *testing.T) {
	key := Key(Identity{RuntimeID: "runtime-1"})
	want := `{"runtime_id":"runtime-1","deployment_sandbox_id":"","image_digest":"","image_format":"","workspace_mount_path":"","sandbox_artifact_digest":"","sandbox_artifact_format":"","substrate_key":"","network":{}}`
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
