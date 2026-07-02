package runtime

import (
	"encoding/json"
	"testing"
)

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
