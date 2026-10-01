//go:build linux

package firecracker

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/helmrdotdev/helmr/internal/vm"
)

func TestExt4FreeBytesReadsFreshFilesystemCapacity(t *testing.T) {
	raw := make([]byte, ext4SuperblockOffset+ext4SuperblockBytes)
	superblock := raw[ext4SuperblockOffset:]
	binary.LittleEndian.PutUint16(superblock[56:58], ext4Magic)
	binary.LittleEndian.PutUint32(superblock[24:28], 2)
	binary.LittleEndian.PutUint32(superblock[12:16], 17)
	binary.LittleEndian.PutUint32(superblock[0x158:0x15c], 1)
	path := filepath.Join(t.TempDir(), "scratch.ext4")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ext4FreeBytes(path)
	if err != nil {
		t.Fatal(err)
	}
	want := (uint64(1)<<32 | 17) * 4096
	if got != want {
		t.Fatalf("free bytes = %d, want %d", got, want)
	}
}

func TestDefaultKernelArgsDeclareSquashFSRoot(t *testing.T) {
	if !strings.Contains(defaultKernelArgs, "rootfstype=squashfs") {
		t.Fatalf("defaultKernelArgs = %q", defaultKernelArgs)
	}
}

func TestRuntimeKernelArgsDescribeExactDriveTopology(t *testing.T) {
	const baseArgs = defaultKernelArgs + " helmr.ip=192.168.127.2::192.168.127.1:255.255.255.252::eth0:off:10.0.0.2::"
	source := &recordingReadOnlyDriveSource{}
	program := []vm.ReadOnlyDrive{
		{ID: vm.ProgramRuntimeDrive, Source: source},
		{ID: vm.ProgramDrive, Source: source},
	}
	if got := runtimeKernelArgs(vm.Topology{}, nil, "10.0.0.2"); got != baseArgs {
		t.Fatalf("default args = %q", got)
	}
	if got := runtimeKernelArgs(
		vm.Topology{Computer: &vm.ComputerDisk{}},
		nil, "10.0.0.2",
	); got != baseArgs+" helmr.computer=1" {
		t.Fatalf("computer args = %q", got)
	}
	if got := runtimeKernelArgs(
		vm.Topology{Computer: &vm.ComputerDisk{}},
		program, "10.0.0.2",
	); got != baseArgs+" helmr.computer=1 helmr.program=1" {
		t.Fatalf("Program args = %q", got)
	}
}

func TestGuestNetworkArgsSurviveSDKSetupAndMatchRestore(t *testing.T) {
	source := &recordingReadOnlyDriveSource{}
	topology := vm.Topology{Computer: &vm.ComputerDisk{}}
	drives := []vm.ReadOnlyDrive{
		{ID: vm.ProgramRuntimeDrive, Source: source},
		{ID: vm.ProgramDrive, Source: source},
	}
	for _, test := range []struct {
		name     string
		topology vm.Topology
		drives   []vm.ReadOnlyDrive
	}{
		{name: "qualification"},
		{name: "runtime", topology: topology},
		{name: "materialization", topology: topology, drives: drives},
	} {
		t.Run(test.name, func(t *testing.T) {
			connector := &Connector{cfg: Config{NetworkResolverIPv4: "10.0.0.2"}}
			if test.name != "qualification" {
				connector.kernelArgs = runtimeKernelArgs(test.topology, test.drives, connector.cfg.NetworkResolverIPv4)
			}
			expectedRestoreArgs := runtimeKernelArgs(test.topology, test.drives, connector.cfg.NetworkResolverIPv4)
			if connector.kernelArgsValue() != expectedRestoreArgs {
				t.Fatal("launch and restore arguments differ")
			}
			sdkMachine := &firecracker.Machine{Cfg: firecracker.Config{
				KernelArgs:        connector.kernelArgsValue(),
				NetworkInterfaces: firecracker.NetworkInterfaces{staticNetworkInterface()},
			}}
			if err := firecracker.SetupKernelArgsHandler.Fn(context.Background(), sdkMachine); err != nil {
				t.Fatal(err)
			}
			tokens := make(map[string]int)
			for _, token := range strings.Fields(sdkMachine.Cfg.KernelArgs) {
				key, _, _ := strings.Cut(token, "=")
				switch key {
				case "ip", "nfsaddrs", "initcall_debug", "ignore_loglevel", "async.dyndbg":
					t.Fatalf("unexpected kernel argument %q", token)
				}
				tokens[key]++
			}
			if tokens["helmr.ip"] != 1 {
				t.Fatalf("helmr.ip count = %d", tokens["helmr.ip"])
			}
			want := strings.Fields(expectedRestoreArgs)
			got := strings.Fields(sdkMachine.Cfg.KernelArgs)
			slices.Sort(want)
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Fatalf("effective SDK arguments = %v, want %v", got, want)
			}
		})
	}
}

