package capacityapi

import (
	"math"
	"strings"
	"testing"
)

func TestWorkerTemplateValidation(t *testing.T) {
	template := WorkerTemplate{
		Schema:    WorkerTemplateSchema,
		Runtime:   testRuntimeProfile(t),
		CPUShapes: testCPUShapes(4),
		Substrate: SubstrateProfile{Format: "ext4", Contract: "helmr.substrate.ext4.v0"},
		Capacity: ResourceVector{
			CPUMillis: 8000, MemoryBytes: 16 << 30, GuestEphemeralDiskBytes: 128 << 30,
			VMSlots: 2,
		},
		PerVM: ResourceVector{
			CPUMillis: 4000, MemoryBytes: 8 << 30, GuestEphemeralDiskBytes: 32 << 30,
		},
	}
	if err := template.Validate(); err != nil {
		t.Fatalf("validate canonical template: %v", err)
	}

	template.Capacity.CPUMillis = 0
	if err := template.Validate(); err == nil {
		t.Fatal("template with invalid capacity was accepted")
	}
}

func TestRuntimeProfileRejectsInconsistentIdentity(t *testing.T) {
	profile := testRuntimeProfile(t)
	profile.ID = "sha256:" + strings.Repeat("f", 64)
	if err := profile.Validate(); err == nil {
		t.Fatal("profile with a forged runtime identity was accepted")
	}
}

func TestRuntimeProfileExpectedIDRejectsNoncanonicalSelector(t *testing.T) {
	profile := testRuntimeProfile(t)
	profile.KernelDigest = strings.ToUpper(profile.KernelDigest)
	if _, err := profile.ExpectedID(); err == nil {
		t.Fatal("ExpectedID accepted a noncanonical selector")
	}
}

func TestRuntimeProfileRejectsInvalidCPUTemplateSelector(t *testing.T) {
	profile := testRuntimeProfile(t)
	profile.CPUTemplate.Digest = "sha256:" + strings.Repeat("4", 64)
	if _, err := profile.ExpectedID(); err == nil {
		t.Fatal("no-template selector with a digest was accepted")
	}
	profile.CPUTemplate = CPUTemplateSelector{Kind: CPUTemplateCustom}
	if _, err := profile.ExpectedID(); err == nil {
		t.Fatal("custom-template selector without a digest was accepted")
	}
}

func TestWorkerTemplateRejectsIncompleteCPUShapes(t *testing.T) {
	template := validTestWorkerTemplate(t)
	template.CPUShapes = template.CPUShapes[:len(template.CPUShapes)-1]
	if err := template.Validate(); err == nil {
		t.Fatal("template with an incomplete CPU shape map was accepted")
	}
}

func TestWorkerTemplateRejectsCPUShapeOverflow(t *testing.T) {
	template := validTestWorkerTemplate(t)
	template.PerVM.CPUMillis = math.MaxInt64
	if err := template.Validate(); err == nil {
		t.Fatal("template with an unrepresentable vCPU range was accepted")
	}
}

func testRuntimeProfile(t *testing.T) RuntimeProfile {
	t.Helper()
	profile := RuntimeProfile{
		Arch: "x86_64", Contract: RuntimeContract,
		VMRuntimeDescriptorDigest: "sha256:" + strings.Repeat("a", 64),
		FirecrackerDigest:         "sha256:" + strings.Repeat("b", 64),
		FirecrackerVersion:        "1.16.1",
		SnapshotFormatVersion:     "6.0.0",
		HostKernelRelease:         "6.8.0-1024-aws",
		CPUTemplate:               CPUTemplateSelector{Kind: CPUTemplateNone},
		KernelDigest:              "sha256:" + strings.Repeat("1", 64),
		InitramfsDigest:           "sha256:" + strings.Repeat("2", 64),
		RootfsDigest:              "sha256:" + strings.Repeat("3", 64),
	}
	var err error
	profile.ID, err = profile.ExpectedID()
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func testCPUShapes(count int) []CPUShape {
	shapes := make([]CPUShape, count)
	for index := range shapes {
		shapes[index] = CPUShape{
			VCPUCount:       int32(index + 1),
			CPUConfigDigest: "sha256:" + strings.Repeat(string(rune('4'+index)), 64),
		}
	}
	return shapes
}
