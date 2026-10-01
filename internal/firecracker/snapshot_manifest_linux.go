//go:build linux

package firecracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
)

func safeSnapshotID(id string) string {
	if id == "" {
		return uuid.NewV7().String()
	}
	out := make([]byte, 0, len(id))
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			out = append(out, byte(r))
		}
	}
	if len(out) == 0 {
		return uuid.NewV7().String()
	}
	return string(out)
}

type snapshotManifest struct {
	RecoveryPoint snapshotRecoveryPointManifest `json:"recovery_point"`
	RuntimeState  snapshotRuntimeStateManifest  `json:"runtime_state"`
}

type snapshotRecoveryPointManifest struct {
	ID      string                  `json:"id"`
	Runtime snapshotRuntimeManifest `json:"runtime"`
}

type snapshotRuntimeManifest struct {
	Backend          string                   `json:"backend"`
	DescriptorDigest string                   `json:"runtime_descriptor_digest"`
	ID               string                   `json:"id"`
	Arch             string                   `json:"arch"`
	Contract         string                   `json:"contract"`
	VCPUCount        int64                    `json:"vcpu_count"`
	CPUConfigDigest  string                   `json:"cpu_config_digest"`
	MemoryMiB        int64                    `json:"memory_mib"`
	ScratchDiskMiB   int64                    `json:"scratch_disk_mib"`
	KernelArgs       string                   `json:"kernel_args"`
	KernelDigest     string                   `json:"kernel_digest"`
	InitramfsDigest  string                   `json:"initramfs_digest"`
	RootfsDigest     string                   `json:"rootfs_digest"`
	Program          *snapshotProgramManifest `json:"program,omitempty"`
	GuestPort        uint32                   `json:"guest_port"`
	HealthPort       uint32                   `json:"health_port"`
}

type snapshotProgramManifest struct {
	Runtime  snapshotProgramArtifact `json:"runtime"`
	Artifact snapshotProgramArtifact `json:"artifact"`
}

type snapshotProgramArtifact struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
}

func snapshotProgram(drives []vm.ReadOnlyDrive) (*snapshotProgramManifest, error) {
	if len(drives) == 0 {
		return nil, nil
	}
	if err := validateProgramDriveIdentities(drives); err != nil {
		return nil, err
	}
	byID := make(map[string]vm.ReadOnlyDrive, len(drives))
	for _, drive := range drives {
		byID[drive.ID] = drive
	}
	artifact := func(id string) snapshotProgramArtifact {
		drive := byID[id]
		return snapshotProgramArtifact{
			Digest: drive.Digest, SizeBytes: drive.SizeBytes, MediaType: drive.MediaType,
		}
	}
	return &snapshotProgramManifest{
		Runtime:  artifact(vm.ProgramRuntimeDrive),
		Artifact: artifact(vm.ProgramDrive),
	}, nil
}

func validateSnapshotProgram(manifest *snapshotProgramManifest, drives []vm.ReadOnlyDrive) error {
	expected, err := snapshotProgram(drives)
	if err != nil {
		return err
	}
	if manifest == nil && expected == nil {
		return nil
	}
	if manifest == nil || expected == nil {
		return errors.New("checkpoint managed program drive identity does not match restore")
	}
	if *manifest != *expected {
		return errors.New("checkpoint managed program artifacts do not match restore")
	}
	return nil
}

type snapshotRuntimeStateManifest struct {
	Network snapshotNetworkManifest `json:"network"`
}

type snapshotNetworkManifest struct {
	GuestIPv4CIDR      string   `json:"guest_ipv4_cidr"`
	GuestMAC           string   `json:"guest_mac"`
	GatewayIPv4        string   `json:"gateway_ipv4"`
	GatewayMAC         string   `json:"gateway_mac"`
	ResolverAddresses  []string `json:"resolver_addresses"`
	GuestInterfaceName string   `json:"guest_interface_name"`
	MTU                int      `json:"mtu"`
}

