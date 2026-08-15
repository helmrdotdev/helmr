package firecracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
)

const (
	VMRuntimeDescriptorContract = "helmr.vm-runtime-descriptor.v0"
	MaxVMVCPUCount              = int64(32)

	defaultKernelArgs    = "console=ttyS0 reboot=k panic=1 root=/dev/vda rootfstype=squashfs ro init=/init"
	buildKernelArgs      = defaultKernelArgs + " helmr.profile=build helmr.pids_max=1024"
	imageBuildKernelArgs = defaultKernelArgs + " helmr.profile=image-build helmr.pids_max=1024"

	apiSocketName              = "api.sock"
	vsockSocketName            = "vsock.sock"
	scratchDiskName            = "scratch.ext4"
	restoreMemoryName          = "memory.mem"
	snapshotMemorySuffix       = ".mem"
	snapshotStateSuffix        = ".vmstate"
	snapshotScratchPackSuffix  = ".scratch.filepack"
	snapshotMemoryPackSuffix   = ".memory.filepack"
	readOnlyDriveSuffix        = ".squashfs"
	rootfsDriveID              = "rootfs"
	scratchDriveID             = "scratch"
	substrateDriveID           = "substrate"
	guestVsockID               = "guest-vsock"
	guestNetworkInterfaceID    = "1"
	defaultRuntimeProfileName  = "default"
	buildRuntimeProfileName    = "build"
	imageBuildProfileName      = "image-build"
	runtimeSubstrateKernelFlag = "helmr.substrate=1"
	runtimeProgramKernelFlag   = "helmr.program=1"
	snapshotBackend            = "firecracker"
	snapshotCreateType         = "Full"
	snapshotMemoryBackend      = "File"
	machineHugePages           = "None"

	GuestNetworkCIDRV0   = "192.168.127.2/30"
	GuestGatewayIPv4V0   = "192.168.127.1"
	GuestGatewayMACV0    = "02:fc:00:00:00:01"
	GuestMACV0           = "02:fc:00:00:00:02"
	GuestTapNameV0       = "tap0"
	GuestInterfaceNameV0 = "eth0"
	GuestMTUV0           = 1500
)

var readOnlyDriveOrder = [...]string{
	vm.ProgramRuntimeDrive,
	vm.ProgramDrive,
	vm.ManagerDrive,
	vm.ManagedRuntimeDrive,
	vm.ToolchainDrive,
	vm.BuildTreeDrive,
}

// VMRuntimeDescriptor is the shape-independent Firecracker restore contract.
// Per-VM CPU, memory, disk, resolver, and workload artifact values are supplied
// separately and must not be added to this descriptor.
type VMRuntimeDescriptor struct {
	Contract string                      `json:"contract"`
	Machine  VMRuntimeMachineDescriptor  `json:"machine"`
	Boot     VMRuntimeBootDescriptor     `json:"boot"`
	Devices  VMRuntimeDeviceDescriptor   `json:"devices"`
	Paths    VMRuntimePathDescriptor     `json:"paths"`
	Network  VMRuntimeNetworkDescriptor  `json:"network"`
	Snapshot VMRuntimeSnapshotDescriptor `json:"snapshot"`
}

type VMRuntimeMachineDescriptor struct {
	SMT             bool   `json:"smt"`
	TrackDirtyPages bool   `json:"track_dirty_pages"`
	HugePages       string `json:"huge_pages"`
	PCIEnabled      bool   `json:"pci_enabled"`
	MaxVCPUCount    int64  `json:"max_vcpu_count"`
	VCPUDerivation  string `json:"vcpu_derivation"`
}

type VMRuntimeBootDescriptor struct {
	Profiles     []VMRuntimeBootProfile `json:"profiles"`
	DynamicFlags []string               `json:"dynamic_flags"`
}

type VMRuntimeBootProfile struct {
	Name       string `json:"name"`
	KernelArgs string `json:"kernel_args"`
}

type VMRuntimeDeviceDescriptor struct {
	Drives        []VMRuntimeDriveDescriptor `json:"drives"`
	NetworkID     string                     `json:"network_id"`
	NetworkIDRule string                     `json:"network_id_rule"`
	Vsock         VMRuntimeVsockDescriptor   `json:"vsock"`
	Absent        []string                   `json:"absent"`
}

