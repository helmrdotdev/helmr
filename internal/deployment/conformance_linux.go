//go:build linux

package deployment

import (
	"context"
	"errors"
	"fmt"
	"os"
)

type ConformanceValidator struct {
	UnitCgroupRoot string
}

func (validator ConformanceValidator) Runtime(
	ctx context.Context,
	identity string,
	runtime *platformTree,
	descriptor RuntimeArtifactDescriptor,
) (PlatformConformance, error) {
	return validator.run(ctx, identity, runtimeConformanceJob, descriptor, runtime)
}

func (validator ConformanceValidator) Manager(
	ctx context.Context,
	identity string,
	runtime *platformTree,
	manager *platformTree,
	descriptor ManagerArtifactDescriptor,
) (PlatformConformance, error) {
	return validator.run(ctx, identity, managerConformanceJob, descriptor, runtime, manager)
}

func (validator ConformanceValidator) Toolchain(
	ctx context.Context,
	identity string,
	runtime *platformTree,
	toolchain *platformTree,
	descriptor ToolchainArtifactDescriptor,
) (PlatformConformance, error) {
	return validator.run(ctx, identity, toolchainConformanceJob, descriptor, runtime, toolchain)
}

func (validator ConformanceValidator) run(
	ctx context.Context,
	identity string,
	job verifierJob,
	descriptor any,
	trees ...*platformTree,
) (PlatformConformance, error) {
	raw, err := CanonicalPlatformDocument(descriptor)
	if err != nil {
		return PlatformConformance{}, err
	}
	descriptorFile, err := sealedConformanceDescriptor(raw)
	if err != nil {
		return PlatformConformance{}, err
	}
	defer descriptorFile.Close()
	artifacts := make([]*os.File, 0, len(trees)+1)
	for _, tree := range trees {
		if tree == nil || tree.artifact == nil {
			return PlatformConformance{}, errors.New("platform conformance tree is closed")
		}
		file, err := tree.artifact.verifierFile()
		if err != nil {
			return PlatformConformance{}, err
		}
		artifacts = append(artifacts, file)
	}
	artifacts = append(artifacts, descriptorFile)
	result, err := runVerifierProcess(ctx, verifierProcessConfig{
		job:            job,
		unitCgroupRoot: validator.UnitCgroupRoot,
		leaseIdentity:  identity,
		artifacts:      artifacts,
	})
	if err != nil {
		return PlatformConformance{}, err
	}
	switch result.kind {
	case verifierVerified:
		var conformance PlatformConformance
		if err := parsePlatformDocument(
			result.payload,
			"Platform conformance result",
			&conformance,
		); err != nil {
			return PlatformConformance{}, err
		}
		return conformance, nil
	case verifierInvalid:
		return PlatformConformance{}, &verifierInvalidError{diagnostic: result.diagnostic}
	case verifierFailed:
		return PlatformConformance{}, errors.New("platform conformance validator failed")
	default:
		return PlatformConformance{}, fmt.Errorf("platform conformance validator outcome = %d", result.kind)
	}
}

func sealedConformanceDescriptor(raw []byte) (*os.File, error) {
	file, err := os.CreateTemp("", ".helmr-conformance-descriptor-*")
	if err != nil {
		return nil, err
	}
	name := file.Name()
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		_ = os.Remove(name)
		return nil, err
	}
	if err := file.Chmod(0400); err != nil {
		_ = file.Close()
		_ = os.Remove(name)
		return nil, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return nil, err
	}
	sealed, err := os.Open(name)
	removeErr := os.Remove(name)
	if err != nil || removeErr != nil {
		if sealed != nil {
			_ = sealed.Close()
		}
		return nil, errors.Join(err, removeErr)
	}
	return sealed, nil
}
