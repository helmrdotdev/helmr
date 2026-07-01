package runtimeprep

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
)

func TestKeyFromSourceNormalizesIdentityFields(t *testing.T) {
	source := api.WorkerPreparedRuntimeSource{
		DeploymentSandboxID:        "sandbox-1",
		RuntimeID:                  "runtime-1",
		ImageDigest:                "sha256:image",
		ImageFormat:                "oci-tar",
		RootfsDigest:               "sha256:rootfs",
		RuntimeABI:                 "runtime-abi",
		GuestdABI:                  "guestd-abi",
		AdapterABI:                 "adapter-abi",
		WorkspaceMountPath:         "/workspace",
		SandboxImageArtifact:       api.CASObject{Digest: "sha256:sandbox", SizeBytes: 1, MediaType: "application/vnd.helmr.sandbox-image.v0.oci-tar"},
		SandboxImageArtifactFormat: "oci-tar",
	}
	trimmed := KeyFromSource(source, compute.NetworkPolicy{})
	source.RuntimeID = " " + source.RuntimeID + " "
	source.ImageDigest = source.ImageDigest + " "
	source.WorkspaceMountPath = " " + source.WorkspaceMountPath
	source.SandboxImageArtifact.Digest = " " + source.SandboxImageArtifact.Digest + " "
	withWhitespace := KeyFromSource(source, compute.NetworkPolicy{})
	if withWhitespace != trimmed {
		t.Fatalf("runtime prep key changed after whitespace normalization:\ntrimmed=%s\nwithWhitespace=%s", trimmed, withWhitespace)
	}
}