func snapshotRuntimeConfig(cfg Config, checkpointID string, runtimeID string, cpuConfigDigest string, kernelDigest string, initramfsDigest string, rootfsDigest string, kernelArgs string, topology vm.Topology, readOnlyDrives ...[]vm.ReadOnlyDrive) (string, []byte, error) {
	if !sha256sum.ValidDigest(runtimeID) {
		return "", nil, errors.New("canonical bound host runtime ID is required for checkpoint restore")
	}
	workerArchitecture, err := vmplatform.ArchitectureFromGo(runtime.GOARCH)
	if err != nil {
		return "", nil, err
	}
	network := snapshotNetworkConfig(cfg)
	if err := validateSnapshotNetwork(network); err != nil {
		return "", nil, fmt.Errorf("build checkpoint network manifest: %w", err)
	}
	var drives []vm.ReadOnlyDrive
	if len(readOnlyDrives) > 1 {
		return "", nil, errors.New("snapshot runtime config accepts at most one read-only drive set")
	}
	if len(readOnlyDrives) == 1 {
		drives = readOnlyDrives[0]
	}
	program, err := snapshotProgram(drives)
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(kernelArgs) == "" {
		return "", nil, errors.New("canonical runtime kernel args are required for checkpoint restore")
	}
	if !sha256sum.ValidDigest(cpuConfigDigest) {
		return "", nil, errors.New("canonical guest CPU configuration digest is required for checkpoint restore")
	}
	descriptorDigest, err := CanonicalVMRuntimeDescriptor().Digest()
	if err != nil {
		return "", nil, err
	}
	manifest, err := json.Marshal(snapshotManifest{
		RecoveryPoint: snapshotRecoveryPointManifest{
			ID: checkpointID,
			Runtime: snapshotRuntimeManifest{
				Backend:          snapshotBackend,
				DescriptorDigest: descriptorDigest,
				ID:               runtimeID,
				Arch:             workerArchitecture,
				Contract:         vmplatform.Contract,
				VCPUCount:        cfg.VCPUCount,
				CPUConfigDigest:  cpuConfigDigest,
				MemoryMiB:        cfg.MemoryMiB,
				ScratchDiskMiB:   cfg.ScratchDiskMiB,
				KernelArgs:       kernelArgs,
				KernelDigest:     kernelDigest,
				InitramfsDigest:  initramfsDigest,
				RootfsDigest:     rootfsDigest,
				Program:          program,
				GuestPort:        cfg.GuestPort,
				HealthPort:       cfg.HealthPort,
			},
		},
		RuntimeState: snapshotRuntimeStateManifest{
			Network: network,
		},
	})
	if err != nil {
		return "", nil, fmt.Errorf("encode Firecracker snapshot manifest: %w", err)
	}
	return sha256sum.DigestBytes(manifest), manifest, nil
}

func snapshotNetworkConfig(cfg Config) snapshotNetworkManifest {
	return snapshotNetworkManifest{
		GuestIPv4CIDR:      GuestNetworkCIDRV0,
		GuestMAC:           GuestMACV0,
		GatewayIPv4:        GuestGatewayIPv4V0,
		GatewayMAC:         GuestGatewayMACV0,
		ResolverAddresses:  []string{strings.TrimSpace(cfg.NetworkResolverIPv4)},
		GuestInterfaceName: GuestInterfaceNameV0,
		MTU:                GuestMTUV0,
	}
}

func validateSnapshotNetwork(network snapshotNetworkManifest) error {
	guestIP, guestCIDR, err := net.ParseCIDR(network.GuestIPv4CIDR)
	if err != nil || guestIP.To4() == nil {
		return errors.New("checkpoint manifest guest_ipv4_cidr must be canonical IPv4 CIDR")
	}
	if guestIP.String()+"/"+strconv.Itoa(maskSize(guestCIDR.Mask)) != network.GuestIPv4CIDR || network.GuestIPv4CIDR != GuestNetworkCIDRV0 {
		return errors.New("checkpoint manifest guest_ipv4_cidr does not match VM runtime contract")
	}
	mac, err := net.ParseMAC(network.GuestMAC)
	if err != nil || len(mac) != 6 || mac.String() != network.GuestMAC || network.GuestMAC != GuestMACV0 {
		return errors.New("checkpoint manifest guest_mac does not match VM runtime contract")
	}
	gateway := net.ParseIP(network.GatewayIPv4)
	if gateway == nil || gateway.To4() == nil || gateway.String() != network.GatewayIPv4 || network.GatewayIPv4 != GuestGatewayIPv4V0 || !guestCIDR.Contains(gateway) {
		return errors.New("checkpoint manifest gateway_ipv4 does not match VM runtime contract")
	}
	gatewayMAC, err := net.ParseMAC(network.GatewayMAC)
	if err != nil || len(gatewayMAC) != 6 || gatewayMAC.String() != network.GatewayMAC || network.GatewayMAC != GuestGatewayMACV0 {
		return errors.New("checkpoint manifest gateway_mac does not match VM runtime contract")
	}
	if len(network.ResolverAddresses) != 1 {
		return errors.New("checkpoint manifest must contain exactly one IPv4 resolver")
	}
	for _, resolver := range network.ResolverAddresses {
		ip := net.ParseIP(resolver)
		if ip == nil || ip.To4() == nil || ip.String() != resolver {
			return errors.New("checkpoint manifest resolver_addresses must contain canonical IPv4 addresses")
		}
	}
	if network.GuestInterfaceName != GuestInterfaceNameV0 {
		return errors.New("checkpoint manifest guest_interface_name does not match VM runtime contract")
	}
	if network.MTU != GuestMTUV0 {
		return errors.New("checkpoint manifest mtu does not match VM runtime contract")
	}
	return nil
}

func maskSize(mask net.IPMask) int {
	ones, _ := mask.Size()
	return ones
}