type VMRuntimeVsockDescriptor struct {
	ID                  string `json:"id"`
	HostPath            string `json:"host_path"`
	GuestCIDStart       uint32 `json:"guest_cid_start"`
	GuestCIDAllocation  string `json:"guest_cid_allocation"`
	GuestPortProtocol   string `json:"guest_port_protocol"`
	HealthPortProtocol  string `json:"health_port_protocol"`
	SnapshotLoadBinding string `json:"snapshot_load_binding"`
}

type VMRuntimeDriveDescriptor struct {
	ID             string `json:"id"`
	Required       bool   `json:"required"`
	Root           bool   `json:"root"`
	ReadOnly       bool   `json:"read_only"`
	PathConvention string `json:"path_convention"`
}

type VMRuntimePathDescriptor struct {
	APISocket                 string `json:"api_socket"`
	VsockSocket               string `json:"vsock_socket"`
	ScratchDisk               string `json:"scratch_disk"`
	ReadOnlyDriveSuffix       string `json:"read_only_drive_suffix"`
	JailRoot                  string `json:"jail_root"`
	SnapshotMemorySuffix      string `json:"snapshot_memory_suffix"`
	SnapshotStateSuffix       string `json:"snapshot_state_suffix"`
	SnapshotScratchPackSuffix string `json:"snapshot_scratch_pack_suffix"`
	SnapshotMemoryPackSuffix  string `json:"snapshot_memory_pack_suffix"`
	RestoreMemory             string `json:"restore_memory"`
}

type VMRuntimeNetworkDescriptor struct {
	GuestIPv4CIDR      string `json:"guest_ipv4_cidr"`
	GuestMAC           string `json:"guest_mac"`
	GatewayIPv4        string `json:"gateway_ipv4"`
	GatewayMAC         string `json:"gateway_mac"`
	TapName            string `json:"tap_name"`
	GuestInterfaceName string `json:"guest_interface_name"`
	MTU                int    `json:"mtu"`
	ResolverSource     string `json:"resolver_source"`
	NetNSPath          string `json:"netns_path"`
}

type VMRuntimeSnapshotDescriptor struct {
	Backend                 string   `json:"backend"`
	CreateType              string   `json:"create_type"`
	PauseBeforeCreate       bool     `json:"pause_before_create"`
	LoadMemoryBackend       string   `json:"load_memory_backend"`
	LoadEnableDiffSnapshots bool     `json:"load_enable_diff_snapshots"`
	LoadResumeVM            bool     `json:"load_resume_vm"`
	RestoreOrder            []string `json:"restore_order"`
	Protocol                string   `json:"protocol"`
}