func TestMaterializeRequiresActivationProbedCPUShape(t *testing.T) {
	computerInstanceID := uuid.NewV7().String()
	rootfsDigest := testCanonicalDigest("0")
	artifacts := testProbeRuntimeArtifacts()
	artifacts.Rootfs.Digest = rootfsDigest
	validRequest := vm.MaterializeRequest{
		ID:        computerInstanceID,
		OwnerKind: vm.OwnerInstance,
		Binding: vm.WorkloadBinding{
			WorkerEpoch: 1, OwnerID: computerInstanceID, Generation: 1,
			ComputerInstanceID: computerInstanceID,
		},
		RootfsDigest:      rootfsDigest,
		ComputerMountPath: "/workspace",
		Resources:         vm.Resources{MilliCPU: 1500},
		VMVCPUCount:       2,
		CPUConfigDigest:   testCPUConfigDigest(2),
	}
	connector := &Connector{
		cfg:         Config{VCPUCount: 2},
		artifacts:   artifacts,
		hostRuntime: newHostRuntimeEvidenceStore(),
	}
	evidence := testHostRuntimeEvidence(t, 2, artifacts)
	validRequest.Binding.VMPlatformID = evidence.RuntimeID
	if err := connector.hostRuntime.bind(evidence, 2); err != nil {
		t.Fatal(err)
	}
	if err := connector.validateMaterializeRequest(validRequest); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		edit func(*vm.MaterializeRequest)
		want string
	}{
		{name: "missing vcpu", edit: func(request *vm.MaterializeRequest) { request.VMVCPUCount = 0 }, want: "does not match"},
		{name: "wrong vcpu", edit: func(request *vm.MaterializeRequest) { request.VMVCPUCount = 1 }, want: "does not match"},
		{name: "invalid digest", edit: func(request *vm.MaterializeRequest) { request.CPUConfigDigest = "sha256:other" }, want: "not canonical"},
		{name: "wrong local digest", edit: func(request *vm.MaterializeRequest) { request.CPUConfigDigest = testCPUConfigDigest(1) }, want: "does not match target digest"},
		{name: "wrong runtime identity", edit: func(request *vm.MaterializeRequest) { request.Binding.VMPlatformID = testCanonicalDigest("f") }, want: "does not match target host runtime"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validRequest
			test.edit(&request)
			if err := connector.validateMaterializeRequest(request); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}

	unbound := *connector
	unbound.hostRuntime = newHostRuntimeEvidenceStore()
	if err := unbound.validateMaterializeRequest(validRequest); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("unbound error = %v", err)
	}
}

func TestMachineRuntimeFailsClosedUntilHostEvidenceIsBound(t *testing.T) {
	artifacts := testProbeRuntimeArtifacts()
	connector := &Connector{
		cfg:         Config{VCPUCount: 2},
		artifacts:   artifacts,
		hostRuntime: newHostRuntimeEvidenceStore(),
	}
	if _, _, _, err := connector.boundMachineRuntime(2); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("unbound machine runtime error = %v", err)
	}
	evidence := testHostRuntimeEvidence(t, 2, artifacts)
	if err := connector.hostRuntime.bind(evidence, 2); err != nil {
		t.Fatal(err)
	}
	identity, digest, firecrackerPath, err := connector.boundMachineRuntime(2)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ID != evidence.RuntimeID || digest != evidence.CPUShapes[1].CPUConfigDigest || firecrackerPath != evidence.firecrackerPath {
		t.Fatalf("bound machine identity = %+v digest=%q executable=%q evidence=%+v", identity, digest, firecrackerPath, evidence)
	}
}

