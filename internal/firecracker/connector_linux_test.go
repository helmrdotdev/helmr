//go:build linux

package firecracker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/operations"
	"github.com/firecracker-microvm/firecracker-go-sdk/vsock"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/runtimeid"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

func TestRuntimeCapabilitiesRemainArtifactContentOnly(t *testing.T) {
	connector := testConnector(t, testRestoreConfig(t))
	capabilities, err := connector.RuntimeCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.Arch != "x86_64" {
		t.Fatalf("runtime capabilities arch = %q, want x86_64", capabilities.Arch)
	}
	if capabilities.Contract != runtimeid.Contract || !sha256sum.ValidDigest(capabilities.KernelDigest) || !sha256sum.ValidDigest(capabilities.InitramfsDigest) || !sha256sum.ValidDigest(capabilities.RootfsDigest) {
		t.Fatalf("runtime artifact capabilities = %+v", capabilities)
	}
}

func TestSnapshotRuntimeConfigIncludesNetworkTopology(t *testing.T) {
	cfg := (Config{NetworkResolverIPv4: "10.0.0.2"}).WithDefaults()
	runtimeID := testRuntimeIdentity(t, "sha256:1111111111111111111111111111111111111111111111111111111111111111", "sha256:2222222222222222222222222222222222222222222222222222222222222222", "sha256:3333333333333333333333333333333333333333333333333333333333333333").ID
	digest, manifestBytes, err := snapshotRuntimeConfig(cfg, "checkpoint-1", runtimeID, testCPUConfigDigest(cfg.VCPUCount), "sha256:1111111111111111111111111111111111111111111111111111111111111111", "sha256:2222222222222222222222222222222222222222222222222222222222222222", "sha256:3333333333333333333333333333333333333333333333333333333333333333", defaultKernelArgs, vm.RuntimeTopology{})
	if err != nil {
		t.Fatal(err)
	}
	if digest != sha256sum.DigestBytes(manifestBytes) {
		t.Fatalf("digest = %q, want %q", digest, sha256sum.DigestBytes(manifestBytes))
	}
	var manifest snapshotManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	network := manifest.RuntimeState.Network
	if network.GuestIPv4CIDR != GuestNetworkCIDRV0 || network.GuestMAC != GuestMACV0 || network.GatewayIPv4 != GuestGatewayIPv4V0 || network.GatewayMAC != GuestGatewayMACV0 || network.GuestInterfaceName != GuestInterfaceNameV0 || network.MTU != GuestMTUV0 || len(network.ResolverAddresses) != 1 || network.ResolverAddresses[0] != "10.0.0.2" {
		t.Fatalf("network = %+v", network)
	}
	if manifest.RecoveryPoint.Runtime.ID != runtimeID || manifest.RecoveryPoint.Runtime.InitramfsDigest != "sha256:2222222222222222222222222222222222222222222222222222222222222222" {
		t.Fatalf("runtime = %+v", manifest.RecoveryPoint.Runtime)
	}
	descriptorDigest, err := CanonicalVMRuntimeDescriptor().Digest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.RecoveryPoint.Runtime.DescriptorDigest != descriptorDigest {
		t.Fatalf("runtime descriptor digest = %q, want %q", manifest.RecoveryPoint.Runtime.DescriptorDigest, descriptorDigest)
	}
	if manifest.RecoveryPoint.Runtime.CPUConfigDigest != testCPUConfigDigest(cfg.VCPUCount) {
		t.Fatalf("runtime CPU config digest = %q", manifest.RecoveryPoint.Runtime.CPUConfigDigest)
	}
}

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

func TestScratchUsableFloorMatchesBuildProfiles(t *testing.T) {
	tests := []struct {
		name       string
		kernelArgs string
		want       uint64
	}{
		{name: "staged build", kernelArgs: buildKernelArgs, want: 19 * 1024 * 1024 * 1024},
		{name: "image build", kernelArgs: imageBuildKernelArgs, want: 19 * 1024 * 1024 * 1024},
		{name: "runtime", kernelArgs: defaultKernelArgs},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			connector := &Connector{kernelArgs: test.kernelArgs}
			if got := connector.scratchUsableFloor(); got != test.want {
				t.Fatalf("usable floor = %d, want %d", got, test.want)
			}
		})
	}
}

func TestSnapshotRuntimeConfigBindsManagedProgramTopology(t *testing.T) {
	cfg := (Config{NetworkResolverIPv4: "10.0.0.2"}).WithDefaults()
	runtimeID := testRuntimeIdentity(t, "sha256:1111111111111111111111111111111111111111111111111111111111111111", "sha256:2222222222222222222222222222222222222222222222222222222222222222", "sha256:3333333333333333333333333333333333333333333333333333333333333333").ID
	drives := testProgramDrives(&recordingReadOnlyDriveSource{})
	_, manifestBytes, err := snapshotRuntimeConfig(
		cfg, "checkpoint-1", runtimeID, testCPUConfigDigest(cfg.VCPUCount), "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"sha256:2222222222222222222222222222222222222222222222222222222222222222", "sha256:3333333333333333333333333333333333333333333333333333333333333333", defaultKernelArgs+" helmr.program=1", vm.RuntimeTopology{}, drives,
	)
	if err != nil {
		t.Fatal(err)
	}
	var manifest snapshotManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.RecoveryPoint.Runtime.KernelArgs != defaultKernelArgs+" helmr.program=1" {
		t.Fatalf("kernel args = %q", manifest.RecoveryPoint.Runtime.KernelArgs)
	}
	program := manifest.RecoveryPoint.Runtime.Program
	if program == nil ||
		program.Runtime.Digest != drives[0].Digest ||
		program.Artifact.Digest != drives[1].Digest {
		t.Fatalf("Program = %+v", program)
	}
	if err := validateSnapshotProgram(program, drives); err != nil {
		t.Fatal(err)
	}
	mismatch := append([]vm.ReadOnlyDrive(nil), drives...)
	mismatch[1].Digest = "sha256:" + strings.Repeat("4", 64)
	if err := validateSnapshotProgram(program, mismatch); err == nil {
		t.Fatal("mismatched Program was accepted")
	}
}

