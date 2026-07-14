package compute

import (
	"strings"
	"testing"
)

func TestRunRuntimeRequirementsFromFields(t *testing.T) {
	requirements, err := RunRuntimeRequirementsFromFields(RunRuntimeRequirementFields{
		RequestedMilliCPU:       1000,
		RequestedMemoryMiB:      512,
		RequestedDiskMiB:        1024,
		RequestedExecutionSlots: 1,
		RuntimeID:               "sha256:runtime",
		RuntimeArch:             "amd64",
		RuntimeABI:              "helmr.firecracker.snapshot.v0",
		KernelDigest:            "sha256:kernel",
		InitramfsDigest:         "sha256:initramfs",
		RootfsDigest:            "sha256:rootfs",
		CNIProfile:              "helmr/v0",
		NetworkPolicyJSON:       []byte(`{"internet":false,"deny":["10.0.0.0/8"]}`),
		PlacementJSON:           []byte(`{"tags":{"pool":"warm"}}`),
	})
	if err != nil {
		t.Fatalf("RunRuntimeRequirementsFromFields() error = %v", err)
	}
	if requirements.Network.Internet || len(requirements.Network.Deny) != 1 {
		t.Fatalf("Network = %#v", requirements.Network)
	}
	if requirements.Placement.Tags["pool"] != "warm" {
		t.Fatalf("Placement.Tags = %#v", requirements.Placement.Tags)
	}
}

func TestRunRuntimeRequirementsRejectsPlacementRegion(t *testing.T) {
	_, err := RunRuntimeRequirementsFromFields(RunRuntimeRequirementFields{
		RequestedMilliCPU:       1000,
		RequestedMemoryMiB:      512,
		RequestedDiskMiB:        1024,
		RequestedExecutionSlots: 1,
		RuntimeID:               "sha256:runtime",
		RuntimeArch:             "amd64",
		RuntimeABI:              "helmr.firecracker.snapshot.v0",
		KernelDigest:            "sha256:kernel",
		InitramfsDigest:         "sha256:initramfs",
		RootfsDigest:            "sha256:rootfs",
		CNIProfile:              "helmr/v0",
		PlacementJSON:           []byte(`{"region":"local"}`),
	})
	if err == nil {
		t.Fatalf("RunRuntimeRequirementsFromFields() error = nil")
	}
}

func TestRunRuntimeRequirementsFromFieldsLabelsJSONErrors(t *testing.T) {
	_, err := RunRuntimeRequirementsFromFields(RunRuntimeRequirementFields{
		NetworkPolicyJSON:  []byte(`{`),
		NetworkPolicyLabel: "worker run network policy",
	})
	if err == nil {
		t.Fatalf("RunRuntimeRequirementsFromFields() error = nil")
	}
	if got, want := err.Error(), "worker run network policy:"; len(got) < len(want) || got[:len(want)] != want {
		t.Fatalf("error = %q, want prefix %q", got, want)
	}
}

func TestRunRuntimeRequirementsFromFieldsRejectsPhysicalPlacementAuthority(t *testing.T) {
	_, err := RunRuntimeRequirementsFromFields(RunRuntimeRequirementFields{
		RequestedMilliCPU:       100,
		RequestedMemoryMiB:      128,
		RequestedDiskMiB:        64,
		RequestedExecutionSlots: 1,
		RuntimeID:               "runtime",
		RuntimeArch:             "amd64",
		RuntimeABI:              "v1",
		KernelDigest:            "sha256:kernel",
		InitramfsDigest:         "sha256:initramfs",
		RootfsDigest:            "sha256:rootfs",
		CNIProfile:              "default",
		PlacementJSON:           []byte(`{"worker_group_id":"hidden-authority"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("RunRuntimeRequirementsFromFields() error = %v, want unknown field", err)
	}
}
