//go:build linux

package firecracker

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/runtimeid"
)

func testRuntimeIdentity(t *testing.T, kernelDigest string, initramfsDigest string, rootfsDigest string) runtimeid.Selector {
	t.Helper()
	artifacts := testProbeRuntimeArtifacts()
	artifacts.Kernel.Digest = kernelDigest
	artifacts.Initramfs.Digest = initramfsDigest
	artifacts.Rootfs.Digest = rootfsDigest
	evidence := testHostRuntimeEvidence(t, 2, artifacts)
	identity, err := evidence.RuntimeIdentity()
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
