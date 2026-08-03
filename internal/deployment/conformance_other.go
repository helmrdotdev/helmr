//go:build !linux

package deployment

import (
	"context"
	"errors"
)

type ConformanceValidator struct {
	UnitCgroupRoot string
}

func (ConformanceValidator) Runtime(
	context.Context,
	string,
	*platformTree,
	RuntimeArtifactDescriptor,
) (PlatformConformance, error) {
	return PlatformConformance{}, errors.New("Platform conformance requires Linux")
}

func (ConformanceValidator) Manager(
	context.Context,
	string,
	*platformTree,
	*platformTree,
	ManagerArtifactDescriptor,
) (PlatformConformance, error) {
	return PlatformConformance{}, errors.New("Platform conformance requires Linux")
}

func (ConformanceValidator) Toolchain(
	context.Context,
	string,
	*platformTree,
	*platformTree,
	ToolchainArtifactDescriptor,
) (PlatformConformance, error) {
	return PlatformConformance{}, errors.New("Platform conformance requires Linux")
}
