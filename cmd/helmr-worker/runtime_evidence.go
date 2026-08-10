package main

import (
	"errors"
	"fmt"
	"math"

	"github.com/helmrdotdev/helmr/capacityapi"
	"github.com/helmrdotdev/helmr/internal/firecracker"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func workerRuntimeProfile(
	architecture string,
	artifacts firecracker.RuntimeCapabilities,
	evidence firecracker.HostRuntimeEvidence,
) (capacityapi.RuntimeProfile, []capacityapi.CPUShape, workerapi.CPUEnvironment, error) {
	if artifacts.Arch != architecture || artifacts.Contract != capacityapi.RuntimeContract {
		return capacityapi.RuntimeProfile{}, nil, workerapi.CPUEnvironment{}, errors.New("runtime artifacts do not match the supported runtime contract")
	}
	if artifacts.KernelDigest != evidence.KernelDigest ||
		artifacts.InitramfsDigest != evidence.InitramfsDigest ||
		artifacts.RootfsDigest != evidence.RootfsDigest {
		return capacityapi.RuntimeProfile{}, nil, workerapi.CPUEnvironment{}, errors.New("measured runtime evidence does not match the loaded guest artifacts")
	}
	profile := capacityapi.RuntimeProfile{
		Arch:                      architecture,
		Contract:                  artifacts.Contract,
		VMRuntimeDescriptorDigest: evidence.VMRuntimeDescriptorDigest,
		FirecrackerDigest:         evidence.FirecrackerDigest,
		FirecrackerVersion:        evidence.FirecrackerVersion,
		SnapshotFormatVersion:     evidence.SnapshotFormatVersion,
		HostKernelRelease:         evidence.HostKernelRelease,
		CPUTemplate: capacityapi.CPUTemplateSelector{
			Kind:   capacityapi.CPUTemplateKind(evidence.CPUTemplateSelector.Kind),
			Digest: evidence.CPUTemplateSelector.Digest,
		},
		KernelDigest:    evidence.KernelDigest,
		InitramfsDigest: evidence.InitramfsDigest,
		RootfsDigest:    evidence.RootfsDigest,
	}
	var err error
	profile.ID, err = profile.ExpectedID()
	if err != nil {
		return capacityapi.RuntimeProfile{}, nil, workerapi.CPUEnvironment{}, err
	}
	if evidence.RuntimeID != profile.ID || evidence.RuntimeArch != profile.Arch || evidence.VMRuntimeContract != profile.Contract {
		return capacityapi.RuntimeProfile{}, nil, workerapi.CPUEnvironment{}, errors.New("measured runtime identity does not match the connector-bound runtime identity")
	}
	if err := profile.Validate(); err != nil {
		return capacityapi.RuntimeProfile{}, nil, workerapi.CPUEnvironment{}, err
	}
	shapes := make([]capacityapi.CPUShape, len(evidence.CPUShapes))
	for index, shape := range evidence.CPUShapes {
		if shape.VCPUCount <= 0 || shape.VCPUCount > math.MaxInt32 {
			return capacityapi.RuntimeProfile{}, nil, workerapi.CPUEnvironment{}, fmt.Errorf("measured CPU shape %d has unsupported vCPU count %d", index, shape.VCPUCount)
		}
		shapes[index] = capacityapi.CPUShape{
			VCPUCount: int32(shape.VCPUCount), CPUConfigDigest: shape.CPUConfigDigest,
		}
	}
	environment := workerapi.CPUEnvironment{
		FirecrackerVersion: evidence.Environment.FirecrackerVersion,
		HostKernelRelease:  evidence.Environment.KernelVersion,
		MicrocodeVersion:   evidence.Environment.MicrocodeVersion,
		BIOSVersion:        evidence.Environment.BIOSVersion,
		BIOSRevision:       evidence.Environment.BIOSRevision,
	}
	environment.Digest, err = environment.ExpectedDigest()
	if err != nil {
		return capacityapi.RuntimeProfile{}, nil, workerapi.CPUEnvironment{}, err
	}
	if environment.FirecrackerVersion != profile.FirecrackerVersion || environment.HostKernelRelease != profile.HostKernelRelease {
		return capacityapi.RuntimeProfile{}, nil, workerapi.CPUEnvironment{}, errors.New("CPU environment does not match the measured runtime profile")
	}
	return profile, shapes, environment, nil
}
