package capacity

import (
	"math"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/runtimeid"
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

func validTestWorkerTemplate(t *testing.T) WorkerTemplate {
	t.Helper()
	return WorkerTemplate{
		Schema:    WorkerTemplateSchema,
		Runtime:   testRuntimeProfile(t),
		CPUShapes: testCPUShapes(4),
		Substrate: SubstrateProfile{Format: SubstrateFormatExt4, Contract: SubstrateContractExt4},
		Capacity:  ResourceVector{CPUMillis: 4000, MemoryBytes: 8 << 30, GuestEphemeralDiskBytes: 64 << 30, VMSlots: 1},
		PerVM:     ResourceVector{CPUMillis: 4000, MemoryBytes: 8 << 30, GuestEphemeralDiskBytes: 32 << 30},
	}
}

func testRuntimeProfile(t *testing.T) runtimeid.Profile {
	t.Helper()
	profile := runtimeid.Profile{
		Arch: "x86_64", Contract: runtimeid.Contract,
		VMRuntimeDescriptorDigest: "sha256:" + strings.Repeat("a", 64),
		FirecrackerDigest:         "sha256:" + strings.Repeat("b", 64),
		FirecrackerVersion:        "1.16.1",
		SnapshotFormatVersion:     "6.0.0",
		HostKernelRelease:         "6.8.0-1024-aws",
		CPUTemplate:               runtimeid.CPUTemplateSelector{Kind: runtimeid.CPUTemplateNone},
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

func testCPUShapes(count int) []runtimeid.CPUShape {
	shapes := make([]runtimeid.CPUShape, count)
	for index := range shapes {
		shapes[index] = runtimeid.CPUShape{
			VCPUCount:       int32(index + 1),
			CPUConfigDigest: "sha256:" + strings.Repeat(string(rune('4'+index)), 64),
		}
	}
	return shapes
}