// CanonicalVMRuntimeDescriptor returns a fresh copy of the only supported VM
// runtime descriptor. Callers may inspect it but must use its canonical digest
// as the restore selector.
func CanonicalVMRuntimeDescriptor() VMRuntimeDescriptor {
	drives := []VMRuntimeDriveDescriptor{
		{ID: rootfsDriveID, Required: true, Root: true, ReadOnly: true, PathConvention: "config.rootfs_path"},
		{ID: scratchDriveID, Required: true, PathConvention: scratchDiskName},
		{ID: substrateDriveID, ReadOnly: true, PathConvention: "basename(config.substrate_path)"},
	}
	for _, id := range readOnlyDriveOrder {
		drives = append(drives, VMRuntimeDriveDescriptor{
			ID: id, ReadOnly: true, PathConvention: id + readOnlyDriveSuffix,
		})
	}
	return VMRuntimeDescriptor{
		Contract: VMRuntimeDescriptorContract,
		Machine: VMRuntimeMachineDescriptor{
			SMT: false, TrackDirtyPages: false, HugePages: machineHugePages,
			PCIEnabled: false, MaxVCPUCount: MaxVMVCPUCount,
			VCPUDerivation: "ceil(positive_millicpu/1000)",
		},
		Boot: VMRuntimeBootDescriptor{
			Profiles: []VMRuntimeBootProfile{
				{Name: defaultRuntimeProfileName, KernelArgs: defaultKernelArgs},
				{Name: buildRuntimeProfileName, KernelArgs: buildKernelArgs},
				{Name: imageBuildProfileName, KernelArgs: imageBuildKernelArgs},
			},
			DynamicFlags: []string{runtimeSubstrateKernelFlag, runtimeProgramKernelFlag},
		},
		Devices: VMRuntimeDeviceDescriptor{
			Drives: drives, NetworkID: guestNetworkInterfaceID,
			NetworkIDRule: "one_based_sdk_slice_order",
			Vsock: VMRuntimeVsockDescriptor{
				ID: guestVsockID, HostPath: "<jail_root>/" + vsockSocketName,
				GuestCIDStart:       3,
				GuestCIDAllocation:  "process_monotonic_uint32",
				GuestPortProtocol:   "config.guest_port;checkpoint_manifest_fenced",
				HealthPortProtocol:  "config.health_port;checkpoint_manifest_fenced",
				SnapshotLoadBinding: "snapshot_state;skip_add_vsock_api",
			},
			Absent: []string{"balloon", "entropy", "mmds"},
		},
		Paths: VMRuntimePathDescriptor{
			APISocket: apiSocketName, VsockSocket: vsockSocketName,
			ScratchDisk: scratchDiskName, ReadOnlyDriveSuffix: readOnlyDriveSuffix,
			JailRoot:             "<jailer_chroot_base>/<firecracker_basename>/<vm_id>/root",
			SnapshotMemorySuffix: snapshotMemorySuffix, SnapshotStateSuffix: snapshotStateSuffix,
			SnapshotScratchPackSuffix: snapshotScratchPackSuffix,
			SnapshotMemoryPackSuffix:  snapshotMemoryPackSuffix,
			RestoreMemory:             restoreMemoryName,
		},
		Network: VMRuntimeNetworkDescriptor{
			GuestIPv4CIDR: GuestNetworkCIDRV0, GuestMAC: GuestMACV0,
			GatewayIPv4: GuestGatewayIPv4V0, GatewayMAC: GuestGatewayMACV0,
			TapName: GuestTapNameV0, GuestInterfaceName: GuestInterfaceNameV0,
			MTU: GuestMTUV0, ResolverSource: "worker_config",
			NetNSPath: "/var/run/netns/<vm_id>",
		},
		Snapshot: VMRuntimeSnapshotDescriptor{
			Backend: snapshotBackend, CreateType: snapshotCreateType,
			PauseBeforeCreate: true, LoadMemoryBackend: snapshotMemoryBackend,
			LoadEnableDiffSnapshots: false, LoadResumeVM: false,
			RestoreOrder: []string{
				"recreate_network", "load_paused", "validate_network", "resume_vm", "wait_guest_health",
			},
			Protocol: "memory+vmstate+scratch-filepack+manifest.v0",
		},
	}
}

func (descriptor VMRuntimeDescriptor) CanonicalJSON() ([]byte, error) {
	if !reflect.DeepEqual(descriptor, CanonicalVMRuntimeDescriptor()) {
		return nil, errors.New("VM runtime descriptor does not match the canonical contract")
	}
	raw, err := json.Marshal(descriptor)
	if err != nil {
		return nil, fmt.Errorf("encode VM runtime descriptor: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize VM runtime descriptor: %w", err)
	}
	return canonical, nil
}

func (descriptor VMRuntimeDescriptor) Digest() (string, error) {
	canonical, err := descriptor.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return sha256sum.DigestBytes(canonical), nil
}

// VCPUCountForMilliCPU is the single restore-contract conversion from a
// positive milliCPU request to the Firecracker vCPU shape. The subtraction
// form avoids overflowing at MaxInt64.
func VCPUCountForMilliCPU(milliCPU int64) (int64, error) {
	if milliCPU <= 0 {
		return 0, fmt.Errorf("milliCPU must be positive, got %d", milliCPU)
	}
	return (milliCPU-1)/1000 + 1, nil
}

type CPUTemplateSelectorKind string

const (
	CPUTemplateNone   CPUTemplateSelectorKind = "none"
	CPUTemplateCustom CPUTemplateSelectorKind = "custom"
)

// CPUTemplateSelector deliberately supports only no template or a reviewed
// custom template digest. Deprecated Firecracker static templates are not part
// of the Helmr restore contract.
type CPUTemplateSelector struct {
	Kind   CPUTemplateSelectorKind `json:"kind"`
	Digest string                  `json:"digest,omitempty"`
}

