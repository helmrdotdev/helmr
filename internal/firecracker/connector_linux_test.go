//go:build linux

package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
)

var (
	_ vm.RestoringBackend     = (*QualifiedRuntime)(nil)
	_ vm.MaterializingBackend = (*QualifiedRuntime)(nil)
	_ vm.Cleaner              = (*QualifiedRuntime)(nil)
)

func TestRuntimeCandidateDoesNotExposeWorkloadInterfaces(t *testing.T) {
	typeOfCandidate := reflect.TypeOf((*Connector)(nil))
	for _, method := range []string{"Connect", "Restore", "Materialize", "Cleanup"} {
		if _, ok := typeOfCandidate.MethodByName(method); ok {
			t.Fatalf("unqualified runtime candidate exposes %s", method)
		}
	}
}

func TestRuntimeCapabilitiesRemainArtifactContentOnly(t *testing.T) {
	connector := testConnector(t, testRestoreConfig(t))
	capabilities, err := connector.runtimeCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.Arch != "x86_64" {
		t.Fatalf("runtime capabilities arch = %q, want x86_64", capabilities.Arch)
	}
	if capabilities.Contract != vmplatform.Contract || !sha256sum.ValidDigest(capabilities.KernelDigest) || !sha256sum.ValidDigest(capabilities.InitramfsDigest) || !sha256sum.ValidDigest(capabilities.RootfsDigest) {
		t.Fatalf("runtime artifact capabilities = %+v", capabilities)
	}
}

func TestProbeGuestRequiresBoundRuntimeEvidence(t *testing.T) {
	connector := &Connector{hostRuntime: newHostRuntimeEvidenceStore()}
	err := connector.probeGuest(context.Background())
	if err == nil || !strings.Contains(err.Error(), "runtime evidence is not bound") {
		t.Fatalf("ProbeGuest() error = %v", err)
	}
}

func TestMaterializeAcceptsOnlyCompleteProgramDriveSet(t *testing.T) {
	source := &recordingReadOnlyDriveSource{}
	computerInstanceID := uuid.NewV7().String()
	rootfsDigest := "sha256:" + strings.Repeat("0", 64)
	artifacts := testProbeRuntimeArtifacts()
	artifacts.Rootfs.Digest = rootfsDigest
	connector := &Connector{
		artifacts:   artifacts,
		hostRuntime: newHostRuntimeEvidenceStore(),
	}
	evidence := testHostRuntimeEvidence(t, 1, artifacts)
	if err := connector.hostRuntime.bind(evidence, 1); err != nil {
		t.Fatal(err)
	}
	request := vm.MaterializeRequest{
		ID:        computerInstanceID,
		OwnerKind: vm.OwnerInstance,
		Binding: vm.WorkloadBinding{
			WorkerEpoch: 1, OwnerID: computerInstanceID, Generation: 1,
			ComputerInstanceID: computerInstanceID, VMPlatformID: evidence.RuntimeID,
		},
		RootfsDigest:      rootfsDigest,
		ComputerMountPath: "/workspace",
		Resources:         vm.Resources{MilliCPU: 1000},
		VMVCPUCount:       1,
		CPUConfigDigest:   testCPUConfigDigest(1),
		ReadOnlyDrives:    testProgramDrives(source),
	}
	if err := connector.validateMaterializeRequest(request); err != nil {
		t.Fatal(err)
	}
	request.ReadOnlyDrives = request.ReadOnlyDrives[:1]
	if err := connector.validateMaterializeRequest(request); err == nil {
		t.Fatal("incomplete Program drive set was accepted")
	}
}

