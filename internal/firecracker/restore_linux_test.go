//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/filepack"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/sirupsen/logrus"
)

func TestValidateRestoreIdentityRejectsManifestMismatch(t *testing.T) {
	cfg := testRestoreConfig(t)
	kernelDigest := testDigest([]byte("kernel"))
	initramfsDigest := testDigest([]byte("initramfs"))
	rootfsDigest := testDigest([]byte("rootfs"))
	runtimeID := testVMPlatform(t, kernelDigest, initramfsDigest, rootfsDigest).ID
	descriptorDigest, err := CanonicalVMRuntimeDescriptor().Digest()
	if err != nil {
		t.Fatal(err)
	}
	connector := testConnector(t, cfg)

	validManifest := snapshotManifest{
		RecoveryPoint: snapshotRecoveryPointManifest{
			ID: "checkpoint-1",
			Runtime: snapshotRuntimeManifest{
				Backend:          "firecracker",
				DescriptorDigest: descriptorDigest,
				ID:               runtimeID,
				Arch:             testCheckpointArchitecture(t),
				Contract:         vmplatform.Contract,
				VCPUCount:        cfg.VCPUCount,
				CPUConfigDigest:  testCPUConfigDigest(cfg.VCPUCount),
				MemoryMiB:        cfg.MemoryMiB,
				ScratchDiskMiB:   cfg.ScratchDiskMiB,
				KernelArgs:       runtimeKernelArgs(vm.Topology{}, nil, cfg.NetworkResolverIPv4),
				KernelDigest:     kernelDigest,
				InitramfsDigest:  initramfsDigest,
				RootfsDigest:     rootfsDigest,
				GuestPort:        cfg.GuestPort,
				HealthPort:       cfg.HealthPort,
			},
		},
		RuntimeState: snapshotRuntimeStateManifest{
			Network: snapshotNetworkConfig(cfg),
		},
	}

	tests := []struct {
		name         string
		checkpointID string
		manifest     []byte
		editManifest func(*snapshotManifest)
		editIdentity func(*vm.CheckpointIdentity)
		want         string
	}{
		{name: "valid"},
		{name: "missing manifest", manifest: []byte{}, want: "checkpoint manifest is required"},
		{name: "malformed manifest", manifest: []byte("{"), want: "decode checkpoint manifest"},
		{name: "checkpoint id", checkpointID: "other", want: `checkpoint manifest recovery point id "checkpoint-1" does not match restore id "other"`},
		{name: "identity backend", editIdentity: func(i *vm.CheckpointIdentity) { i.RuntimeBackend = "test" }, want: `checkpoint runtime backend "test" is not supported`},
		{name: "identity arch", editIdentity: func(i *vm.CheckpointIdentity) { i.RuntimeArch = "other" }, want: `checkpoint runtime arch "other" does not match`},
		{name: "identity contract", editIdentity: func(i *vm.CheckpointIdentity) { i.VMRuntimeContract = "other" }, want: `checkpoint runtime contract "other" does not match`},
		{name: "identity runtime id", editIdentity: func(i *vm.CheckpointIdentity) { i.RuntimeID = "sha256:other" }, want: "checkpoint runtime id sha256:other does not match"},
		{name: "identity kernel digest", editIdentity: func(i *vm.CheckpointIdentity) { i.KernelDigest = "sha256:other" }, want: "checkpoint kernel digest sha256:other does not match"},
		{name: "identity initramfs digest", editIdentity: func(i *vm.CheckpointIdentity) { i.InitramfsDigest = "sha256:other" }, want: "checkpoint initramfs digest sha256:other does not match"},
		{name: "identity rootfs digest", editIdentity: func(i *vm.CheckpointIdentity) { i.RootfsDigest = "sha256:other" }, want: "checkpoint rootfs digest sha256:other does not match"},
		{name: "identity runtime config digest", editIdentity: func(i *vm.CheckpointIdentity) { i.VMConfigDigest = "sha256:other" }, want: "checkpoint runtime config digest sha256:other does not match"},
		{name: "identity vcpu count", editIdentity: func(i *vm.CheckpointIdentity) { i.VMVCPUCount = 0 }, want: "checkpoint VM vCPU count 0 is invalid"},
		{name: "identity vcpu count does not match manifest", editIdentity: func(i *vm.CheckpointIdentity) { i.VMVCPUCount-- }, want: "does not match checkpoint manifest vCPU count"},
		{name: "identity cpu config digest", editIdentity: func(i *vm.CheckpointIdentity) { i.CPUConfigDigest = "sha256:other" }, want: "checkpoint guest CPU configuration digest is not canonical"},
		{name: "manifest backend", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.Backend = "test" }, want: `checkpoint manifest runtime backend "test" is not supported`},
		{name: "manifest descriptor", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.DescriptorDigest = "sha256:other" }, want: "checkpoint manifest VM runtime descriptor digest sha256:other does not match"},
		{name: "manifest arch", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.Arch = "other" }, want: `checkpoint manifest runtime arch "other" does not match`},
		{name: "manifest contract", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.Contract = "other" }, want: `checkpoint manifest runtime contract "other" does not match`},
		{name: "manifest runtime id", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.ID = "sha256:other" }, want: "checkpoint manifest runtime id sha256:other does not match"},
		{name: "manifest kernel digest", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.KernelDigest = "sha256:other" }, want: "checkpoint manifest kernel digest sha256:other does not match"},
		{name: "manifest initramfs digest", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.InitramfsDigest = "sha256:other" }, want: "checkpoint manifest initramfs digest sha256:other does not match"},
		{name: "manifest rootfs digest", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.RootfsDigest = "sha256:other" }, want: "checkpoint manifest rootfs digest sha256:other does not match"},
		{name: "manifest vcpu exceeds worker capacity", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.VCPUCount = cfg.VCPUCount + 1 }, editIdentity: func(i *vm.CheckpointIdentity) { i.VMVCPUCount = int32(cfg.VCPUCount + 1) }, want: "checkpoint manifest vcpu count"},
		{name: "manifest cpu config digest", editManifest: func(m *snapshotManifest) {
			m.RecoveryPoint.Runtime.CPUConfigDigest = testCPUConfigDigest(cfg.VCPUCount + 1)
		}, want: "does not match checkpoint manifest digest"},
		{
			name: "target cpu config digest",
			editManifest: func(m *snapshotManifest) {
				m.RecoveryPoint.Runtime.CPUConfigDigest = testCPUConfigDigest(cfg.VCPUCount + 1)
			},
			editIdentity: func(i *vm.CheckpointIdentity) {
				i.CPUConfigDigest = testCPUConfigDigest(cfg.VCPUCount + 1)
			},
			want: "does not match target digest",
		},
		{name: "manifest memory exceeds worker capacity", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.MemoryMiB = cfg.MemoryMiB + 1 }, want: "checkpoint manifest memory"},
		{name: "manifest scratch disk exceeds worker capacity", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.ScratchDiskMiB = cfg.ScratchDiskMiB + 1 }, want: "checkpoint manifest scratch disk size"},
		{name: "mismatched VM descriptor", editManifest: func(m *snapshotManifest) {
			m.RecoveryPoint.Runtime.DescriptorDigest = "sha256:" + strings.Repeat("a", 64)
		}, want: "descriptor"},
		{name: "manifest kernel args", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.KernelArgs = "other" }, want: "checkpoint manifest runtime ports or kernel args do not match"},
		{name: "manifest guest port", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.GuestPort++ }, want: "checkpoint manifest runtime ports or kernel args do not match"},
		{name: "manifest health port", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.HealthPort++ }, want: "checkpoint manifest runtime ports or kernel args do not match"},
		{name: "manifest guest ip", editManifest: func(m *snapshotManifest) { m.RuntimeState.Network.GuestIPv4CIDR = "" }, want: "checkpoint manifest guest_ipv4_cidr"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkpointID := tt.checkpointID
			if checkpointID == "" {
				checkpointID = "checkpoint-1"
			}
			manifestBytes := tt.manifest
			if manifestBytes == nil {
				manifest := validManifest
				if tt.editManifest != nil {
					tt.editManifest(&manifest)
				}
				var err error
				manifestBytes, err = json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
			}
			identity := vm.CheckpointIdentity{
				RuntimeBackend:    "firecracker",
				RuntimeID:         runtimeID,
				RuntimeArch:       testCheckpointArchitecture(t),
				VMRuntimeContract: vmplatform.Contract,
				KernelDigest:      kernelDigest,
				InitramfsDigest:   initramfsDigest,
				RootfsDigest:      rootfsDigest,
				VMConfigDigest:    sha256sum.DigestBytes(manifestBytes),
				VMVCPUCount:       int32(cfg.VCPUCount),
				CPUConfigDigest:   testCPUConfigDigest(cfg.VCPUCount),
			}
			if tt.editIdentity != nil {
				tt.editIdentity(&identity)
			}

			_, _, err := connector.validateRestoreIdentity(
				checkpointID,
				manifestBytes,
				identity,
				vm.Topology{},
				runtimeKernelArgs(vm.Topology{}, nil, cfg.NetworkResolverIPv4),
				nil,
			)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateRestoreIdentityUsesManifestRuntimeShape(t *testing.T) {
	cfg := testRestoreConfig(t)
	connector := testConnector(t, cfg)
	manifestBytes, identity := testRestoreManifestAndIdentity(t, cfg, "checkpoint-1")
	var manifest snapshotManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.RecoveryPoint.Runtime.VCPUCount = 1
	manifest.RecoveryPoint.Runtime.CPUConfigDigest = testCPUConfigDigest(1)
	manifest.RecoveryPoint.Runtime.MemoryMiB = cfg.MemoryMiB / 2
	manifest.RecoveryPoint.Runtime.ScratchDiskMiB = cfg.ScratchDiskMiB / 2
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	identity.VMConfigDigest = sha256sum.DigestBytes(manifestBytes)
	identity.VMVCPUCount = 1
	identity.CPUConfigDigest = testCPUConfigDigest(1)

	_, restoreCfg, err := connector.validateRestoreIdentity(
		"checkpoint-1",
		manifestBytes,
		identity,
		vm.Topology{},
		runtimeKernelArgs(vm.Topology{}, nil, cfg.NetworkResolverIPv4),
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if restoreCfg.VCPUCount != 1 {
		t.Fatalf("restore vcpu count = %d, want 1", restoreCfg.VCPUCount)
	}
	if restoreCfg.MemoryMiB != cfg.MemoryMiB/2 {
		t.Fatalf("restore memory = %d MiB, want %d", restoreCfg.MemoryMiB, cfg.MemoryMiB/2)
	}
	if restoreCfg.ScratchDiskMiB != cfg.ScratchDiskMiB/2 {
		t.Fatalf("restore scratch disk = %d MiB, want %d", restoreCfg.ScratchDiskMiB, cfg.ScratchDiskMiB/2)
	}
}

func TestRestoreRecordsUnpackPhasesOnFilepackFailure(t *testing.T) {
	cfg := testRestoreConfig(t)
	cfg.StateDir = t.TempDir()
	connector := testConnector(t, cfg)
	dir := t.TempDir()
	scratchRaw := filepath.Join(dir, "scratch.ext4")
	scratchPack := filepath.Join(dir, "scratch.filepack")
	memoryPack := filepath.Join(dir, "memory.filepack")
	statePath := filepath.Join(dir, "vmstate")
	if err := os.WriteFile(statePath, []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := createSparseTestFile(scratchRaw, cfg.ScratchDiskMiB*1024*1024); err != nil {
		t.Fatal(err)
	}
	if _, err := filepack.Pack(context.Background(), scratchRaw, scratchPack, filepack.ScratchRole); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memoryPack, []byte("not a filepack"), 0o600); err != nil {
		t.Fatal(err)
	}
	topology := testRestoreComputerTopology(t)
	manifestBytes, identity := testPairedRestoreManifest(t, cfg, "checkpoint-1", topology)
	computerInstanceID := uuid.NewV7().String()
	var mu sync.Mutex
	var phases []vm.Phase

	request := vm.RestoreRequest{
		Topology: topology, Resources: vm.Resources{MemoryMiB: cfg.MemoryMiB, DiskMiB: cfg.ScratchDiskMiB},
		ID:                 "checkpoint-1",
		ComputerInstanceID: computerInstanceID,
		OwnerKind:          vm.OwnerInstance,
		Binding: vm.WorkloadBinding{
			WorkerEpoch: 1, OwnerID: computerInstanceID, Generation: 1,
			ComputerInstanceID: computerInstanceID, VMPlatformID: identity.RuntimeID,
		},
		VMState:              statePath,
		VMStateMediaType:     cas.CheckpointVMStateMediaType,
		ScratchDisk:          scratchPack,
		ScratchDiskMediaType: cas.CheckpointScratchDiskMediaType,
		Memory:               []string{memoryPack},
		MemoryMediaTypes:     []string{cas.CheckpointMemoryMediaType},
		Manifest:             manifestBytes,
		Checkpoint:           identity,
		RecordPhase: func(phase vm.Phase) {
			mu.Lock()
			defer mu.Unlock()
			phases = append(phases, phase)
		},
	}
	_, err := connector.restore(context.Background(), request)

	if !errors.Is(err, filepack.ErrInvalidContent) || !strings.Contains(err.Error(), "unpack checkpoint memory") {
		t.Fatalf("err = %v, want memory unpack failure", err)
	}
	if !hasPhase(phases, "restore_validate_identity", "") {
		t.Fatalf("missing validate phase: %+v", phases)
	}
	if !hasPhase(phases, "restore_unpack_scratch_filepack", "") {
		t.Fatalf("missing scratch unpack phase: %+v", phases)
	}
	if !hasPhase(phases, "restore_unpack_memory_filepack", "io") {
		t.Fatalf("missing memory unpack failure phase: %+v", phases)
	}
	entries, readErr := os.ReadDir(cfg.StateDir)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("failed restore left owner state: entries=%v err=%v", entries, readErr)
	}
	// A fresh Connector cannot recreate a failed restore under the consumed identity.
	retry := testConnector(t, cfg)
	if _, err := retry.restore(t.Context(), request); !errors.Is(err, os.ErrExist) {
		t.Fatalf("failed restore identity was reusable: %v", err)
	}
}

func TestUnpackRestoreArtifactReturnsFilepackStats(t *testing.T) {
	cfg := testRestoreConfig(t)
	cfg.StateDir = t.TempDir()
	connector := testConnector(t, cfg)
	dir := t.TempDir()
	raw := filepath.Join(dir, "scratch.ext4")
	pack := filepath.Join(dir, "scratch.filepack")
	if err := createSparseTestFile(raw, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := filepack.Pack(context.Background(), raw, pack, filepack.ScratchRole); err != nil {
		t.Fatal(err)
	}
	owner := vm.Owner{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()}
	ownerDir, err := createOwnerStateRoot(cfg.StateDir, owner)
	if err != nil {
		t.Fatal(err)
	}

	restored, phase, err := connector.unpackRestoreArtifact(context.Background(), ownerDir, pack, filepack.ScratchRole, "scratch.ext4", 1<<20, cas.CheckpointScratchDiskMediaType)
	if err != nil {
		t.Fatal(err)
	}
	defer removeStateRootLast(ownerDir, owner)
	if filepath.Dir(restored) != ownerDir {
		t.Fatalf("restore artifact path = %q, want owner directory %q", restored, ownerDir)
	}
	if phase.Name != "restore_unpack_scratch_filepack" || phase.ErrorClass != "" || phase.Filepack == nil || phase.Filepack.LogicalBytes != 1<<20 {
		t.Fatalf("phase = %+v", phase)
	}
}

func TestWithSnapshotRestoreSkipsVsockReconfiguration(t *testing.T) {
	sdkMachine := &firecracker.Machine{}
	firecracker.WithLogger(logrus.NewEntry(logrus.New()))(sdkMachine)

	withSnapshotRestore("/checkpoint.mem", "/checkpoint.vmstate")(sdkMachine)

	if sdkMachine.Cfg.Snapshot.MemFilePath != "/checkpoint.mem" {
		t.Fatalf("memory path = %q", sdkMachine.Cfg.Snapshot.MemFilePath)
	}
	if sdkMachine.Cfg.Snapshot.SnapshotPath != "/checkpoint.vmstate" {
		t.Fatalf("state path = %q", sdkMachine.Cfg.Snapshot.SnapshotPath)
	}
	if sdkMachine.Cfg.Snapshot.EnableDiffSnapshots {
		t.Fatal("restore enabled differential snapshots")
	}
	if sdkMachine.Cfg.Snapshot.ResumeVM {
		t.Fatal("restore must load paused so network identity can be validated before resume")
	}
	if !sdkMachine.Handlers.FcInit.Has(firecracker.LoadSnapshotHandlerName) {
		t.Fatal("expected snapshot load handler")
	}
	if sdkMachine.Handlers.FcInit.Has(firecracker.AddVsocksHandlerName) {
		t.Fatal("restore must not re-add vsock devices after loading a snapshot")
	}
}

func TestConfigForRestoreManifestUsesCheckpointRuntimeShape(t *testing.T) {
	connector := &Connector{cfg: Config{VCPUCount: 4, MemoryMiB: 4096, ScratchDiskMiB: 32768}}
	cfg, err := connector.configForRestoreManifest(snapshotManifest{
		RecoveryPoint: snapshotRecoveryPointManifest{
			Runtime: snapshotRuntimeManifest{
				VCPUCount:      1,
				MemoryMiB:      1024,
				ScratchDiskMiB: 4096,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VCPUCount != 1 {
		t.Fatalf("restore vcpu count = %d, want 1", cfg.VCPUCount)
	}
	if cfg.MemoryMiB != 1024 {
		t.Fatalf("restore memory = %d MiB, want 1024", cfg.MemoryMiB)
	}
	if cfg.ScratchDiskMiB != 4096 {
		t.Fatalf("restore scratch disk = %d MiB, want 4096", cfg.ScratchDiskMiB)
	}
}

func TestConfigForRestoreManifestRejectsInvalidOrOversizedRuntimeShape(t *testing.T) {
	connector := &Connector{cfg: Config{VCPUCount: 2, MemoryMiB: 2048, ScratchDiskMiB: 8192}}
	if _, err := connector.configForRestoreManifest(snapshotManifest{RecoveryPoint: snapshotRecoveryPointManifest{Runtime: snapshotRuntimeManifest{VCPUCount: 0, MemoryMiB: 1024, ScratchDiskMiB: 4096}}}); err == nil {
		t.Fatal("expected invalid vcpu count to fail")
	}
	if _, err := connector.configForRestoreManifest(snapshotManifest{RecoveryPoint: snapshotRecoveryPointManifest{Runtime: snapshotRuntimeManifest{VCPUCount: 1, MemoryMiB: 0, ScratchDiskMiB: 4096}}}); err == nil {
		t.Fatal("expected invalid memory to fail")
	}
	if _, err := connector.configForRestoreManifest(snapshotManifest{RecoveryPoint: snapshotRecoveryPointManifest{Runtime: snapshotRuntimeManifest{VCPUCount: 1, MemoryMiB: 1024, ScratchDiskMiB: 0}}}); err == nil {
		t.Fatal("expected invalid scratch disk to fail")
	}
	if _, err := connector.configForRestoreManifest(snapshotManifest{RecoveryPoint: snapshotRecoveryPointManifest{Runtime: snapshotRuntimeManifest{VCPUCount: 3, MemoryMiB: 1024, ScratchDiskMiB: 4096}}}); err == nil {
		t.Fatal("expected oversized vcpu count to fail")
	}
	if _, err := connector.configForRestoreManifest(snapshotManifest{RecoveryPoint: snapshotRecoveryPointManifest{Runtime: snapshotRuntimeManifest{VCPUCount: 1, MemoryMiB: 4096, ScratchDiskMiB: 4096}}}); err == nil {
		t.Fatal("expected oversized memory to fail")
	}
	if _, err := connector.configForRestoreManifest(snapshotManifest{RecoveryPoint: snapshotRecoveryPointManifest{Runtime: snapshotRuntimeManifest{VCPUCount: 1, MemoryMiB: 1024, ScratchDiskMiB: 16384}}}); err == nil {
		t.Fatal("expected oversized scratch disk to fail")
	}
}

func testCheckpointArchitecture(t *testing.T) string {
	t.Helper()
	architecture, err := vmplatform.ArchitectureFromGo(runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	return architecture
}

func testRestoreManifestAndIdentity(t *testing.T, cfg Config, checkpointID string) ([]byte, vm.CheckpointIdentity) {
	t.Helper()
	kernelDigest := testDigest([]byte("kernel"))
	initramfsDigest := testDigest([]byte("initramfs"))
	rootfsDigest := testDigest([]byte("rootfs"))
	vmPlatform := testVMPlatform(t, kernelDigest, initramfsDigest, rootfsDigest)
	runtimeID := vmPlatform.ID
	descriptorDigest, err := CanonicalVMRuntimeDescriptor().Digest()
	if err != nil {
		t.Fatal(err)
	}
	manifest := snapshotManifest{
		RecoveryPoint: snapshotRecoveryPointManifest{
			ID: checkpointID,
			Runtime: snapshotRuntimeManifest{
				Backend:          "firecracker",
				DescriptorDigest: descriptorDigest,
				ID:               runtimeID,
				Arch:             vmPlatform.Arch,
				Contract:         vmPlatform.Contract,
				VCPUCount:        cfg.VCPUCount,
				CPUConfigDigest:  testCPUConfigDigest(cfg.VCPUCount),
				MemoryMiB:        cfg.MemoryMiB,
				ScratchDiskMiB:   cfg.ScratchDiskMiB,
				KernelArgs:       runtimeKernelArgs(vm.Topology{}, nil, cfg.NetworkResolverIPv4),
				KernelDigest:     kernelDigest,
				InitramfsDigest:  initramfsDigest,
				RootfsDigest:     rootfsDigest,
				GuestPort:        cfg.GuestPort,
				HealthPort:       cfg.HealthPort,
			},
		},
		RuntimeState: snapshotRuntimeStateManifest{
			Network: snapshotNetworkConfig(cfg),
		},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return manifestBytes, vm.CheckpointIdentity{
		RuntimeBackend:    "firecracker",
		RuntimeID:         runtimeID,
		RuntimeArch:       vmPlatform.Arch,
		VMRuntimeContract: vmPlatform.Contract,
		KernelDigest:      kernelDigest,
		InitramfsDigest:   initramfsDigest,
		RootfsDigest:      rootfsDigest,
		VMConfigDigest:    sha256sum.DigestBytes(manifestBytes),
		VMVCPUCount:       int32(cfg.VCPUCount),
		CPUConfigDigest:   testCPUConfigDigest(cfg.VCPUCount),
	}
}

func createSparseTestFile(path string, size int64) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	truncateErr := file.Truncate(size)
	closeErr := file.Close()
	return errors.Join(truncateErr, closeErr)
}

func hasPhase(phases []vm.Phase, name string, errorClass string) bool {
	for _, phase := range phases {
		if phase.Name != name {
			continue
		}
		if errorClass == "" || phase.ErrorClass == errorClass {
			return true
		}
	}
	return false
}

func testRestoreComputerTopology(t *testing.T) vm.Topology {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "computer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if err := file.Truncate(4096); err != nil {
		t.Fatal(err)
	}
	return vm.Topology{Computer: &vm.ComputerDisk{ComputerID: uuid.NewV7().String(), VersionID: uuid.NewV7().String(), File: file, SizeBytes: 4096}}
}
func testPairedRestoreManifest(t *testing.T, cfg Config, id string, topology vm.Topology) ([]byte, vm.CheckpointIdentity) {
	t.Helper()
	data, identity := testRestoreManifestAndIdentity(t, cfg, id)
	var manifest snapshotManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.RecoveryPoint.Runtime.KernelArgs = runtimeKernelArgs(topology, nil, cfg.NetworkResolverIPv4)
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	identity.VMConfigDigest = testDigest(data)
	return data, identity
}
func TestRestoreRejectsResourceMismatchBeforeUnpack(t *testing.T) {
	cfg := testRestoreConfig(t)
	connector := testConnector(t, cfg)
	topology := testRestoreComputerTopology(t)
	data, identity := testPairedRestoreManifest(t, cfg, "checkpoint", topology)
	id := uuid.NewV7().String()
	for _, resources := range []vm.Resources{{MemoryMiB: cfg.MemoryMiB + 1, DiskMiB: cfg.ScratchDiskMiB}, {MemoryMiB: cfg.MemoryMiB, DiskMiB: cfg.ScratchDiskMiB + 1}} {
		_, err := connector.restore(t.Context(), vm.RestoreRequest{ID: "checkpoint", ComputerInstanceID: id, OwnerKind: vm.OwnerInstance,
			Binding: vm.WorkloadBinding{WorkerEpoch: 1, OwnerID: id, Generation: 1, ComputerInstanceID: id, VMPlatformID: identity.RuntimeID}, Topology: topology, Resources: resources,
			Manifest: data, Checkpoint: identity, VMState: "not-read", VMStateMediaType: cas.CheckpointVMStateMediaType, ScratchDisk: "not-read", ScratchDiskMediaType: cas.CheckpointScratchDiskMediaType, Memory: []string{"not-read"}, MemoryMediaTypes: []string{cas.CheckpointMemoryMediaType}})
		if err == nil || !strings.Contains(err.Error(), "does not match runtime reservation") {
			t.Fatalf("error=%v", err)
		}
	}
}