func validateRestoredNetworkConfig(expected snapshotNetworkManifest, actual snapshotNetworkManifest) error {
	if err := validateSnapshotNetwork(expected); err != nil {
		return fmt.Errorf("validate checkpoint network manifest: %w", err)
	}
	if err := validateSnapshotNetwork(actual); err != nil {
		return fmt.Errorf("validate restored checkpoint network: %w", err)
	}
	if expected.GuestIPv4CIDR != actual.GuestIPv4CIDR ||
		expected.GuestMAC != actual.GuestMAC ||
		expected.GatewayIPv4 != actual.GatewayIPv4 ||
		expected.GatewayMAC != actual.GatewayMAC ||
		expected.GuestInterfaceName != actual.GuestInterfaceName ||
		expected.MTU != actual.MTU ||
		!slices.Equal(expected.ResolverAddresses, actual.ResolverAddresses) {
		return errors.New("restored checkpoint network does not exactly match manifest")
	}
	return nil
}

func validateRuntimeManifest(
	cfg Config,
	manifest snapshotManifest,
	runtimeID string,
	kernelDigest string,
	initramfsDigest string,
	rootfsDigest string,
	expectedCPUConfigDigest string,
	expectedKernelArgs string,
	expectedProgram []vm.ReadOnlyDrive,
) error {
	runtimeManifest := manifest.RecoveryPoint.Runtime
	if runtimeManifest.Backend != snapshotBackend {
		return fmt.Errorf("checkpoint manifest runtime backend %q is not supported", runtimeManifest.Backend)
	}
	descriptorDigest, err := CanonicalVMRuntimeDescriptor().Digest()
	if err != nil {
		return err
	}
	if runtimeManifest.DescriptorDigest != descriptorDigest {
		return fmt.Errorf("checkpoint manifest VM runtime descriptor digest %s does not match worker descriptor digest %s", runtimeManifest.DescriptorDigest, descriptorDigest)
	}
	workerArchitecture, err := vmplatform.ArchitectureFromGo(runtime.GOARCH)
	if err != nil {
		return err
	}
	if runtimeManifest.Arch != workerArchitecture {
		return fmt.Errorf("checkpoint manifest runtime arch %q does not match worker arch %q", runtimeManifest.Arch, workerArchitecture)
	}
	if runtimeManifest.Contract != vmplatform.Contract {
		return fmt.Errorf("checkpoint manifest runtime contract %q does not match worker contract %q", runtimeManifest.Contract, vmplatform.Contract)
	}
	if runtimeManifest.ID == "" {
		return errors.New("checkpoint manifest runtime id is required")
	}
	if runtimeManifest.ID != runtimeID {
		return fmt.Errorf("checkpoint manifest runtime id %s does not match worker runtime id %s", runtimeManifest.ID, runtimeID)
	}
	if runtimeManifest.KernelDigest != kernelDigest {
		return fmt.Errorf("checkpoint manifest kernel digest %s does not match worker kernel digest %s", runtimeManifest.KernelDigest, kernelDigest)
	}
	if runtimeManifest.InitramfsDigest != initramfsDigest {
		return fmt.Errorf("checkpoint manifest initramfs digest %s does not match worker initramfs digest %s", runtimeManifest.InitramfsDigest, initramfsDigest)
	}
	if runtimeManifest.RootfsDigest != rootfsDigest {
		return fmt.Errorf("checkpoint manifest rootfs digest %s does not match worker rootfs digest %s", runtimeManifest.RootfsDigest, rootfsDigest)
	}
	if runtimeManifest.CPUConfigDigest != expectedCPUConfigDigest {
		return fmt.Errorf("checkpoint manifest guest CPU configuration digest %s does not match expected digest %s", runtimeManifest.CPUConfigDigest, expectedCPUConfigDigest)
	}
	if err := validateSnapshotProgram(runtimeManifest.Program, expectedProgram); err != nil {
		return err
	}
	if runtimeManifest.VCPUCount != cfg.VCPUCount || runtimeManifest.MemoryMiB != cfg.MemoryMiB {
		return fmt.Errorf("checkpoint manifest machine shape vcpu=%d memory=%d does not match worker vcpu=%d memory=%d", runtimeManifest.VCPUCount, runtimeManifest.MemoryMiB, cfg.VCPUCount, cfg.MemoryMiB)
	}
	if runtimeManifest.ScratchDiskMiB != cfg.ScratchDiskMiB {
		return fmt.Errorf("checkpoint manifest scratch disk size %d MiB does not match worker scratch disk size %d MiB", runtimeManifest.ScratchDiskMiB, cfg.ScratchDiskMiB)
	}
	if runtimeManifest.KernelArgs != expectedKernelArgs ||
		runtimeManifest.GuestPort != cfg.GuestPort ||
		runtimeManifest.HealthPort != cfg.HealthPort {
		return errors.New("checkpoint manifest runtime ports or kernel args do not match worker runtime")
	}
	network := manifest.RuntimeState.Network
	if err := validateSnapshotNetwork(network); err != nil {
		return err
	}
	if err := validateRestoredNetworkConfig(snapshotNetworkConfig(cfg), network); err != nil {
		return fmt.Errorf("checkpoint manifest network does not match VM runtime contract: %w", err)
	}
	return nil
}

func cloneRuntimeComputer(source *vm.ComputerDisk) *vm.ComputerDisk {
	if source == nil {
		return nil
	}
	result := *source
	result.File = nil
	result.Device = nil
	return &result
}
