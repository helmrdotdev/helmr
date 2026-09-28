//go:build linux

package firecracker

import (
	"os"
	"testing"

	"github.com/helmrdotdev/helmr/internal/vmplatform"
)

func TestPackagedFirecrackerProbeOutputIsAccepted(t *testing.T) {
	path := os.Getenv("FIRECRACKER_PATH")
	if path == "" {
		t.Skip("FIRECRACKER_PATH is not configured")
	}
	versionOutput, err := runRuntimeProbeCommand(t.Context(), path, "--version")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseFirecrackerVersion(versionOutput); err != nil {
		t.Fatal(err)
	}
	snapshotOutput, err := runRuntimeProbeCommand(t.Context(), path, "--snapshot-version")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseSnapshotFormatVersion(snapshotOutput); err != nil {
		t.Fatal(err)
	}
}

func testVMPlatform(t *testing.T, kernelDigest string, initramfsDigest string, rootfsDigest string) vmplatform.Profile {
	t.Helper()
	artifacts := testProbeRuntimeArtifacts()
	artifacts.Kernel.Digest = kernelDigest
	artifacts.Initramfs.Digest = initramfsDigest
	artifacts.Rootfs.Digest = rootfsDigest
	evidence := testHostRuntimeEvidence(t, 2, artifacts)
	identity, err := evidence.VMPlatform()
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
