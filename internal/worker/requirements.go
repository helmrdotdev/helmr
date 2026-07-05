package worker

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
)

func validateLeaseRequirements(capabilities api.WorkerCapabilities, lease api.WorkerRunLease, run api.WorkerRun) error {
	if strings.TrimSpace(capabilities.ProtocolVersion) != strings.TrimSpace(lease.ProtocolVersion) {
		return fmt.Errorf("worker protocol %q does not match lease protocol %q", capabilities.ProtocolVersion, lease.ProtocolVersion)
	}
	if strings.TrimSpace(run.WorkerProtocolVersion) != strings.TrimSpace(lease.ProtocolVersion) {
		return fmt.Errorf("run worker protocol %q does not match lease protocol %q", run.WorkerProtocolVersion, lease.ProtocolVersion)
	}
	requirements := run.Requirements
	if err := requirements.Validate(); err != nil {
		return fmt.Errorf("run requirements: %w", err)
	}
	if err := validateRuntime(capabilities, requirements.Runtime); err != nil {
		return err
	}
	if err := validateResources(capabilities, requirements.Resources); err != nil {
		return err
	}
	if err := validatePlacement(capabilities, requirements.Placement); err != nil {
		return err
	}
	if err := validateNetwork(capabilities.Network, requirements.Network); err != nil {
		return err
	}
	return nil
}

func validateRuntime(capabilities api.WorkerCapabilities, runtime compute.RuntimeSelector) error {
	if capabilities.RuntimeID != runtime.ID {
		return fmt.Errorf("runtime id %q does not match worker runtime %q", runtime.ID, capabilities.RuntimeID)
	}
	if capabilities.RuntimeArch != runtime.Arch {
		return fmt.Errorf("runtime arch %q does not match worker runtime %q", runtime.Arch, capabilities.RuntimeArch)
	}
	if capabilities.RuntimeABI != runtime.ABI {
		return fmt.Errorf("runtime abi %q does not match worker runtime %q", runtime.ABI, capabilities.RuntimeABI)
	}
	if capabilities.KernelDigest != runtime.KernelDigest {
		return errors.New("runtime kernel digest does not match worker runtime")
	}
	if capabilities.InitramfsDigest != runtime.InitramfsDigest {
		return errors.New("runtime initramfs digest does not match worker runtime")
	}
	if capabilities.RootfsDigest != runtime.RootfsDigest {
		return errors.New("runtime rootfs digest does not match worker runtime")
	}
	if capabilities.CNIProfile != runtime.CNIProfile {
		return fmt.Errorf("runtime cni profile %q does not match worker runtime %q", runtime.CNIProfile, capabilities.CNIProfile)
	}
	return nil
}

func validateResources(capabilities api.WorkerCapabilities, resources compute.ResourceVector) error {
	available := compute.ResourceVector{
		MilliCPU:  capabilities.MaxVCPUs * 1000,
		MemoryMiB: capabilities.MaxMemoryMiB,
		DiskMiB:   capabilities.MaxDiskMiB,
		Slots:     capabilities.ExecutionSlotsAvailable,
	}
	if !available.Fits(resources) {
		return fmt.Errorf("run resources %+v exceed worker capacity %+v", resources, available)
	}
	return nil
}

func validatePlacement(capabilities api.WorkerCapabilities, placement compute.Placement) error {
	for key, value := range placement.Tags {
		if capabilities.Labels[key] != value {
			return fmt.Errorf("placement tag %s=%q does not match worker label", key, value)
		}
	}
	if placement.DedicatedKey != "" && capabilities.Labels["dedicated_key"] != placement.DedicatedKey {
		return fmt.Errorf("placement dedicated_key %q does not match worker label", placement.DedicatedKey)
	}
	if placement.SnapshotKey != "" && capabilities.Labels["snapshot_key"] != placement.SnapshotKey {
		return fmt.Errorf("placement snapshot_key %q does not match worker label", placement.SnapshotKey)
	}
	return nil
}

func validateNetwork(capabilities api.WorkerNetworkCapabilities, network compute.NetworkPolicy) error {
	if network.Internet && !capabilities.Internet {
		return errors.New("worker cannot provide internet access")
	}
	if !network.Internet && !capabilities.BlockInternet {
		return errors.New("worker cannot block internet access")
	}
	if len(network.Deny) > 0 && !capabilities.DenyCIDRs {
		return errors.New("worker cannot enforce CIDR deny rules")
	}
	if len(network.Allow) > 0 {
		if !capabilities.AllowCIDRs {
			return errors.New("worker cannot enforce network allow rules")
		}
		for _, entry := range network.Allow {
			if _, err := netip.ParsePrefix(entry); err != nil {
				if !capabilities.AllowDomains {
					return fmt.Errorf("worker cannot enforce domain allow rule %q", entry)
				}
			}
		}
	}
	return nil
}
