package main

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/capacity"
	"github.com/helmrdotdev/helmr/internal/firecracker"
)

func TestWorkerRuntimeProfileUsesMeasuredHostEvidence(t *testing.T) {
	digest := func(character string) string { return "sha256:" + strings.Repeat(character, 64) }
	artifacts := firecracker.RuntimeCapabilities{
		Arch: "x86_64", Contract: capacity.RuntimeContract,
		KernelDigest: digest("1"), InitramfsDigest: digest("2"), RootfsDigest: digest("3"),
	}
	evidence := firecracker.HostRuntimeEvidence{
		RuntimeArch: "x86_64", VMRuntimeContract: capacity.RuntimeContract,
		FirecrackerDigest: digest("4"), FirecrackerVersion: "1.16.1", SnapshotFormatVersion: "6.0.0",
		VMRuntimeDescriptorDigest: digest("5"), HostKernelRelease: "6.8.0-1024-aws",
		CPUTemplateSelector: firecracker.CPUTemplateSelector{Kind: firecracker.CPUTemplateNone},
		CPUShapes: []firecracker.CPUShapeEvidence{
			{VCPUCount: 1, CPUConfigDigest: digest("6")},
			{VCPUCount: 2, CPUConfigDigest: digest("7")},
		},
		Environment: firecracker.RuntimeEnvironmentEvidence{
			FirecrackerVersion: "1.16.1", KernelVersion: "6.8.0-1024-aws",
			MicrocodeVersion: "0x2b000643", BIOSVersion: "1.0", BIOSRevision: "1.0",
		},
		KernelDigest: artifacts.KernelDigest, InitramfsDigest: artifacts.InitramfsDigest,
		RootfsDigest: artifacts.RootfsDigest,
	}
	identity := capacity.RuntimeProfile{
		Arch: evidence.RuntimeArch, Contract: evidence.VMRuntimeContract,
		VMRuntimeDescriptorDigest: evidence.VMRuntimeDescriptorDigest,
		FirecrackerDigest:         evidence.FirecrackerDigest, FirecrackerVersion: evidence.FirecrackerVersion,
		SnapshotFormatVersion: evidence.SnapshotFormatVersion, HostKernelRelease: evidence.HostKernelRelease,
		CPUTemplate:  capacity.CPUTemplateSelector{Kind: capacity.CPUTemplateNone},
		KernelDigest: evidence.KernelDigest, InitramfsDigest: evidence.InitramfsDigest, RootfsDigest: evidence.RootfsDigest,
	}
	var err error
	evidence.RuntimeID, err = identity.ExpectedID()
	if err != nil {
		t.Fatal(err)
	}
	profile, shapes, environment, err := workerRuntimeProfile("x86_64", artifacts, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(shapes) != 2 || shapes[1].VCPUCount != 2 || shapes[1].CPUConfigDigest != digest("7") {
		t.Fatalf("CPU shapes = %+v", shapes)
	}
	if err := environment.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerRuntimeProfileRejectsArtifactDrift(t *testing.T) {
	digest := func(character string) string { return "sha256:" + strings.Repeat(character, 64) }
	_, _, _, err := workerRuntimeProfile("x86_64", firecracker.RuntimeCapabilities{
		Arch: "x86_64", Contract: capacity.RuntimeContract,
		KernelDigest: digest("1"), InitramfsDigest: digest("2"), RootfsDigest: digest("3"),
	}, firecracker.HostRuntimeEvidence{
		KernelDigest: digest("9"), InitramfsDigest: digest("2"), RootfsDigest: digest("3"),
	})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("artifact drift error = %v", err)
	}
}