func TestCleanupRequiresCanonicalExactOwnership(t *testing.T) {
	stateDir := t.TempDir()
	jailerDir := t.TempDir()
	id := "019fc619-8443-77f6-9498-8c348c25f701"
	statePath := filepath.Join(stateDir, id)
	if err := os.MkdirAll(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "owner"), []byte(string(vm.OwnerBuild)+"\n"+id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	connector := &Connector{cfg: Config{StateDir: stateDir, JailerChrootBaseDir: jailerDir, IPPath: "/bin/true"}}
	err := connector.Cleanup(context.Background(), vm.Owner{Kind: vm.OwnerRuntime, ID: id})
	var unproven *vm.CleanupUnprovenError
	if !errors.As(err, &unproven) || unproven.Owner != (vm.Owner{Kind: vm.OwnerRuntime, ID: id}) || !strings.Contains(err.Error(), "ownership marker") {
		t.Fatalf("Cleanup() error = %v, want typed exact ownership rejection", err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("mismatched owner state was removed: %v", err)
	}
	if err := connector.Cleanup(context.Background(), vm.Owner{Kind: vm.OwnerRuntime, ID: strings.ToUpper(id)}); !errors.As(err, &unproven) {
		t.Fatal("non-canonical owner id was accepted")
	}
}

func TestCleanupRemovesExactBuildOwnerAndMarkerLast(t *testing.T) {
	stateDir := t.TempDir()
	jailerDir := t.TempDir()
	id := "019fc619-8443-77f6-9498-8c348c25f702"
	statePath := filepath.Join(stateDir, id)
	jailerPath := filepath.Join(jailerDir, "firecracker", id)
	if err := os.MkdirAll(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(jailerPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "owner"), []byte("build\n"+id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "scratch"), []byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	connector := &Connector{cfg: Config{StateDir: stateDir, JailerChrootBaseDir: jailerDir, IPPath: "/bin/true"}}
	if err := connector.Cleanup(context.Background(), vm.Owner{Kind: vm.OwnerBuild, ID: id}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{statePath, jailerPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("cleanup path remains %s: %v", path, err)
		}
	}
}

func TestSnapshotRuntimeConfigIncludesSubstrateIdentity(t *testing.T) {
	cfg := (Config{NetworkResolverIPv4: "10.0.0.2"}).WithDefaults()
	topology := vm.RuntimeTopology{Substrate: &vm.RuntimeSubstrate{
		Path:     filepath.Join(t.TempDir(), "substrate.ext4"),
		Digest:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Format:   "ext4",
		Contract: "builder-v1",
	}}
	runtimeID := testRuntimeIdentity(t, "sha256:1111111111111111111111111111111111111111111111111111111111111111", "sha256:2222222222222222222222222222222222222222222222222222222222222222", "sha256:3333333333333333333333333333333333333333333333333333333333333333").ID
	_, manifestBytes, err := snapshotRuntimeConfig(cfg, "checkpoint-1", runtimeID, testCPUConfigDigest(cfg.VCPUCount), "sha256:1111111111111111111111111111111111111111111111111111111111111111", "sha256:2222222222222222222222222222222222222222222222222222222222222222", "sha256:3333333333333333333333333333333333333333333333333333333333333333", defaultKernelArgs+" helmr.substrate=1", topology)
	if err != nil {
		t.Fatal(err)
	}
	var manifest snapshotManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	substrate := manifest.RecoveryPoint.Runtime.Substrate
	if substrate == nil {
		t.Fatal("substrate manifest is nil")
	}
	if substrate.Digest != topology.Substrate.Digest || substrate.Format != "ext4" || substrate.Contract != "builder-v1" {
		t.Fatalf("substrate = %+v", substrate)
	}
}

func TestValidateRuntimeSubstrateManifestRequiresExactTopologyMatch(t *testing.T) {
	manifest := &snapshotRuntimeSubstrate{
		Digest:   "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Format:   "ext4",
		Contract: "builder-v1",
	}
	expected := &vm.RuntimeSubstrate{
		Path:     filepath.Join(t.TempDir(), "substrate.ext4"),
		Digest:   manifest.Digest,
		Format:   manifest.Format,
		Contract: manifest.Contract,
	}
	if err := validateRuntimeSubstrateManifest(manifest, expected); err != nil {
		t.Fatal(err)
	}
	if err := validateRuntimeSubstrateManifest(manifest, nil); err == nil || !strings.Contains(err.Error(), "requires runtime substrate") {
		t.Fatalf("missing expected substrate err = %v", err)
	}
	mismatch := *expected
	mismatch.Digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := validateRuntimeSubstrateManifest(manifest, &mismatch); err == nil || !strings.Contains(err.Error(), "substrate digest") {
		t.Fatalf("digest mismatch err = %v", err)
	}
	if err := validateRuntimeSubstrateManifest(nil, expected); err == nil || !strings.Contains(err.Error(), "provided one") {
		t.Fatalf("unexpected provided substrate err = %v", err)
	}
}

func TestIgnoreExpectedStopErrorsDropsFirecrackerSIGTERM(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatal("waitErr = nil, want signal error")
	}
	if err := ignoreExpectedStopErrors(waitErr); err != nil {
		t.Fatalf("ignoreExpectedStopErrors = %v, want nil", err)
	}

	cleanupErr := os.ErrPermission
	if err := ignoreExpectedStopErrors(testWrappedErrors{waitErr, cleanupErr}); !errors.Is(err, cleanupErr) {
		t.Fatalf("ignoreExpectedStopErrors wrapped = %v, want %v", err, cleanupErr)
	}
}

func TestIgnoreStopSignalErrorDropsForcedSIGKILL(t *testing.T) {
	cmd := exec.Command("sleep", "10")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitErr := cmd.Wait()
	if waitErr == nil {
		t.Fatal("waitErr = nil, want signal error")
	}
	if err := ignoreStopSignalError(waitErr, syscall.SIGKILL); err != nil {
		t.Fatalf("ignoreStopSignalError = %v, want nil", err)
	}
	if err := ignoreExpectedStopErrors(waitErr); err == nil {
		t.Fatal("ignoreExpectedStopErrors ignored SIGKILL outside force-kill path")
	}
}

func TestCleanupGuestSessionResourcesRunsAfterStopError(t *testing.T) {
	called := false
	cleanupGuestSessionResources(func() { called = true })
	if !called {
		t.Fatal("cleanup did not run")
	}
}

type testWrappedErrors []error

func (e testWrappedErrors) Error() string {
	return "wrapped errors"
}

func (e testWrappedErrors) WrappedErrors() []error {
	return []error(e)
}

func TestSnapshotRuntimeConfigRequiresResolver(t *testing.T) {
	cfg := (Config{}).WithDefaults()
	runtimeID := testRuntimeIdentity(t, "sha256:1111111111111111111111111111111111111111111111111111111111111111", "sha256:2222222222222222222222222222222222222222222222222222222222222222", "sha256:3333333333333333333333333333333333333333333333333333333333333333").ID
	_, _, err := snapshotRuntimeConfig(cfg, "checkpoint-1", runtimeID, testCPUConfigDigest(cfg.VCPUCount), "sha256:1111111111111111111111111111111111111111111111111111111111111111", "sha256:2222222222222222222222222222222222222222222222222222222222222222", "sha256:3333333333333333333333333333333333333333333333333333333333333333", defaultKernelArgs, vm.RuntimeTopology{})
	if err == nil {
		t.Fatal("expected missing resolver error")
	}
}

func TestStaticNetworkInterfaceMatchesVMRuntimeContract(t *testing.T) {
	iface := staticNetworkInterface("10.0.0.2")
	if iface.StaticConfiguration == nil || iface.StaticConfiguration.IPConfiguration == nil {
		t.Fatalf("interface = %+v", iface)
	}
	static := iface.StaticConfiguration
	if static.HostDevName != GuestTapNameV0 || static.MacAddress != GuestMACV0 || static.IPConfiguration.IPAddr.String() != GuestNetworkCIDRV0 || static.IPConfiguration.Gateway.String() != GuestGatewayIPv4V0 || static.IPConfiguration.IfName != GuestInterfaceNameV0 || len(static.IPConfiguration.Nameservers) != 1 || static.IPConfiguration.Nameservers[0] != "10.0.0.2" {
		t.Fatalf("static interface = %+v", static)
	}
}

func TestValidateRestoredNetworkConfigRequiresExactGuestVisibleIdentity(t *testing.T) {
	expected := snapshotNetworkConfig(Config{NetworkResolverIPv4: "10.0.0.2"})
	if err := validateRestoredNetworkConfig(expected, expected); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func(*snapshotNetworkManifest)
	}{
		{name: "guest IP", edit: func(network *snapshotNetworkManifest) {
			network.GuestIPv4CIDR = "192.168.127.3/30"
		}},
		{name: "guest MAC", edit: func(network *snapshotNetworkManifest) {
			network.GuestMAC = "06:00:ac:10:00:03"
		}},
		{name: "gateway", edit: func(network *snapshotNetworkManifest) {
			network.GatewayIPv4 = "192.168.127.254"
		}},
		{name: "gateway MAC", edit: func(network *snapshotNetworkManifest) {
			network.GatewayMAC = "02:fc:00:00:00:03"
		}},
		{name: "resolver", edit: func(network *snapshotNetworkManifest) {
			network.ResolverAddresses = []string{"10.0.0.3"}
		}},
		{name: "guest interface", edit: func(network *snapshotNetworkManifest) {
			network.GuestInterfaceName = "eth1"
		}},
		{name: "mtu", edit: func(network *snapshotNetworkManifest) {
			network.MTU++
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := expected
			actual.ResolverAddresses = append([]string(nil), expected.ResolverAddresses...)
			test.edit(&actual)
			if err := validateRestoredNetworkConfig(expected, actual); err == nil {
				t.Fatal("expected network mismatch")
			}
		})
	}
}

func TestDefaultKernelArgsDeclareSquashFSRoot(t *testing.T) {
	if !strings.Contains(defaultKernelArgs, "rootfstype=squashfs") {
		t.Fatalf("defaultKernelArgs = %q", defaultKernelArgs)
	}
}

func TestCloneSparseFilePreservesSparseExtents(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.raw")
	dest := filepath.Join(dir, "dest.raw")
	const logicalSize = int64(64 << 20)
	const dataOffset = int64(32 << 20)
	payload := bytes.Repeat([]byte("x"), 4096)

	file, err := os.OpenFile(source, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(logicalSize); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if _, err := file.WriteAt(payload, dataOffset); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	if err := cloneSparseFile(source, dest); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != logicalSize {
		t.Fatalf("dest size = %d, want %d", info.Size(), logicalSize)
	}
	destFile, err := os.Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer destFile.Close()
	read := make([]byte, len(payload))
	if _, err := destFile.ReadAt(read, dataOffset); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read, payload) {
		t.Fatalf("copied payload mismatch")
	}
	if allocatedBytes(t, dest) > logicalSize/8 {
		t.Fatalf("dest was copied densely: allocated=%d logical=%d", allocatedBytes(t, dest), logicalSize)
	}
}

func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		t.Fatal(err)
	}
	return stat.Blocks * 512
}

