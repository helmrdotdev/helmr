package capacityapi

import (
	"strings"
	"testing"
)

func TestWorkerReleaseManifestFingerprintAndValidation(t *testing.T) {
	manifest := WorkerReleaseManifest{
		Schema:        WorkerReleaseManifestSchema,
		WorkerVersion: "0123456789abcdef0123456789abcdef01234567",
		SupportsRun:   true,
		SupportsBuild: true,
		Runtime:       testRuntimeProfile(t),
		Substrate:     SubstrateProfile{Format: "ext4", Contract: "helmr.substrate.ext4.v0"},
		Capacity: ResourceVector{
			CPUMillis: 8000, MemoryBytes: 16 << 30, GuestEphemeralDiskBytes: 128 << 30,
			VMSlots: 2, BuildExecutors: 1,
		},
		PerVM: ResourceVector{
			CPUMillis: 4000, MemoryBytes: 8 << 30, GuestEphemeralDiskBytes: 32 << 30,
		},
	}
	fingerprint, err := manifest.ExpectedFingerprint()
	if err != nil {
		t.Fatal(err)
	}
	manifest.ReleaseFingerprint = fingerprint
	if err := manifest.Validate(); err != nil {
		t.Fatalf("validate canonical manifest: %v", err)
	}

	manifest.Capacity.CPUMillis++
	if err := manifest.Validate(); err == nil {
		t.Fatal("mutated manifest retained a valid fingerprint")
	}
}

func TestWorkerReleaseManifestRejectsInconsistentRuntime(t *testing.T) {
	manifest := validTestWorkerReleaseManifest(t)
	manifest.Runtime.ID = "sha256:" + strings.Repeat("f", 64)
	manifest.ReleaseFingerprint, _ = manifest.ExpectedFingerprint()
	if err := manifest.Validate(); err == nil {
		t.Fatal("manifest with a forged runtime identity was accepted")
	}
}

func TestWorkerReleaseManifestRejectsMultipleBuildExecutors(t *testing.T) {
	manifest := validTestWorkerReleaseManifest(t)
	manifest.Capacity.BuildExecutors = 2
	manifest.ReleaseFingerprint, _ = manifest.ExpectedFingerprint()
	if err := manifest.Validate(); err == nil {
		t.Fatal("manifest with multiple build executors was accepted")
	}
}

func testRuntimeProfile(t *testing.T) RuntimeProfile {
	t.Helper()
	profile := RuntimeProfile{
		Arch: "x86_64", Contract: "helmr.vm-runtime.v0",
		KernelDigest:    "sha256:" + strings.Repeat("1", 64),
		InitramfsDigest: "sha256:" + strings.Repeat("2", 64),
		RootfsDigest:    "sha256:" + strings.Repeat("3", 64),
	}
	var err error
	profile.ID, err = profile.ExpectedID()
	if err != nil {
		t.Fatal(err)
	}
	return profile
}
