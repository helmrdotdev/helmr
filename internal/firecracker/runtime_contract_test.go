package firecracker

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestVMRuntimeDescriptorGoldenJSONAndDigest(t *testing.T) {
	const goldenJSON = `{"boot":{"dynamic_flags":["helmr.substrate=1","helmr.program=1"],"profiles":[{"kernel_args":"console=ttyS0 reboot=k panic=1 root=/dev/vda rootfstype=squashfs ro init=/init","name":"default"},{"kernel_args":"console=ttyS0 reboot=k panic=1 root=/dev/vda rootfstype=squashfs ro init=/init helmr.profile=build helmr.pids_max=1024","name":"build"},{"kernel_args":"console=ttyS0 reboot=k panic=1 root=/dev/vda rootfstype=squashfs ro init=/init helmr.profile=image-build helmr.pids_max=1024","name":"image-build"}]},"contract":"helmr.vm-runtime-descriptor.v0","devices":{"absent":["balloon","entropy","mmds"],"drives":[{"id":"rootfs","path_convention":"config.rootfs_path","read_only":true,"required":true,"root":true},{"id":"scratch","path_convention":"scratch.ext4","read_only":false,"required":true,"root":false},{"id":"substrate","path_convention":"basename(config.substrate_path)","read_only":true,"required":false,"root":false},{"id":"program_runtime","path_convention":"program_runtime.squashfs","read_only":true,"required":false,"root":false},{"id":"program","path_convention":"program.squashfs","read_only":true,"required":false,"root":false},{"id":"manager","path_convention":"manager.squashfs","read_only":true,"required":false,"root":false},{"id":"managed_runtime","path_convention":"managed_runtime.squashfs","read_only":true,"required":false,"root":false},{"id":"toolchain","path_convention":"toolchain.squashfs","read_only":true,"required":false,"root":false},{"id":"build_tree","path_convention":"build_tree.squashfs","read_only":true,"required":false,"root":false}],"network_id":"1","network_id_rule":"one_based_sdk_slice_order","vsock":{"guest_cid_allocation":"process_monotonic_uint32","guest_cid_start":3,"guest_port_protocol":"config.guest_port;checkpoint_manifest_fenced","health_port_protocol":"config.health_port;checkpoint_manifest_fenced","host_path":"<jail_root>/vsock.sock","id":"guest-vsock","snapshot_load_binding":"snapshot_state;skip_add_vsock_api"}},"machine":{"huge_pages":"None","max_vcpu_count":32,"pci_enabled":false,"smt":false,"track_dirty_pages":false,"vcpu_derivation":"ceil(positive_millicpu/1000)"},"network":{"gateway_ipv4":"192.168.127.1","gateway_mac":"02:fc:00:00:00:01","guest_interface_name":"eth0","guest_ipv4_cidr":"192.168.127.2/30","guest_mac":"02:fc:00:00:00:02","mtu":1500,"netns_path":"/var/run/netns/<vm_id>","resolver_source":"worker_config","tap_name":"tap0"},"paths":{"api_socket":"api.sock","jail_root":"<jailer_chroot_base>/<firecracker_basename>/<vm_id>/root","read_only_drive_suffix":".squashfs","restore_memory":"memory.mem","scratch_disk":"scratch.ext4","snapshot_memory_pack_suffix":".memory.filepack","snapshot_memory_suffix":".mem","snapshot_scratch_pack_suffix":".scratch.filepack","snapshot_state_suffix":".vmstate","vsock_socket":"vsock.sock"},"snapshot":{"backend":"firecracker","create_type":"Full","load_enable_diff_snapshots":false,"load_memory_backend":"File","load_resume_vm":false,"pause_before_create":true,"protocol":"memory+vmstate+scratch-filepack+manifest.v0","restore_order":["recreate_network","load_paused","validate_network","resume_vm","wait_guest_health"]}}`
	const goldenDigest = "sha256:49c64d202b195ae159fc38098cddb8d6196be81126f2418d6d419953c28d4344"

	descriptor := CanonicalVMRuntimeDescriptor()
	canonical, err := descriptor.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != goldenJSON {
		t.Fatalf("canonical descriptor = %s\nwant = %s", canonical, goldenJSON)
	}
	digest, err := descriptor.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if digest != goldenDigest {
		t.Fatalf("descriptor digest = %s, want %s", digest, goldenDigest)
	}
}