func TestValidateRestoreIdentityRejectsManifestMismatch(t *testing.T) {
	cfg := testRestoreConfig(t)
	kernelDigest := testDigest([]byte("kernel"))
	initramfsDigest := testDigest([]byte("initramfs"))
	rootfsDigest := testDigest([]byte("rootfs"))
	runtimeID := testRuntimeIdentity(t, kernelDigest, initramfsDigest, rootfsDigest).ID
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
				Contract:         runtimeid.Contract,
				VCPUCount:        cfg.VCPUCount,
				CPUConfigDigest:  testCPUConfigDigest(cfg.VCPUCount),
				MemoryMiB:        cfg.MemoryMiB,
				ScratchDiskMiB:   cfg.ScratchDiskMiB,
				KernelArgs:       defaultKernelArgs,
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
		{name: "identity runtime config digest", editIdentity: func(i *vm.CheckpointIdentity) { i.RuntimeConfigDigest = "sha256:other" }, want: "checkpoint runtime config digest sha256:other does not match"},
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
				RuntimeBackend:      "firecracker",
				RuntimeID:           runtimeID,
				RuntimeArch:         testCheckpointArchitecture(t),
				VMRuntimeContract:   runtimeid.Contract,
				KernelDigest:        kernelDigest,
				InitramfsDigest:     initramfsDigest,
				RootfsDigest:        rootfsDigest,
				RuntimeConfigDigest: sha256sum.DigestBytes(manifestBytes),
				VMVCPUCount:         int32(cfg.VCPUCount),
				CPUConfigDigest:     testCPUConfigDigest(cfg.VCPUCount),
			}
			if tt.editIdentity != nil {
				tt.editIdentity(&identity)
			}

			_, _, err := connector.validateRestoreIdentity(
				checkpointID,
				manifestBytes,
				identity,
				vm.RuntimeTopology{},
				defaultKernelArgs,
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
	identity.RuntimeConfigDigest = sha256sum.DigestBytes(manifestBytes)
	identity.VMVCPUCount = 1
	identity.CPUConfigDigest = testCPUConfigDigest(1)

	_, restoreCfg, err := connector.validateRestoreIdentity(
		"checkpoint-1",
		manifestBytes,
		identity,
		vm.RuntimeTopology{},
		defaultKernelArgs,
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
	if _, err := packRuntimeFile(context.Background(), scratchRaw, scratchPack, filepackScratchRole); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(memoryPack, []byte("not a filepack"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifestBytes, identity := testRestoreManifestAndIdentity(t, cfg, "checkpoint-1")
	runtimeInstanceID := uuid.Must(uuid.NewV7()).String()
	var mu sync.Mutex
	var phases []vm.RuntimePhase

	_, err := connector.Restore(context.Background(), vm.RestoreRequest{
		ID:                "checkpoint-1",
		RuntimeInstanceID: runtimeInstanceID,
		OwnerKind:         vm.OwnerRuntime,
		Binding: vm.WorkloadBinding{
			WorkerEpoch: 1, OwnerID: runtimeInstanceID, Generation: 1,
			RuntimeInstanceID: runtimeInstanceID, RuntimeIdentityID: identity.RuntimeID,
		},
		VMState:              statePath,
		VMStateMediaType:     cas.CheckpointVMStateMediaType,
		ScratchDisk:          scratchPack,
		ScratchDiskMediaType: cas.CheckpointScratchDiskMediaType,
		Memory:               []string{memoryPack},
		MemoryMediaTypes:     []string{cas.CheckpointMemoryMediaType},
		Manifest:             manifestBytes,
		Checkpoint:           identity,
		RecordPhase: func(phase vm.RuntimePhase) {
			mu.Lock()
			defer mu.Unlock()
			phases = append(phases, phase)
		},
	})

	if err == nil || !strings.Contains(err.Error(), "unpack checkpoint memory") {
		t.Fatalf("err = %v, want memory unpack failure", err)
	}
	if !hasRuntimePhase(phases, "restore_validate_identity", "") {
		t.Fatalf("missing validate phase: %+v", phases)
	}
	if !hasRuntimePhase(phases, "restore_unpack_scratch_filepack", "") {
		t.Fatalf("missing scratch unpack phase: %+v", phases)
	}
	if !hasRuntimePhase(phases, "restore_unpack_memory_filepack", "io") {
		t.Fatalf("missing memory unpack failure phase: %+v", phases)
	}
	entries, readErr := os.ReadDir(cfg.StateDir)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("failed restore left owner state: entries=%v err=%v", entries, readErr)
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
	if _, err := packRuntimeFile(context.Background(), raw, pack, filepackScratchRole); err != nil {
		t.Fatal(err)
	}
	owner := vm.Owner{Kind: vm.OwnerRuntime, ID: uuid.Must(uuid.NewV7()).String()}
	ownerDir, err := createOwnerStateRoot(cfg.StateDir, owner)
	if err != nil {
		t.Fatal(err)
	}

	restored, phase, err := connector.unpackRestoreArtifact(context.Background(), ownerDir, pack, filepackScratchRole, "scratch.ext4", 1<<20, cas.CheckpointScratchDiskMediaType)
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

func TestReadHealthSendsHTTPRequest(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	errc := make(chan error, 1)
	go func() {
		req, err := http.ReadRequest(bufio.NewReader(server))
		if err != nil {
			errc <- err
			return
		}
		if req.Method != http.MethodGet || req.URL.Path != "/" {
			t.Errorf("request = %s %s", req.Method, req.URL.Path)
		}
		_, err = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 36\r\nConnection: close\r\n\r\n{\"status\":\"ok\",\"component\":\"guestd\"}")
		errc <- err
	}()

	response, err := readHealth(client)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != "ok" || response.Component != "guestd" {
		t.Fatalf("response = %+v", response)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestWaitForHealthRetriesTransientReadFailure(t *testing.T) {
	previousDial := dialVsock
	defer func() { dialVsock = previousDial }()

	attempts := 0
	dialVsock = func(context.Context, string, uint32, ...vsock.DialOption) (net.Conn, error) {
		attempts++
		client, server := net.Pipe()
		if attempts == 1 {
			_ = server.Close()
			return client, nil
		}
		go func() {
			defer server.Close()
			if _, err := http.ReadRequest(bufio.NewReader(server)); err != nil {
				return
			}
			_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 36\r\nConnection: close\r\n\r\n{\"status\":\"ok\",\"component\":\"guestd\"}")
		}()
		return client, nil
	}

	connector := &Connector{cfg: (Config{HealthTimeout: time.Second}).WithDefaults()}
	if err := connector.waitForHealth(context.Background(), "vsock.sock", nil, nil); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("dial attempts = %d, want 2", attempts)
	}
}

func TestWaitForHealthRetriesStalledReadWithAttemptDeadline(t *testing.T) {
	previousDial := dialVsock
	defer func() { dialVsock = previousDial }()

	attempts := 0
	dialVsock = func(context.Context, string, uint32, ...vsock.DialOption) (net.Conn, error) {
		attempts++
		client, server := net.Pipe()
		if attempts == 1 {
			go func() {
				defer server.Close()
				_, _ = http.ReadRequest(bufio.NewReader(server))
				time.Sleep(100 * time.Millisecond)
			}()
			return client, nil
		}
		go func() {
			defer server.Close()
			if _, err := http.ReadRequest(bufio.NewReader(server)); err != nil {
				return
			}
			_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 36\r\nConnection: close\r\n\r\n{\"status\":\"ok\",\"component\":\"guestd\"}")
		}()
		return client, nil
	}

	var logs []string
	logf := func(format string, args ...interface{}) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	connector := &Connector{cfg: (Config{HealthTimeout: time.Second, HealthAttemptTimeout: 20 * time.Millisecond}).WithDefaults()}
	if err := connector.waitForHealth(context.Background(), "vsock.sock", nil, logf); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("dial attempts = %d, want 2", attempts)
	}
	if !strings.Contains(strings.Join(logs, "\n"), `bucket="read"`) {
		t.Fatalf("logs = %v, want read bucket attempt log", logs)
	}
}

func TestWaitForHealthClassifiesUnbufferedStalledWriteWithAttemptDeadline(t *testing.T) {
	previousDial := dialVsock
	defer func() { dialVsock = previousDial }()

	dialVsock = func(context.Context, string, uint32, ...vsock.DialOption) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			time.Sleep(200 * time.Millisecond)
		}()
		return client, nil
	}

	connector := &Connector{cfg: (Config{HealthTimeout: 80 * time.Millisecond, HealthAttemptTimeout: 20 * time.Millisecond}).WithDefaults()}
	err := connector.waitForHealth(context.Background(), "vsock.sock", nil, nil)
	if err == nil {
		t.Fatal("waitForHealth error = nil, want timeout")
	}
	text := err.Error()
	if !strings.Contains(text, "write_errors=") || !strings.Contains(text, `last_bucket="write"`) {
		t.Fatalf("waitForHealth error = %v, want write bucket summary", err)
	}
}

func TestWaitForHealthAppliesAttemptDeadlineToDial(t *testing.T) {
	previousDial := dialVsock
	defer func() { dialVsock = previousDial }()

	attempts := 0
	sawAttemptDeadline := false
	dialVsock = func(ctx context.Context, _ string, _ uint32, _ ...vsock.DialOption) (net.Conn, error) {
		attempts++
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("dial context has no deadline")
		}
		if remaining := time.Until(deadline); remaining > 80*time.Millisecond {
			t.Fatalf("dial context deadline remaining = %s, want attempt-scoped deadline", remaining)
		}
		sawAttemptDeadline = true
		<-ctx.Done()
		return nil, fmt.Errorf("dial blocked: %w", ctx.Err())
	}

	connector := &Connector{cfg: (Config{HealthTimeout: 120 * time.Millisecond, HealthAttemptTimeout: 20 * time.Millisecond}).WithDefaults()}
	err := connector.waitForHealth(context.Background(), "vsock.sock", nil, nil)
	if err == nil {
		t.Fatal("waitForHealth error = nil, want timeout")
	}
	if attempts < 1 {
		t.Fatalf("dial attempts = %d, want at least 1", attempts)
	}
	if !sawAttemptDeadline {
		t.Fatal("dial context was not checked")
	}
	text := err.Error()
	if !strings.Contains(text, "dial_errors=") || !strings.Contains(text, `last_bucket="dial"`) {
		t.Fatalf("waitForHealth error = %v, want dial bucket summary", err)
	}
}

func TestWaitForHealthLogsTerminalStatusWithoutStaleError(t *testing.T) {
	previousDial := dialVsock
	defer func() { dialVsock = previousDial }()

	dialVsock = func(context.Context, string, uint32, ...vsock.DialOption) (net.Conn, error) {
		client, server := net.Pipe()
		go func() {
			defer server.Close()
			if _, err := http.ReadRequest(bufio.NewReader(server)); err != nil {
				return
			}
			body := `{"status":"degraded","component":"guestd"}`
			_, _ = fmt.Fprintf(server, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		}()
		return client, nil
	}

	var logs []string
	logf := func(format string, args ...interface{}) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	connector := &Connector{cfg: (Config{HealthTimeout: time.Second}).WithDefaults()}
	err := connector.waitForHealth(context.Background(), "vsock.sock", nil, logf)
	if err == nil {
		t.Fatal("waitForHealth error = nil, want terminal status error")
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, `bucket="status"`) || !strings.Contains(joined, `error="guest health status=\"degraded\""`) {
		t.Fatalf("logs = %v, want status bucket with current status error", logs)
	}
}

func TestWaitForHealthReportsMachineExit(t *testing.T) {
	exit := &machineExit{done: make(chan struct{})}
	exit.err = errors.New("vm exited")
	close(exit.done)

	connector := &Connector{cfg: (Config{HealthTimeout: time.Second}).WithDefaults()}
	err := connector.waitForHealth(context.Background(), "vsock.sock", exit, nil)
	if err == nil {
		t.Fatal("waitForHealth error = nil, want machine exit error")
	}
	if !strings.Contains(err.Error(), "the Firecracker machine exited during guest health wait") {
		t.Fatalf("waitForHealth error = %v, want Firecracker machine exit context", err)
	}
	if !strings.Contains(err.Error(), "machine_exited=true") {
		t.Fatalf("waitForHealth error = %v, want machine_exited summary", err)
	}
}

func TestConnectGuestPortReturnsMachineExitWithoutHealthTimeout(t *testing.T) {
	previousDial := dialVsock
	defer func() { dialVsock = previousDial }()

	dialEntered := make(chan struct{})
	dialVsock = func(ctx context.Context, _ string, _ uint32, _ ...vsock.DialOption) (net.Conn, error) {
		close(dialEntered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	exit := &machineExit{done: make(chan struct{})}
	connector := &Connector{cfg: (Config{HealthTimeout: time.Minute}).WithDefaults()}

	result := make(chan error, 1)
	go func() {
		_, err := connector.connectGuestPort(context.Background(), "vsock.sock", exit)
		result <- err
	}()
	select {
	case <-dialEntered:
	case <-time.After(time.Second):
		t.Fatal("connectGuestPort did not enter dial")
	}
	exit.err = errors.New("vm exited")
	close(exit.done)

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "the Firecracker machine exited before guest port") {
			t.Fatalf("connectGuestPort error = %v, want Firecracker machine exit", err)
		}
	case <-time.After(time.Second):
		t.Fatal("connectGuestPort waited after machine exit")
	}
}

func TestConnectGuestPortUsesFullDuplexStream(t *testing.T) {
	previousDial := dialVsock
	defer func() { dialVsock = previousDial }()

	client, server := net.Pipe()
	defer server.Close()
	dialVsock = func(context.Context, string, uint32, ...vsock.DialOption) (net.Conn, error) {
		return client, nil
	}

	connector := &Connector{cfg: (Config{HealthTimeout: time.Second}).WithDefaults()}
	stream, err := connector.connectGuestPort(context.Background(), "vsock.sock", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	errChannel := make(chan error, 1)
	go func() {
		request := make([]byte, len("request"))
		if _, err := io.ReadFull(server, request); err != nil {
			errChannel <- err
			return
		}
		if string(request) != "request" {
			errChannel <- fmt.Errorf("request = %q, want request", request)
			return
		}
		_, err := io.WriteString(server, "response")
		errChannel <- err
	}()
	if _, err := io.WriteString(stream, "request"); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len("response"))
	if _, err := io.ReadFull(stream, response); err != nil {
		t.Fatal(err)
	}
	if string(response) != "response" {
		t.Fatalf("response = %q, want response", response)
	}
	if err := <-errChannel; err != nil {
		t.Fatal(err)
	}
}

func TestReadHealthRejectsOversizedBody(t *testing.T) {
	client, server := net.Pipe()
	errc := make(chan error, 1)
	go func() {
		defer server.Close()
		if _, err := http.ReadRequest(bufio.NewReader(server)); err != nil {
			errc <- err
			return
		}
		body := strings.Repeat("x", maxGuestHealthResponseBytes+1)
		_, err := fmt.Fprintf(server, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
		errc <- err
	}()

	_, err := readHealth(client)
	if err == nil {
		t.Fatal("readHealth error = nil, want oversized body error")
	}
	if !strings.Contains(err.Error(), "body exceeds") {
		t.Fatalf("readHealth error = %v, want body size context", err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestReadHealthAcceptsChunkedBody(t *testing.T) {
	client, server := net.Pipe()
	errc := make(chan error, 1)
	go func() {
		defer server.Close()
		if _, err := http.ReadRequest(bufio.NewReader(server)); err != nil {
			errc <- err
			return
		}
		_, err := io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n24\r\n{\"status\":\"ok\",\"component\":\"guestd\"}\r\n0\r\n\r\n")
		errc <- err
	}()

	response, err := readHealth(client)
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != "ok" || response.Component != "guestd" {
		t.Fatalf("response = %+v", response)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestPreparedGuestSeparatesHealthFromGuestPort(t *testing.T) {
	previousDial := dialVsock
	defer func() { dialVsock = previousDial }()

	var ports []uint32
	dialVsock = func(_ context.Context, _ string, port uint32, _ ...vsock.DialOption) (net.Conn, error) {
		ports = append(ports, port)
		client, server := net.Pipe()
		if port == uint32((Config{}).WithDefaults().HealthPort) {
			if len(ports) == 1 {
				_ = server.Close()
				return client, nil
			}
			go func() {
				defer server.Close()
				if _, err := http.ReadRequest(bufio.NewReader(server)); err != nil {
					return
				}
				_, _ = io.WriteString(server, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 36\r\nConnection: close\r\n\r\n{\"status\":\"ok\",\"component\":\"guestd\"}")
			}()
			return client, nil
		}
		go func() {
			<-time.After(10 * time.Millisecond)
			_ = server.Close()
		}()
		return client, nil
	}

	connector := &Connector{cfg: (Config{HealthTimeout: time.Second}).WithDefaults()}
	if err := connector.waitForHealth(
		context.Background(),
		"vsock.sock",
		nil,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if len(ports) != 2 {
		t.Fatalf("ports after health = %v, want health only", ports)
	}
	conn, err := connector.connectGuestPort(
		context.Background(),
		"vsock.sock",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	want := []uint32{uint32(connector.cfg.HealthPort), uint32(connector.cfg.HealthPort), uint32(connector.cfg.GuestPort)}
	if len(ports) != len(want) {
		t.Fatalf("ports = %v, want %v", ports, want)
	}
	for i := range want {
		if ports[i] != want[i] {
			t.Fatalf("ports = %v, want %v", ports, want)
		}
	}
}

func TestCopySparseRangeRejectsShortRead(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input.raw")
	outputPath := filepath.Join(dir, "output.raw")
	if err := os.WriteFile(inputPath, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	buffer := bytes.Repeat([]byte{0xff}, 16)

	if err := copySparseRange(input, output, buffer, 0, 16); err == nil {
		t.Fatal("copy succeeded with short read")
	}
}

func TestJailRootPath(t *testing.T) {
	cfg := (Config{
		FirecrackerPath:     "/usr/bin/firecracker",
		JailerChrootBaseDir: "/var/lib/helmr/jailer",
	}).WithDefaults()
	got := jailRootPath(cfg, "vm-1")
	want := "/var/lib/helmr/jailer/firecracker/vm-1/root"
	if got != want {
		t.Fatalf("jail root = %q, want %q", got, want)
	}
}

func TestLinkIntoJailSetsOwnerAndMode(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("requires root to verify chown")
	}
	source := filepath.Join(t.TempDir(), "snapshot.mem")
	if err := os.WriteFile(source, []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := linkIntoJailForVMM(source, root, "snapshot.mem", os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "snapshot.mem"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestWithJailedRestoreFilesLinksScratchDiskAndRewritesDrivePaths(t *testing.T) {
	chrootBase := t.TempDir()
	vmID := "vm-1"
	root := filepath.Join(chrootBase, "firecracker", vmID, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceDir := t.TempDir()
	rootfsPath := filepath.Join(sourceDir, "rootfs.squashfs")
	scratchDiskPath := filepath.Join(sourceDir, "restored-scratch.ext4")
	substrateDiskPath := filepath.Join(sourceDir, "9270959e49b0181ace5338d3acce327260b9e46d6f3827402dfca962a5189126.ext4")
	memoryPath := filepath.Join(sourceDir, "checkpoint.mem")
	statePath := filepath.Join(sourceDir, "checkpoint.vmstate")
	for _, path := range []string{rootfsPath, scratchDiskPath, substrateDiskPath, memoryPath, statePath} {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	machine := &firecracker.Machine{
		Cfg: firecracker.Config{
			JailerCfg: &firecracker.JailerConfig{
				ExecFile:      "/usr/bin/firecracker",
				ChrootBaseDir: chrootBase,
				ID:            vmID,
				UID:           firecracker.Int(os.Getuid()),
				GID:           firecracker.Int(os.Getgid()),
			},
			Drives: []models.Drive{{
				DriveID:    firecracker.String("rootfs"),
				PathOnHost: firecracker.String(rootfsPath),
			}, {
				DriveID:    firecracker.String("scratch"),
				PathOnHost: firecracker.String(scratchDiskPath),
			}, {
				DriveID:    firecracker.String("substrate"),
				PathOnHost: firecracker.String(substrateDiskPath),
			}},
			Snapshot: firecracker.SnapshotConfig{},
		},
		Handlers: firecracker.Handlers{
			FcInit: firecracker.HandlerList{}.Append(firecracker.Handler{
				Name: firecracker.CreateLogFilesHandlerName,
				Fn: func(context.Context, *firecracker.Machine) error {
					return nil
				},
			}),
		},
	}
	firecracker.WithLogger(logrus.NewEntry(logrus.New()))(machine)
	opt := withJailedRestoreFiles(rootfsPath, scratchDiskPath, substrateDiskPath, memoryPath, statePath)
	opt(machine)
	if err := machine.Handlers.FcInit.Run(context.Background(), machine); err != nil {
		t.Fatal(err)
	}

	if got := firecracker.StringValue(machine.Cfg.Drives[0].PathOnHost); got != filepath.Base(rootfsPath) {
		t.Fatalf("rootfs drive path = %q", got)
	}
	if got := firecracker.StringValue(machine.Cfg.Drives[1].PathOnHost); got != scratchDiskName {
		t.Fatalf("scratch drive path = %q", got)
	}
	substrateName := filepath.Base(substrateDiskPath)
	if got := firecracker.StringValue(machine.Cfg.Drives[2].PathOnHost); got != substrateName {
		t.Fatalf("substrate drive path = %q", got)
	}
	for _, name := range []string{filepath.Base(rootfsPath), scratchDiskName, substrateName, filepath.Base(memoryPath), filepath.Base(statePath)} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("expected %s linked into jail: %v", name, err)
		}
	}
}

func TestRuntimeDrivesIncludeOptionalReadonlySubstrate(t *testing.T) {
	drives := runtimeDrives("/rootfs.squashfs", "/scratch.ext4", "/substrate.ext4", nil)
	if len(drives) != 3 {
		t.Fatalf("drive count = %d, want 3", len(drives))
	}
	if got := firecracker.StringValue(drives[2].DriveID); got != "substrate" {
		t.Fatalf("substrate drive id = %q", got)
	}
	if !firecracker.BoolValue(drives[2].IsReadOnly) {
		t.Fatalf("substrate drive should be readonly")
	}
	if firecracker.BoolValue(drives[2].IsRootDevice) {
		t.Fatalf("substrate drive must not be root device")
	}
}

func TestRuntimeDrivesIncludeSealedReadOnlyDrives(t *testing.T) {
	source := &recordingReadOnlyDriveSource{}
	drives := runtimeDrives(
		"/rootfs.squashfs",
		"/scratch.ext4",
		"",
		[]vm.ReadOnlyDrive{{ID: vm.ProgramDrive, Source: source}},
	)
	if len(drives) != 3 {
		t.Fatalf("drive count = %d, want 3", len(drives))
	}
	if got := firecracker.StringValue(drives[2].DriveID); got != vm.ProgramDrive {
		t.Fatalf("program drive id = %q", got)
	}
	if got := firecracker.StringValue(drives[2].PathOnHost); got != "program.squashfs" {
		t.Fatalf("program drive path = %q", got)
	}
	if !firecracker.BoolValue(drives[2].IsReadOnly) {
		t.Fatal("program drive should be read-only")
	}
}

func TestRuntimeDrivesUseFixedProgramOrder(t *testing.T) {
	source := &recordingReadOnlyDriveSource{}
	drives := runtimeDrives(
		"/rootfs.squashfs",
		"/scratch.ext4",
		"/workspace.ext4",
		[]vm.ReadOnlyDrive{
			{ID: vm.ProgramDrive, Source: source},
			{ID: vm.ProgramRuntimeDrive, Source: source},
		},
	)
	want := []string{
		"rootfs",
		"scratch",
		"substrate",
		vm.ProgramRuntimeDrive,
		vm.ProgramDrive,
	}
	if len(drives) != len(want) {
		t.Fatalf("drive count = %d, want %d", len(drives), len(want))
	}
	for index, drive := range drives {
		if got := firecracker.StringValue(drive.DriveID); got != want[index] {
			t.Fatalf("drive %d ID = %q, want %q", index, got, want[index])
		}
	}
}

func TestRuntimeKernelArgsDescribeExactDriveTopology(t *testing.T) {
	source := &recordingReadOnlyDriveSource{}
	program := []vm.ReadOnlyDrive{
		{ID: vm.ProgramRuntimeDrive, Source: source},
		{ID: vm.ProgramDrive, Source: source},
	}
	if got := runtimeKernelArgs(vm.RuntimeTopology{}, nil); got != defaultKernelArgs {
		t.Fatalf("default args = %q", got)
	}
	if got := runtimeKernelArgs(
		vm.RuntimeTopology{Substrate: &vm.RuntimeSubstrate{}},
		nil,
	); got != defaultKernelArgs+" helmr.substrate=1" {
		t.Fatalf("substrate args = %q", got)
	}
	if got := runtimeKernelArgs(
		vm.RuntimeTopology{Substrate: &vm.RuntimeSubstrate{}},
		program,
	); got != defaultKernelArgs+" helmr.substrate=1 helmr.program=1" {
		t.Fatalf("Program args = %q", got)
	}
}

func TestMaterializeAcceptsOnlyCompleteProgramDriveSet(t *testing.T) {
	source := &recordingReadOnlyDriveSource{}
	runtimeInstanceID := uuid.Must(uuid.NewV7()).String()
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
		ID:        runtimeInstanceID,
		OwnerKind: vm.OwnerRuntime,
		Binding: vm.WorkloadBinding{
			WorkerEpoch: 1, OwnerID: runtimeInstanceID, Generation: 1,
			RuntimeInstanceID: runtimeInstanceID, RuntimeIdentityID: evidence.RuntimeID,
		},
		RootfsDigest:       rootfsDigest,
		WorkspaceMountPath: "/workspace",
		Resources:          compute.ResourceVector{MilliCPU: 1000},
		VMVCPUCount:        1,
		CPUConfigDigest:    testCPUConfigDigest(1),
		ReadOnlyDrives:     testProgramDrives(source),
	}
	if err := connector.validateMaterializeRequest(request); err != nil {
		t.Fatal(err)
	}
	request.ReadOnlyDrives = request.ReadOnlyDrives[:1]
	if err := connector.validateMaterializeRequest(request); err == nil {
		t.Fatal("incomplete Program drive set was accepted")
	}
}

func TestMaterializeRequiresActivationProbedCPUShape(t *testing.T) {
	runtimeInstanceID := uuid.Must(uuid.NewV7()).String()
	rootfsDigest := testCanonicalDigest("0")
	artifacts := testProbeRuntimeArtifacts()
	artifacts.Rootfs.Digest = rootfsDigest
	validRequest := vm.MaterializeRequest{
		ID:        runtimeInstanceID,
		OwnerKind: vm.OwnerRuntime,
		Binding: vm.WorkloadBinding{
			WorkerEpoch: 1, OwnerID: runtimeInstanceID, Generation: 1,
			RuntimeInstanceID: runtimeInstanceID,
		},
		RootfsDigest:       rootfsDigest,
		WorkspaceMountPath: "/workspace",
		Resources:          compute.ResourceVector{MilliCPU: 1500},
		VMVCPUCount:        2,
		CPUConfigDigest:    testCPUConfigDigest(2),
	}
	connector := &Connector{
		cfg:         Config{VCPUCount: 2},
		artifacts:   artifacts,
		hostRuntime: newHostRuntimeEvidenceStore(),
	}
	evidence := testHostRuntimeEvidence(t, 2, artifacts)
	validRequest.Binding.RuntimeIdentityID = evidence.RuntimeID
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
		{name: "wrong runtime identity", edit: func(request *vm.MaterializeRequest) { request.Binding.RuntimeIdentityID = testCanonicalDigest("f") }, want: "does not match target host runtime"},
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

func TestSessionRuntimeFailsClosedUntilHostEvidenceIsBound(t *testing.T) {
	artifacts := testProbeRuntimeArtifacts()
	connector := &Connector{
		cfg:         Config{VCPUCount: 2},
		artifacts:   artifacts,
		hostRuntime: newHostRuntimeEvidenceStore(),
	}
	if _, _, _, err := connector.boundSessionRuntime(2); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("unbound session runtime error = %v", err)
	}
	evidence := testHostRuntimeEvidence(t, 2, artifacts)
	if err := connector.hostRuntime.bind(evidence, 2); err != nil {
		t.Fatal(err)
	}
	identity, digest, firecrackerPath, err := connector.boundSessionRuntime(2)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ID != evidence.RuntimeID || digest != evidence.CPUShapes[1].CPUConfigDigest || firecrackerPath != evidence.firecrackerPath {
		t.Fatalf("bound session identity = %+v digest=%q executable=%q evidence=%+v", identity, digest, firecrackerPath, evidence)
	}
}

func TestSessionEntryPointsRejectWorkloadRuntimeIdentityMismatch(t *testing.T) {
	connector := testConnector(t, testRestoreConfig(t))
	runtimeInstanceID := uuid.Must(uuid.NewV7()).String()
	binding := vm.WorkloadBinding{
		WorkerEpoch:       1,
		OwnerID:           runtimeInstanceID,
		Generation:        1,
		RuntimeInstanceID: runtimeInstanceID,
		RuntimeIdentityID: testCanonicalDigest("f"),
	}
	if _, err := connector.prepareSession(
		context.Background(),
		runtimeInstanceID,
		vm.OwnerRuntime,
		binding,
		"",
		"",
		"",
		nil,
		vm.RuntimeTopology{},
		nil,
		nil,
		false,
	); err == nil || !strings.Contains(err.Error(), "does not match bound host runtime") {
		t.Fatalf("prepare session runtime identity error = %v", err)
	}
	if _, err := connector.Restore(context.Background(), vm.RestoreRequest{
		RuntimeInstanceID: runtimeInstanceID,
		OwnerKind:         vm.OwnerRuntime,
		Binding:           binding,
	}); err == nil || !strings.Contains(err.Error(), "does not match target host runtime") {
		t.Fatalf("restore runtime identity error = %v", err)
	}
}

func TestBuildGuestProfilesUseExactDriveSets(t *testing.T) {
	source := &recordingReadOnlyDriveSource{}
	tests := map[string]struct {
		request    vm.ConnectRequest
		kernelArgs string
	}{
		"build": {
			request: vm.ConnectRequest{
				OwnerKind: vm.OwnerBuild,
				Resources: compute.BuildGuestResources(),
				PIDsMax:   compute.BuildGuestPIDsMax,
				ReadOnlyDrives: []vm.ReadOnlyDrive{
					{ID: vm.ToolchainDrive, Source: source},
					{ID: vm.ManagerDrive, Source: source},
					{ID: vm.ManagedRuntimeDrive, Source: source},
				},
			},
			kernelArgs: buildKernelArgs,
		},
		"image build": {
			request: vm.ConnectRequest{
				OwnerKind: vm.OwnerImageBuild,
				Resources: compute.ImageBuildGuestResources(),
				PIDsMax:   compute.ImageBuildGuestPIDsMax,
			},
			kernelArgs: imageBuildKernelArgs,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			kernelArgs, err := buildGuestProfile(test.request)
			if err != nil {
				t.Fatal(err)
			}
			if kernelArgs != test.kernelArgs {
				t.Fatalf("profile = %q, want %q", kernelArgs, test.kernelArgs)
			}
		})
	}
}

func TestBuildGuestProfileRejectsOpenProfiles(t *testing.T) {
	source := &recordingReadOnlyDriveSource{}
	required := []vm.ReadOnlyDrive{
		{ID: vm.ManagerDrive, Source: source},
		{ID: vm.ManagedRuntimeDrive, Source: source},
		{ID: vm.ToolchainDrive, Source: source},
	}
	tests := map[string]vm.ConnectRequest{
		"missing component": {
			OwnerKind:      vm.OwnerBuild,
			Resources:      compute.BuildGuestResources(),
			PIDsMax:        compute.BuildGuestPIDsMax,
			ReadOnlyDrives: required[:2],
		},
		"wrong process limit": {
			OwnerKind:      vm.OwnerBuild,
			Resources:      compute.BuildGuestResources(),
			PIDsMax:        compute.BuildGuestPIDsMax - 1,
			ReadOnlyDrives: required,
		},
		"workspace substrate": {
			OwnerKind: vm.OwnerBuild,
			Resources: compute.BuildGuestResources(),
			PIDsMax:   compute.BuildGuestPIDsMax,
			Topology: vm.RuntimeTopology{Substrate: &vm.RuntimeSubstrate{
				Path: "workspace.ext4",
			}},
			ReadOnlyDrives: required,
		},
		"build drives on standard build": {
			OwnerKind:      vm.OwnerBuild,
			Resources:      compute.BuildGuestResources(),
			ReadOnlyDrives: required,
		},
		"image build with drive": {
			OwnerKind:      vm.OwnerImageBuild,
			Resources:      compute.ImageBuildGuestResources(),
			PIDsMax:        compute.ImageBuildGuestPIDsMax,
			ReadOnlyDrives: required[:1],
		},
	}
	for name, request := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := buildGuestProfile(request); err == nil {
				t.Fatal("buildGuestProfile error = nil")
			}
		})
	}
}

func TestRuntimeDrivesUseFixedBuildOrder(t *testing.T) {
	source := &recordingReadOnlyDriveSource{}
	drives := runtimeDrives(
		"/rootfs.squashfs",
		"/scratch.ext4",
		"",
		[]vm.ReadOnlyDrive{
			{ID: vm.BuildTreeDrive, Source: source},
			{ID: vm.ToolchainDrive, Source: source},
			{ID: vm.ManagedRuntimeDrive, Source: source},
			{ID: vm.ManagerDrive, Source: source},
		},
	)
	want := []string{
		"rootfs",
		"scratch",
		vm.ManagerDrive,
		vm.ManagedRuntimeDrive,
		vm.ToolchainDrive,
		vm.BuildTreeDrive,
	}
	if len(drives) != len(want) {
		t.Fatalf("drive count = %d, want %d", len(drives), len(want))
	}
	for index, drive := range drives {
		if got := firecracker.StringValue(drive.DriveID); got != want[index] {
			t.Fatalf("drive %d ID = %q, want %q", index, got, want[index])
		}
	}
}

func TestSealedDriveChrootStrategySeparatesSourceCapabilities(t *testing.T) {
	chrootBase := t.TempDir()
	vmID := "vm-1"
	root := filepath.Join(chrootBase, "firecracker", vmID, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceDirectory := t.TempDir()
	kernelPath := filepath.Join(sourceDirectory, "vmlinux")
	rootfsPath := filepath.Join(sourceDirectory, "rootfs.squashfs")
	scratchPath := filepath.Join(sourceDirectory, "scratch.ext4")
	for _, path := range []string{kernelPath, rootfsPath, scratchPath} {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	source := &recordingReadOnlyDriveSource{}
	machine := &firecracker.Machine{
		Cfg: firecracker.Config{
			KernelImagePath: kernelPath,
			JailerCfg: &firecracker.JailerConfig{
				ExecFile:      "/usr/bin/firecracker",
				ChrootBaseDir: chrootBase,
				ID:            vmID,
				UID:           firecracker.Int(os.Getuid()),
				GID:           firecracker.Int(os.Getgid()),
			},
			Drives: runtimeDrives(
				rootfsPath,
				scratchPath,
				"",
				[]vm.ReadOnlyDrive{{
					ID:     vm.ProgramDrive,
					Source: source,
				}},
			),
		},
		Handlers: firecracker.Handlers{
			FcInit: firecracker.HandlerList{}.Append(firecracker.Handler{
				Name: firecracker.CreateLogFilesHandlerName,
				Fn: func(context.Context, *firecracker.Machine) error {
					return nil
				},
			}),
		},
	}
	firecracker.WithLogger(logrus.NewEntry(logrus.New()))(machine)
	strategy := sealedDriveChrootStrategy{
		kernelImagePath: kernelPath,
		drives: []vm.ReadOnlyDrive{{
			ID:     vm.ProgramDrive,
			Source: source,
		}},
	}
	if err := strategy.AdaptHandlers(&machine.Handlers); err != nil {
		t.Fatal(err)
	}
	if err := machine.Handlers.FcInit.Run(context.Background(), machine); err != nil {
		t.Fatal(err)
	}
	if source.directory != root ||
		source.name != "program.squashfs" ||
		source.uid != os.Getuid() ||
		source.gid != os.Getgid() {
		t.Fatalf("link request = %+v", source)
	}
	if len(machine.Cfg.Drives) != 3 {
		t.Fatalf("drive count = %d, want 3", len(machine.Cfg.Drives))
	}
	if got := firecracker.StringValue(machine.Cfg.Drives[2].PathOnHost); got != "program.squashfs" {
		t.Fatalf("program drive path = %q", got)
	}
	for _, name := range []string{
		filepath.Base(kernelPath),
		filepath.Base(rootfsPath),
		filepath.Base(scratchPath),
	} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("ordinary jail link %q: %v", name, err)
		}
	}
}

func TestSealedDriveChrootStrategyPreservesBuildDriveOrder(t *testing.T) {
	chrootBase := t.TempDir()
	vmID := "vm-build"
	root := filepath.Join(chrootBase, "firecracker", vmID, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceDirectory := t.TempDir()
	kernelPath := filepath.Join(sourceDirectory, "vmlinux")
	rootfsPath := filepath.Join(sourceDirectory, "rootfs.squashfs")
	scratchPath := filepath.Join(sourceDirectory, "scratch.ext4")
	for _, path := range []string{kernelPath, rootfsPath, scratchPath} {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sources := map[string]*recordingReadOnlyDriveSource{
		vm.ManagerDrive:        {},
		vm.ManagedRuntimeDrive: {},
		vm.ToolchainDrive:      {},
		vm.BuildTreeDrive:      {},
	}
	declared := []vm.ReadOnlyDrive{
		{ID: vm.BuildTreeDrive, Source: sources[vm.BuildTreeDrive]},
		{ID: vm.ToolchainDrive, Source: sources[vm.ToolchainDrive]},
		{ID: vm.ManagedRuntimeDrive, Source: sources[vm.ManagedRuntimeDrive]},
		{ID: vm.ManagerDrive, Source: sources[vm.ManagerDrive]},
	}
	machine := &firecracker.Machine{
		Cfg: firecracker.Config{
			KernelImagePath: kernelPath,
			JailerCfg: &firecracker.JailerConfig{
				ExecFile:      "/usr/bin/firecracker",
				ChrootBaseDir: chrootBase,
				ID:            vmID,
				UID:           firecracker.Int(os.Getuid()),
				GID:           firecracker.Int(os.Getgid()),
			},
			Drives: runtimeDrives(rootfsPath, scratchPath, "", declared),
		},
		Handlers: firecracker.Handlers{
			FcInit: firecracker.HandlerList{}.Append(firecracker.Handler{
				Name: firecracker.CreateLogFilesHandlerName,
				Fn: func(context.Context, *firecracker.Machine) error {
					return nil
				},
			}),
		},
	}
	firecracker.WithLogger(logrus.NewEntry(logrus.New()))(machine)
	strategy := sealedDriveChrootStrategy{kernelImagePath: kernelPath, drives: declared}
	if err := strategy.AdaptHandlers(&machine.Handlers); err != nil {
		t.Fatal(err)
	}
	if err := machine.Handlers.FcInit.Run(context.Background(), machine); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"rootfs",
		"scratch",
		vm.ManagerDrive,
		vm.ManagedRuntimeDrive,
		vm.ToolchainDrive,
		vm.BuildTreeDrive,
	}
	if len(machine.Cfg.Drives) != len(want) {
		t.Fatalf("drive count = %d, want %d", len(machine.Cfg.Drives), len(want))
	}
	for index, drive := range machine.Cfg.Drives {
		id := firecracker.StringValue(drive.DriveID)
		if id != want[index] {
			t.Fatalf("drive %d ID = %q, want %q", index, id, want[index])
		}
		if source := sources[id]; source != nil {
			if source.directory != root ||
				source.name != readOnlyDriveName(id) ||
				source.uid != os.Getuid() ||
				source.gid != os.Getgid() {
				t.Fatalf("drive %q link request = %+v", id, source)
			}
		}
	}
}

func TestValidateReadOnlyDrivesRejectsInvalidDeclarations(t *testing.T) {
	source := &recordingReadOnlyDriveSource{}
	for _, test := range []struct {
		name   string
		drives []vm.ReadOnlyDrive
	}{
		{
			name:   "path ID",
			drives: []vm.ReadOnlyDrive{{ID: "../code", Source: source}},
		},
		{
			name:   "nil source",
			drives: []vm.ReadOnlyDrive{{ID: vm.ProgramDrive}},
		},
		{
			name: "duplicate ID",
			drives: []vm.ReadOnlyDrive{
				{ID: vm.ProgramDrive, Source: source},
				{ID: vm.ProgramDrive, Source: source},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateReadOnlyDrives(test.drives); err == nil {
				t.Fatal("expected validation error")
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

func TestWithSnapshotRestoreSkipsVsockReconfiguration(t *testing.T) {
	machine := &firecracker.Machine{}
	firecracker.WithLogger(logrus.NewEntry(logrus.New()))(machine)

	withSnapshotRestore("/checkpoint.mem", "/checkpoint.vmstate")(machine)

	if machine.Cfg.Snapshot.MemFilePath != "/checkpoint.mem" {
		t.Fatalf("memory path = %q", machine.Cfg.Snapshot.MemFilePath)
	}
	if machine.Cfg.Snapshot.SnapshotPath != "/checkpoint.vmstate" {
		t.Fatalf("state path = %q", machine.Cfg.Snapshot.SnapshotPath)
	}
	if machine.Cfg.Snapshot.EnableDiffSnapshots {
		t.Fatal("restore enabled differential snapshots")
	}
	if machine.Cfg.Snapshot.ResumeVM {
		t.Fatal("restore must load paused so network identity can be validated before resume")
	}
	if !machine.Handlers.FcInit.Has(firecracker.LoadSnapshotHandlerName) {
		t.Fatal("expected snapshot load handler")
	}
	if machine.Handlers.FcInit.Has(firecracker.AddVsocksHandlerName) {
		t.Fatal("restore must not re-add vsock devices after loading a snapshot")
	}
}

func TestExplicitFullSnapshotSetsSnapshotType(t *testing.T) {
	parameters := &operations.CreateSnapshotParams{Body: &models.SnapshotCreateParams{}}
	explicitFullSnapshot(parameters)
	if parameters.Body.SnapshotType != models.SnapshotCreateParamsSnapshotTypeFull {
		t.Fatalf("snapshot type = %q", parameters.Body.SnapshotType)
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
		Resources: compute.ResourceVector{
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
	for name, resources := range map[string]compute.ResourceVector{
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
		VMRuntimeContract: runtimeid.Contract,
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
	connector := &Connector{cfg: cfg, artifacts: artifacts, hostRuntime: newHostRuntimeEvidenceStore()}
	if err := connector.hostRuntime.bind(testHostRuntimeEvidence(t, cfg.VCPUCount, artifacts), cfg.VCPUCount); err != nil {
		t.Fatal(err)
	}
	return connector
}

func testCheckpointArchitecture(t *testing.T) string {
	t.Helper()
	architecture, err := runtimeid.ArchitectureFromGo(runtime.GOARCH)
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
	runtimeIdentity := testRuntimeIdentity(t, kernelDigest, initramfsDigest, rootfsDigest)
	runtimeID := runtimeIdentity.ID
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
				Arch:             runtimeIdentity.Arch,
				Contract:         runtimeIdentity.Contract,
				VCPUCount:        cfg.VCPUCount,
				CPUConfigDigest:  testCPUConfigDigest(cfg.VCPUCount),
				MemoryMiB:        cfg.MemoryMiB,
				ScratchDiskMiB:   cfg.ScratchDiskMiB,
				KernelArgs:       defaultKernelArgs,
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
		RuntimeBackend:      "firecracker",
		RuntimeID:           runtimeID,
		RuntimeArch:         runtimeIdentity.Arch,
		VMRuntimeContract:   runtimeIdentity.Contract,
		KernelDigest:        kernelDigest,
		InitramfsDigest:     initramfsDigest,
		RootfsDigest:        rootfsDigest,
		RuntimeConfigDigest: sha256sum.DigestBytes(manifestBytes),
		VMVCPUCount:         int32(cfg.VCPUCount),
		CPUConfigDigest:     testCPUConfigDigest(cfg.VCPUCount),
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

func hasRuntimePhase(phases []vm.RuntimePhase, name string, errorClass string) bool {
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

func testDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}