func TestSessionEntryPointsRejectWorkloadVMPlatformMismatch(t *testing.T) {
	connector := testConnector(t, testRestoreConfig(t))
	computerInstanceID := uuid.NewV7().String()
	binding := vm.WorkloadBinding{
		WorkerEpoch:        1,
		OwnerID:            computerInstanceID,
		Generation:         1,
		ComputerInstanceID: computerInstanceID,
		VMPlatformID:       testCanonicalDigest("f"),
	}
	if _, err := connector.prepareMachine(
		context.Background(),
		workloadLaunch,
		computerInstanceID,
		vm.OwnerInstance,
		binding,
		"",
		"",
		"",
		nil,
		vm.Topology{},
		nil,
		nil,
		nil,
	); err == nil || !strings.Contains(err.Error(), "does not match bound host runtime") {
		t.Fatalf("prepare machine runtime identity error = %v", err)
	}
	if _, err := connector.restore(context.Background(), vm.RestoreRequest{
		ComputerInstanceID: computerInstanceID,
		OwnerKind:          vm.OwnerInstance,
		Binding:            binding,
	}); err == nil || !strings.Contains(err.Error(), "does not match target host runtime") {
		t.Fatalf("restore runtime identity error = %v", err)
	}
}

func TestRuntimeSDKConfigurationUsesDescriptor(t *testing.T) {
	descriptor := CanonicalVMRuntimeDescriptor()
	machine := runtimeMachineConfiguration(descriptor, Config{VCPUCount: 4, MemoryMiB: 4096})
	if machine.VcpuCount == nil || *machine.VcpuCount != 4 || machine.MemSizeMib == nil || *machine.MemSizeMib != 4096 {
		t.Fatalf("machine configuration = %+v", machine)
	}
	if machine.Smt == nil || *machine.Smt != descriptor.Machine.SMT || machine.TrackDirtyPages != descriptor.Machine.TrackDirtyPages {
		t.Fatalf("machine configuration = %+v descriptor = %+v", machine, descriptor.Machine)
	}
	if machine.CPUTemplate != "" {
		t.Fatalf("static CPU template = %q", machine.CPUTemplate)
	}
	device := runtimeVsockDevice(descriptor, descriptor.Devices.Vsock.GuestCIDStart)
	if device.ID != descriptor.Devices.Vsock.ID || device.Path != descriptor.Paths.VsockSocket || device.CID != descriptor.Devices.Vsock.GuestCIDStart {
		t.Fatalf("vsock device = %+v descriptor = %+v", device, descriptor.Devices.Vsock)
	}
}

func TestConfigForMaterializeRequestUsesRequestedRuntimeResources(t *testing.T) {
	connector := &Connector{cfg: Config{VCPUCount: 4, MemoryMiB: 4096, ScratchDiskMiB: 32768}}
	cfg, err := connector.configForMaterializeRequest(vm.MaterializeRequest{
		Resources: vm.Resources{
			MilliCPU:  1500,
			MemoryMiB: 1024,
			DiskMiB:   4096,
			Slots:     1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VCPUCount != 2 {
		t.Fatalf("vcpu count = %d, want 2", cfg.VCPUCount)
	}
	if cfg.MemoryMiB != 1024 {
		t.Fatalf("memory = %d MiB, want 1024", cfg.MemoryMiB)
	}
	if cfg.ScratchDiskMiB != 4096 {
		t.Fatalf("scratch disk = %d MiB, want 4096", cfg.ScratchDiskMiB)
	}
}

func TestConfigForMaterializeRequestRejectsOversizedRuntimeResources(t *testing.T) {
	connector := &Connector{cfg: Config{VCPUCount: 2, MemoryMiB: 2048, ScratchDiskMiB: 8192}}
	for name, resources := range map[string]vm.Resources{
		"memory": {MilliCPU: 1000, MemoryMiB: 4096, DiskMiB: 4096, Slots: 1},
		"cpu":    {MilliCPU: 3000, MemoryMiB: 1024, DiskMiB: 4096, Slots: 1},
		"disk":   {MilliCPU: 1000, MemoryMiB: 1024, DiskMiB: 16384, Slots: 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := connector.configForMaterializeRequest(vm.MaterializeRequest{Resources: resources}); err == nil {
				t.Fatalf("expected oversized %s request to fail", name)
			}
		})
	}
}