func TestVCPUCountForMilliCPUIsOverflowSafe(t *testing.T) {
	tests := []struct {
		milliCPU int64
		want     int64
	}{
		{milliCPU: 1, want: 1},
		{milliCPU: 999, want: 1},
		{milliCPU: 1000, want: 1},
		{milliCPU: 1001, want: 2},
		{milliCPU: 2000, want: 2},
		{milliCPU: math.MaxInt64, want: (math.MaxInt64-1)/1000 + 1},
	}
	for _, test := range tests {
		got, err := VCPUCountForMilliCPU(test.milliCPU)
		if err != nil {
			t.Fatalf("VCPUCountForMilliCPU(%d): %v", test.milliCPU, err)
		}
		if got != test.want {
			t.Fatalf("VCPUCountForMilliCPU(%d) = %d, want %d", test.milliCPU, got, test.want)
		}
	}
	for _, invalid := range []int64{0, -1} {
		if _, err := VCPUCountForMilliCPU(invalid); err == nil {
			t.Fatalf("VCPUCountForMilliCPU(%d) succeeded", invalid)
		}
	}
}

func TestCPUTemplateSelectorOnlyAllowsNoneOrCanonicalCustomDigest(t *testing.T) {
	if err := NoCPUTemplateSelector().Validate(); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	selector, err := CustomCPUTemplateSelector(digest)
	if err != nil {
		t.Fatal(err)
	}
	if selector.Kind != CPUTemplateCustom || selector.Digest != digest {
		t.Fatalf("selector = %+v", selector)
	}
	if err := validateCPUTemplateLaunch(NoCPUTemplateSelector()); err != nil {
		t.Fatal(err)
	}
	if err := validateCPUTemplateLaunch(selector); err == nil || !strings.Contains(err.Error(), "launch integration") {
		t.Fatalf("custom launch error = %v", err)
	}
	for _, invalid := range []CPUTemplateSelector{
		{},
		{Kind: CPUTemplateNone, Digest: digest},
		{Kind: CPUTemplateCustom, Digest: "sha256:" + strings.Repeat("A", 64)},
		{Kind: "T2"},
	} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("selector %+v validated", invalid)
		}
	}
}

func TestCPUTemplateHelperConfigComesFromRuntimeDescriptor(t *testing.T) {
	raw, err := canonicalCPUTemplateHelperConfig(Config{
		KernelPath: "/runtime/vmlinux", InitramfsPath: "/runtime/initramfs",
		RootfsPath: "/runtime/rootfs.squashfs", MemoryMiB: 2048,
	}, 3)
	if err != nil {
		t.Fatal(err)
	}
	var config cpuTemplateHelperConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	descriptor := CanonicalVMRuntimeDescriptor()
	if config.MachineConfig.VCPUCount != 3 || config.MachineConfig.MemoryMiB != 2048 ||
		config.MachineConfig.SMT != descriptor.Machine.SMT ||
		config.MachineConfig.TrackDirtyPages != descriptor.Machine.TrackDirtyPages ||
		config.MachineConfig.HugePages != descriptor.Machine.HugePages {
		t.Fatalf("machine config = %+v", config.MachineConfig)
	}
	if config.BootSource.BootArgs != descriptor.Boot.Profiles[0].KernelArgs {
		t.Fatalf("boot args = %q", config.BootSource.BootArgs)
	}
	if len(config.Drives) != 1 || config.Drives[0].DriveID != descriptor.Devices.Drives[0].ID || !config.Drives[0].IsRootDevice || !config.Drives[0].IsReadOnly {
		t.Fatalf("drives = %+v", config.Drives)
	}
}

func TestVMRuntimeDescriptorReturnsDefensiveCopies(t *testing.T) {
	before := CanonicalVMRuntimeDescriptor()
	beforeDigest, err := before.Digest()
	if err != nil {
		t.Fatal(err)
	}
	mutated := CanonicalVMRuntimeDescriptor()
	mutated.Boot.Profiles[0].KernelArgs = "mutated"
	mutated.Boot.DynamicFlags[0] = "mutated"
	mutated.Devices.Drives[0].ID = "mutated"
	mutated.Devices.Absent[0] = "mutated"
	mutated.Snapshot.RestoreOrder[0] = "mutated"
	after := CanonicalVMRuntimeDescriptor()
	afterDigest, err := after.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || beforeDigest != afterDigest {
		t.Fatalf("canonical descriptor changed through a returned copy")
	}
}