func TestMaterializeRecordsScratchDiskPhase(t *testing.T) {
	tests := []struct {
		name        string
		missingMkfs bool
		errorClass  string
	}{
		{name: "success"},
		{name: "failure", missingMkfs: true, errorClass: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := testRestoreConfig(t)
			cfg.StateDir = t.TempDir()
			cfg.ScratchDiskMiB = 16
			if test.missingMkfs {
				cfg.MkfsExt4Path = filepath.Join(t.TempDir(), "missing-mkfs.ext4")
			}
			connector := testConnector(t, cfg)
			vmPlatform, err := connector.hostRuntime.vmPlatform()
			if err != nil {
				t.Fatal(err)
			}
			cpuConfigDigest, err := connector.hostRuntime.cpuConfigDigest(1)
			if err != nil {
				t.Fatal(err)
			}
			computerInstanceID := uuid.NewV7().String()
			var phases []vm.Phase

			_, err = connector.materialize(t.Context(), vm.MaterializeRequest{
				ID:        computerInstanceID,
				OwnerKind: vm.OwnerInstance,
				Binding: vm.WorkloadBinding{
					WorkerEpoch: 1, OwnerID: computerInstanceID, Generation: 1,
					ComputerInstanceID: computerInstanceID, VMPlatformID: vmPlatform.ID,
				},
				RootfsDigest:      connector.artifacts.Rootfs.Digest,
				Resources:         vm.Resources{MilliCPU: 1000, MemoryMiB: 256, DiskMiB: 16, Slots: 1},
				VMVCPUCount:       1,
				CPUConfigDigest:   cpuConfigDigest,
				ComputerMountPath: "/workspace",
				RecordPhase: func(phase vm.Phase) {
					phases = append(phases, phase)
				},
			})
			if err == nil {
				t.Fatal("materialize unexpectedly succeeded without a Firecracker runtime")
			}
			if test.missingMkfs && !strings.Contains(err.Error(), "format scratch disk") {
				t.Fatalf("materialize error = %v, want scratch format failure", err)
			}
			var scratchPhase *vm.Phase
			for i := range phases {
				if phases[i].Name == "materialize_create_scratch_disk" {
					scratchPhase = &phases[i]
					break
				}
			}
			if scratchPhase == nil || scratchPhase.ErrorClass != test.errorClass {
				t.Fatalf("scratch phase = %+v, want error class %q; all phases: %+v", scratchPhase, test.errorClass, phases)
			}
		})
	}
}

type recordingReadOnlyDriveSource struct {
	directory string
	name      string
	uid       int
	gid       int
}

func testProgramDrives(source vm.ReadOnlyDriveSource) []vm.ReadOnlyDrive {
	return []vm.ReadOnlyDrive{
		{
			ID: vm.ProgramRuntimeDrive, Digest: "sha256:" + strings.Repeat("1", 64),
			SizeBytes: 4096, MediaType: "application/vnd.helmr.runtime.v0+squashfs",
			Source: source,
		},
		{
			ID: vm.ProgramDrive, Digest: "sha256:" + strings.Repeat("2", 64),
			SizeBytes: 4096, MediaType: "application/vnd.helmr.deployment-program.v0+squashfs",
			Source: source,
		},
	}
}

func (source *recordingReadOnlyDriveSource) LinkInto(
	directory string,
	name string,
	uid int,
	gid int,
) error {
	source.directory = directory
	source.name = name
	source.uid = uid
	source.gid = gid
	return nil
}

func testRestoreConfig(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	kernelPath := filepath.Join(dir, "kernel")
	initramfsPath := filepath.Join(dir, "initramfs")
	rootfsPath := filepath.Join(dir, "rootfs")
	if err := os.WriteFile(kernelPath, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initramfsPath, []byte("initramfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfsPath, []byte("rootfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := (Config{
		KernelPath:          kernelPath,
		InitramfsPath:       initramfsPath,
		RootfsPath:          rootfsPath,
		NetworkResolverIPv4: "10.0.0.2",
		VCPUCount:           2,
		MemoryMiB:           256,
	}).WithDefaults()
	manifest := runtimeArtifacts{
		Schema:            runtimeArtifactsSchema,
		Arch:              runtime.GOARCH,
		VMRuntimeContract: vmplatform.Contract,
		Kernel:            runtimeArtifact{Path: filepath.Base(kernelPath), Digest: testDigest([]byte("kernel")), SizeBytes: int64(len("kernel"))},
		Initramfs:         runtimeArtifact{Path: filepath.Base(initramfsPath), Digest: testDigest([]byte("initramfs")), SizeBytes: int64(len("initramfs"))},
		Rootfs:            runtimeArtifact{Path: filepath.Base(rootfsPath), Digest: testDigest([]byte("rootfs")), SizeBytes: int64(len("rootfs"))},
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.RuntimeArtifactsPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func testConnector(t *testing.T, cfg Config) *Connector {
	t.Helper()
	artifacts, err := loadRuntimeArtifacts(cfg)
	if err != nil {
		t.Fatal(err)
	}
	connector := &Connector{cfg: cfg, artifacts: artifacts, hostRuntime: newHostRuntimeEvidenceStore(), computerDevices: &sync.Map{}}
	if err := connector.hostRuntime.bind(testHostRuntimeEvidence(t, cfg.VCPUCount, artifacts), cfg.VCPUCount); err != nil {
		t.Fatal(err)
	}
	return connector
}

func testDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return sha256sum.FormatDigest(sum[:])
}