func NoCPUTemplateSelector() CPUTemplateSelector {
	return CPUTemplateSelector{Kind: CPUTemplateNone}
}

func CustomCPUTemplateSelector(digest string) (CPUTemplateSelector, error) {
	selector := CPUTemplateSelector{Kind: CPUTemplateCustom, Digest: digest}
	if err := selector.Validate(); err != nil {
		return CPUTemplateSelector{}, err
	}
	return selector, nil
}

func (selector CPUTemplateSelector) withDefaults() CPUTemplateSelector {
	if selector.Kind == "" && selector.Digest == "" {
		return NoCPUTemplateSelector()
	}
	return selector
}

func (selector CPUTemplateSelector) Validate() error {
	switch selector.Kind {
	case CPUTemplateNone:
		if selector.Digest != "" {
			return errors.New("no-template CPU selector must not include a digest")
		}
	case CPUTemplateCustom:
		if !sha256sum.ValidDigest(selector.Digest) {
			return errors.New("custom CPU template selector requires a canonical SHA-256 digest")
		}
	default:
		return fmt.Errorf("CPU template selector kind %q is not supported", selector.Kind)
	}
	return nil
}

func validateCPUTemplateLaunch(selector CPUTemplateSelector) error {
	selector = selector.withDefaults()
	if err := selector.Validate(); err != nil {
		return err
	}
	if selector.Kind == CPUTemplateCustom {
		return errors.New("custom CPU template probing is supported, but Firecracker launch integration is not available")
	}
	return nil
}

type cpuTemplateHelperConfig struct {
	BootSource    cpuTemplateHelperBootSource    `json:"boot-source"`
	Drives        []cpuTemplateHelperDrive       `json:"drives"`
	MachineConfig cpuTemplateHelperMachineConfig `json:"machine-config"`
}

type cpuTemplateHelperBootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	InitrdPath      string `json:"initrd_path"`
	BootArgs        string `json:"boot_args"`
}

type cpuTemplateHelperDrive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type cpuTemplateHelperMachineConfig struct {
	VCPUCount       int64  `json:"vcpu_count"`
	MemoryMiB       int64  `json:"mem_size_mib"`
	SMT             bool   `json:"smt"`
	TrackDirtyPages bool   `json:"track_dirty_pages"`
	HugePages       string `json:"huge_pages"`
}

func canonicalCPUTemplateHelperConfig(cfg Config, vcpuCount int64) ([]byte, error) {
	descriptor := CanonicalVMRuntimeDescriptor()
	if vcpuCount < 1 || vcpuCount > descriptor.Machine.MaxVCPUCount {
		return nil, fmt.Errorf("CPU template helper vCPU count %d is outside [1,%d]", vcpuCount, descriptor.Machine.MaxVCPUCount)
	}
	if cfg.MemoryMiB <= 0 {
		return nil, fmt.Errorf("CPU template helper memory must be positive, got %d MiB", cfg.MemoryMiB)
	}
	for name, value := range map[string]string{
		"kernel": cfg.KernelPath, "initramfs": cfg.InitramfsPath, "rootfs": cfg.RootfsPath,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("CPU template helper %s path is required", name)
		}
	}
	document := cpuTemplateHelperConfig{
		BootSource: cpuTemplateHelperBootSource{
			KernelImagePath: cfg.KernelPath, InitrdPath: cfg.InitramfsPath,
			BootArgs: descriptor.Boot.Profiles[0].KernelArgs,
		},
		Drives: []cpuTemplateHelperDrive{{
			DriveID: rootfsDriveID, PathOnHost: cfg.RootfsPath,
			IsRootDevice: true, IsReadOnly: true,
		}},
		MachineConfig: cpuTemplateHelperMachineConfig{
			VCPUCount: vcpuCount, MemoryMiB: cfg.MemoryMiB, SMT: descriptor.Machine.SMT,
			TrackDirtyPages: descriptor.Machine.TrackDirtyPages,
			HugePages:       descriptor.Machine.HugePages,
		},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode CPU template helper config: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize CPU template helper config: %w", err)
	}
	return canonical, nil
}
