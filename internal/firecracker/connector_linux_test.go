//go:build linux

package firecracker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
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
	"github.com/firecracker-microvm/firecracker-go-sdk/vsock"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

func TestSnapshotRuntimeConfigIncludesCNIIdentity(t *testing.T) {
	cfg := (Config{}).WithDefaults()
	machine := &firecracker.Machine{
		Cfg: firecracker.Config{
			NetworkInterfaces: firecracker.NetworkInterfaces{{
				StaticConfiguration: &firecracker.StaticNetworkConfiguration{
					IPConfiguration: &firecracker.IPConfiguration{
						IPAddr: net.IPNet{
							IP:   net.IPv4(192, 168, 127, 2),
							Mask: net.CIDRMask(24, 32),
						},
					},
				},
			}},
		},
	}

	runtimeID, err := compute.RuntimeIdentityDigest(compute.RuntimeSelector{Arch: runtime.GOARCH, ABI: runtimeABI, KernelDigest: "sha256:kernel", InitramfsDigest: "sha256:initramfs", RootfsDigest: "sha256:rootfs", CNIProfile: cfg.CNIProfile})
	if err != nil {
		t.Fatal(err)
	}
	digest, manifestBytes, err := snapshotRuntimeConfig(cfg, machine, "checkpoint-1", runtimeID, "sha256:kernel", "sha256:initramfs", "sha256:rootfs", vm.RuntimeTopology{})
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
	if network.Mode != "cni" || network.Profile != cfg.CNIProfile || network.NetworkName != cfg.CNINetworkName || network.IfName != cfg.CNIIfName || network.VMIfName != cfg.CNIVMIfName || network.GuestIPCIDR != "192.168.127.2/24" {
		t.Fatalf("network = %+v", network)
	}
	if manifest.RecoveryPoint.Runtime.ID != runtimeID || manifest.RecoveryPoint.Runtime.InitramfsDigest != "sha256:initramfs" {
		t.Fatalf("runtime = %+v", manifest.RecoveryPoint.Runtime)
	}
}

func TestCleanupRuntimeRequiresCanonicalExactOwnership(t *testing.T) {
	stateDir := t.TempDir()
	jailerDir := t.TempDir()
	id := "00000000-0000-0000-0000-000000000701"
	statePath := filepath.Join(stateDir, id)
	if err := os.MkdirAll(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statePath, "owner"), []byte(vm.RuntimeOwnerBuild+"\n"+id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	connector := &Connector{cfg: Config{StateDir: stateDir, JailerChrootBaseDir: jailerDir, IPPath: "/bin/true"}}
	if err := connector.CleanupRuntime(context.Background(), id); err == nil || !strings.Contains(err.Error(), "ownership evidence") {
		t.Fatalf("CleanupRuntime() error = %v, want exact ownership rejection", err)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("mismatched owner state was removed: %v", err)
	}
	if err := connector.CleanupRuntime(context.Background(), strings.ToUpper(id)); err == nil {
		t.Fatal("non-canonical owner id was accepted")
	}
}

func TestGuestSessionExposesActualCNINetworkFacts(t *testing.T) {
	cfg := (Config{}).WithDefaults()
	session := &guestSession{cfg: cfg, machine: &firecracker.Machine{Cfg: firecracker.Config{
		NetNS: "/var/run/netns/0190f9c2-aaaa-7bbb-8ccc-0123456789ab",
		NetworkInterfaces: firecracker.NetworkInterfaces{{StaticConfiguration: &firecracker.StaticNetworkConfiguration{
			HostDevName: "tap7f3a", MacAddress: "06:00:ac:10:00:02",
			IPConfiguration: &firecracker.IPConfiguration{
				IPAddr:  net.IPNet{IP: net.IPv4(172, 16, 0, 2), Mask: net.CIDRMask(24, 32)},
				Gateway: net.IPv4(172, 16, 0, 1),
			},
		}}},
	}}}
	facts, err := session.NetworkFacts()
	if err != nil {
		t.Fatal(err)
	}
	if facts.HostInterfaceName != "tap7f3a" || facts.TapName != "tap7f3a" ||
		facts.NetNSName != "0190f9c2-aaaa-7bbb-8ccc-0123456789ab" ||
		facts.GuestAddress != "172.16.0.2" || facts.GatewayAddress != "172.16.0.1" ||
		facts.Subnet != "172.16.0.0/24" || facts.GuestMAC != "06:00:ac:10:00:02" {
		t.Fatalf("network facts = %+v", facts)
	}
}

func TestSnapshotRuntimeConfigIncludesSubstrateIdentity(t *testing.T) {
	cfg := (Config{}).WithDefaults()
	machine := &firecracker.Machine{
		Cfg: firecracker.Config{
			NetworkInterfaces: firecracker.NetworkInterfaces{{
				StaticConfiguration: &firecracker.StaticNetworkConfiguration{
					IPConfiguration: &firecracker.IPConfiguration{
						IPAddr: net.IPNet{
							IP:   net.IPv4(192, 168, 127, 2),
							Mask: net.CIDRMask(24, 32),
						},
					},
				},
			}},
		},
	}
	topology := vm.RuntimeTopology{Substrate: &vm.RuntimeSubstrate{
		Path:       filepath.Join(t.TempDir(), "substrate.ext4"),
		Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Format:     "ext4",
		BuilderABI: "builder-v1",
		LayoutABI:  "layout-v1",
	}}
	runtimeID, err := compute.RuntimeIdentityDigest(compute.RuntimeSelector{Arch: runtime.GOARCH, ABI: runtimeABI, KernelDigest: "sha256:kernel", InitramfsDigest: "sha256:initramfs", RootfsDigest: "sha256:rootfs", CNIProfile: cfg.CNIProfile})
	if err != nil {
		t.Fatal(err)
	}
	_, manifestBytes, err := snapshotRuntimeConfig(cfg, machine, "checkpoint-1", runtimeID, "sha256:kernel", "sha256:initramfs", "sha256:rootfs", topology)
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
	if substrate.Digest != topology.Substrate.Digest || substrate.Format != "ext4" || substrate.BuilderABI != "builder-v1" || substrate.LayoutABI != "layout-v1" {
		t.Fatalf("substrate = %+v", substrate)
	}
}

func TestValidateRuntimeSubstrateManifestRequiresExactTopologyMatch(t *testing.T) {
	manifest := &snapshotRuntimeSubstrate{
		Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Format:     "ext4",
		BuilderABI: "builder-v1",
		LayoutABI:  "layout-v1",
	}
	expected := &vm.RuntimeSubstrate{
		Path:       filepath.Join(t.TempDir(), "substrate.ext4"),
		Digest:     manifest.Digest,
		Format:     manifest.Format,
		BuilderABI: manifest.BuilderABI,
		LayoutABI:  manifest.LayoutABI,
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

func TestSnapshotRuntimeConfigRequiresCNIIP(t *testing.T) {
	cfg := (Config{}).WithDefaults()
	runtimeID, err := compute.RuntimeIdentityDigest(compute.RuntimeSelector{Arch: runtime.GOARCH, ABI: runtimeABI, KernelDigest: "sha256:kernel", InitramfsDigest: "sha256:initramfs", RootfsDigest: "sha256:rootfs", CNIProfile: cfg.CNIProfile})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = snapshotRuntimeConfig(cfg, &firecracker.Machine{}, "checkpoint-1", runtimeID, "sha256:kernel", "sha256:initramfs", "sha256:rootfs", vm.RuntimeTopology{})
	if err == nil {
		t.Fatal("expected missing guest IP error")
	}
}

func TestRestoreNetworkInterfaceRequestsCheckpointIP(t *testing.T) {
	connector := &Connector{cfg: (Config{}).WithDefaults()}
	iface := connector.networkInterface(&snapshotNetworkManifest{GuestIPCIDR: "192.168.127.2/24"})
	if iface.CNIConfiguration == nil || len(iface.CNIConfiguration.Args) != 1 {
		t.Fatalf("interface = %+v", iface)
	}
	if got := iface.CNIConfiguration.Args[0]; got != [2]string{"IP", "192.168.127.2/24"} {
		t.Fatalf("args = %+v", iface.CNIConfiguration.Args)
	}
}

func TestDefaultKernelArgsDeclareExt4Root(t *testing.T) {
	if !strings.Contains(defaultKernelArgs, "rootfstype=ext4") {
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
	runtimeID, err := compute.RuntimeIdentityDigest(compute.RuntimeSelector{Arch: runtime.GOARCH, ABI: runtimeABI, KernelDigest: kernelDigest, InitramfsDigest: initramfsDigest, RootfsDigest: rootfsDigest, CNIProfile: cfg.CNIProfile})
	if err != nil {
		t.Fatal(err)
	}
	connector := testConnector(t, cfg)

	validManifest := snapshotManifest{
		RecoveryPoint: snapshotRecoveryPointManifest{
			ID: "checkpoint-1",
			Runtime: snapshotRuntimeManifest{
				Backend:         "firecracker",
				ID:              runtimeID,
				Arch:            runtime.GOARCH,
				ABI:             runtimeABI,
				VCPUCount:       cfg.VCPUCount,
				MemoryMiB:       cfg.MemoryMiB,
				ScratchDiskMiB:  cfg.ScratchDiskMiB,
				KernelArgs:      defaultKernelArgs,
				KernelDigest:    kernelDigest,
				InitramfsDigest: initramfsDigest,
				RootfsDigest:    rootfsDigest,
				GuestPort:       cfg.GuestPort,
				HealthPort:      cfg.HealthPort,
				Network: snapshotNetworkIdentityManifest{
					Mode:        "cni",
					Profile:     cfg.CNIProfile,
					NetworkName: cfg.CNINetworkName,
					IfName:      cfg.CNIIfName,
					VMIfName:    cfg.CNIVMIfName,
				},
			},
		},
		RuntimeState: snapshotRuntimeStateManifest{
			Network: snapshotNetworkManifest{
				Mode:        "cni",
				Profile:     cfg.CNIProfile,
				NetworkName: cfg.CNINetworkName,
				IfName:      cfg.CNIIfName,
				VMIfName:    cfg.CNIVMIfName,
				GuestIPCIDR: "192.168.127.2/24",
			},
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
		{name: "identity abi", editIdentity: func(i *vm.CheckpointIdentity) { i.RuntimeABI = "other" }, want: `checkpoint runtime abi "other" does not match`},
		{name: "identity runtime id", editIdentity: func(i *vm.CheckpointIdentity) { i.RuntimeID = "sha256:other" }, want: "checkpoint runtime id sha256:other does not match"},
		{name: "identity kernel digest", editIdentity: func(i *vm.CheckpointIdentity) { i.KernelDigest = "sha256:other" }, want: "checkpoint kernel digest sha256:other does not match"},
		{name: "identity initramfs digest", editIdentity: func(i *vm.CheckpointIdentity) { i.InitramfsDigest = "sha256:other" }, want: "checkpoint initramfs digest sha256:other does not match"},
		{name: "identity rootfs digest", editIdentity: func(i *vm.CheckpointIdentity) { i.RootfsDigest = "sha256:other" }, want: "checkpoint rootfs digest sha256:other does not match"},
		{name: "identity runtime config digest", editIdentity: func(i *vm.CheckpointIdentity) { i.RuntimeConfigDigest = "sha256:other" }, want: "checkpoint runtime config digest sha256:other does not match"},
		{name: "manifest backend", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.Backend = "test" }, want: `checkpoint manifest runtime backend "test" is not supported`},
		{name: "manifest arch", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.Arch = "other" }, want: `checkpoint manifest runtime arch "other" does not match`},
		{name: "manifest abi", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.ABI = "other" }, want: `checkpoint manifest runtime abi "other" does not match`},
		{name: "manifest runtime id", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.ID = "sha256:other" }, want: "checkpoint manifest runtime id sha256:other does not match"},
		{name: "manifest kernel digest", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.KernelDigest = "sha256:other" }, want: "checkpoint manifest kernel digest sha256:other does not match"},
		{name: "manifest initramfs digest", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.InitramfsDigest = "sha256:other" }, want: "checkpoint manifest initramfs digest sha256:other does not match"},
		{name: "manifest rootfs digest", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.RootfsDigest = "sha256:other" }, want: "checkpoint manifest rootfs digest sha256:other does not match"},
		{name: "manifest vcpu exceeds worker capacity", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.VCPUCount = cfg.VCPUCount + 1 }, want: "checkpoint manifest vcpu count"},
		{name: "manifest memory exceeds worker capacity", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.MemoryMiB = cfg.MemoryMiB + 1 }, want: "checkpoint manifest memory"},
		{name: "manifest scratch disk exceeds worker capacity", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.ScratchDiskMiB = cfg.ScratchDiskMiB + 1 }, want: "checkpoint manifest scratch disk size"},
		{name: "manifest kernel args", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.KernelArgs = "other" }, want: "checkpoint manifest runtime ports or kernel args do not match"},
		{name: "manifest guest port", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.GuestPort++ }, want: "checkpoint manifest runtime ports or kernel args do not match"},
		{name: "manifest health port", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.HealthPort++ }, want: "checkpoint manifest runtime ports or kernel args do not match"},
		{name: "manifest network mode", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.Network.Mode = "tap" }, want: `checkpoint manifest network mode "tap" is not supported`},
		{name: "manifest CNI profile", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.Network.Profile = "other/v1" }, want: "checkpoint manifest CNI configuration does not match"},
		{name: "manifest CNI network", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.Network.NetworkName = "other" }, want: "checkpoint manifest CNI configuration does not match"},
		{name: "manifest CNI if", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.Network.IfName = "other0" }, want: "checkpoint manifest CNI configuration does not match"},
		{name: "manifest CNI vm if", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.Network.VMIfName = "other0" }, want: "checkpoint manifest CNI configuration does not match"},
		{name: "manifest guest ip", editManifest: func(m *snapshotManifest) { m.RuntimeState.Network.GuestIPCIDR = "" }, want: "checkpoint manifest guest_ip_cidr is required"},
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
				RuntimeArch:         runtime.GOARCH,
				RuntimeABI:          runtimeABI,
				KernelDigest:        kernelDigest,
				InitramfsDigest:     initramfsDigest,
				RootfsDigest:        rootfsDigest,
				RuntimeConfigDigest: sha256sum.DigestBytes(manifestBytes),
			}
			if tt.editIdentity != nil {
				tt.editIdentity(&identity)
			}

			_, _, err := connector.validateRestoreIdentity(checkpointID, manifestBytes, identity, vm.RuntimeTopology{})
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
	manifest.RecoveryPoint.Runtime.MemoryMiB = cfg.MemoryMiB / 2
	manifest.RecoveryPoint.Runtime.ScratchDiskMiB = cfg.ScratchDiskMiB / 2
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	identity.RuntimeConfigDigest = sha256sum.DigestBytes(manifestBytes)

	_, restoreCfg, err := connector.validateRestoreIdentity("checkpoint-1", manifestBytes, identity, vm.RuntimeTopology{})
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
	var mu sync.Mutex
	var phases []vm.RuntimePhase

	_, err := connector.Restore(context.Background(), vm.RestoreRequest{
		ID:                   "checkpoint-1",
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

	restored, phase, err := connector.unpackRestoreArtifact(context.Background(), pack, filepackScratchRole, "scratch.ext4", 1<<20, cas.CheckpointScratchDiskMediaType)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(restored)
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
	if !strings.Contains(err.Error(), "firecracker machine exited during guest health wait") {
		t.Fatalf("waitForHealth error = %v, want machine exit context", err)
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
		if err == nil || !strings.Contains(err.Error(), "firecracker machine exited before guest port") {
			t.Fatalf("connectGuestPort error = %v, want machine exit", err)
		}
	case <-time.After(time.Second):
		t.Fatal("connectGuestPort waited after machine exit")
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

func TestConnectReadyGuestWaitsForHealthBeforeGuestPort(t *testing.T) {
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
	conn, err := connector.connectReadyGuest(context.Background(), "vsock.sock", nil, nil)
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
	rootfsPath := filepath.Join(sourceDir, "rootfs.ext4")
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
	drives := runtimeDrives("/rootfs.ext4", "/scratch.ext4", "/substrate.ext4")
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
	if !machine.Handlers.FcInit.Has(firecracker.LoadSnapshotHandlerName) {
		t.Fatal("expected snapshot load handler")
	}
	if machine.Handlers.FcInit.Has(firecracker.AddVsocksHandlerName) {
		t.Fatal("restore must not re-add vsock devices after loading a snapshot")
	}
}

func TestConfigForMaterializeRequestUsesRequestedRuntimeResources(t *testing.T) {
	connector := &Connector{cfg: Config{VCPUCount: 4, MemoryMiB: 4096, ScratchDiskMiB: 32768}}
	cfg, err := connector.configForMaterializeRequest(vm.MaterializeRequest{
		Resources: compute.ResourceVector{
			MilliCPU:  1500,
			MemoryMiB: 1024,
			DiskMiB:   4096,
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
	if _, err := connector.configForMaterializeRequest(vm.MaterializeRequest{Resources: compute.ResourceVector{MemoryMiB: 4096}}); err == nil {
		t.Fatal("expected oversized memory request to fail")
	}
	if _, err := connector.configForMaterializeRequest(vm.MaterializeRequest{Resources: compute.ResourceVector{MilliCPU: 3000}}); err == nil {
		t.Fatal("expected oversized cpu request to fail")
	}
	if _, err := connector.configForMaterializeRequest(vm.MaterializeRequest{Resources: compute.ResourceVector{DiskMiB: 16384}}); err == nil {
		t.Fatal("expected oversized disk request to fail")
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

func TestRestoreCleanupSessionPreservesCheckpointSupport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "restore.raw")
	if err := os.WriteFile(path, []byte("restore"), 0o600); err != nil {
		t.Fatal(err)
	}
	inner := &checkpointableTestSession{}
	session := restoreCleanupSession{CheckpointableSession: inner, paths: []string{path}}
	if _, ok := any(session).(vm.CheckpointableSession); !ok {
		t.Fatal("restore cleanup session must remain checkpointable")
	}
	if _, err := session.CreateSnapshot(context.Background(), vm.SnapshotRequest{ID: "checkpoint-1"}); err != nil {
		t.Fatal(err)
	}
	if !inner.snapshotCalled {
		t.Fatal("snapshot call did not reach inner session")
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !inner.closed {
		t.Fatal("close call did not reach inner session")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("restore file still exists: %v", err)
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
		KernelPath:    kernelPath,
		InitramfsPath: initramfsPath,
		RootfsPath:    rootfsPath,
		VCPUCount:     2,
		MemoryMiB:     256,
	}).WithDefaults()
	manifest := runtimeArtifacts{
		Schema:     runtimeArtifactsSchema,
		Arch:       runtime.GOARCH,
		RuntimeABI: runtimeABI,
		Kernel:     runtimeArtifact{Path: filepath.Base(kernelPath), Digest: testDigest([]byte("kernel")), SizeBytes: int64(len("kernel"))},
		Initramfs:  runtimeArtifact{Path: filepath.Base(initramfsPath), Digest: testDigest([]byte("initramfs")), SizeBytes: int64(len("initramfs"))},
		Rootfs:     runtimeArtifact{Path: filepath.Base(rootfsPath), Digest: testDigest([]byte("rootfs")), SizeBytes: int64(len("rootfs"))},
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
	return &Connector{cfg: cfg, artifacts: artifacts}
}

func testRestoreManifestAndIdentity(t *testing.T, cfg Config, checkpointID string) ([]byte, vm.CheckpointIdentity) {
	t.Helper()
	kernelDigest := testDigest([]byte("kernel"))
	initramfsDigest := testDigest([]byte("initramfs"))
	rootfsDigest := testDigest([]byte("rootfs"))
	runtimeID, err := compute.RuntimeIdentityDigest(compute.RuntimeSelector{Arch: runtime.GOARCH, ABI: runtimeABI, KernelDigest: kernelDigest, InitramfsDigest: initramfsDigest, RootfsDigest: rootfsDigest, CNIProfile: cfg.CNIProfile})
	if err != nil {
		t.Fatal(err)
	}
	manifest := snapshotManifest{
		RecoveryPoint: snapshotRecoveryPointManifest{
			ID: checkpointID,
			Runtime: snapshotRuntimeManifest{
				Backend:         "firecracker",
				ID:              runtimeID,
				Arch:            runtime.GOARCH,
				ABI:             runtimeABI,
				VCPUCount:       cfg.VCPUCount,
				MemoryMiB:       cfg.MemoryMiB,
				ScratchDiskMiB:  cfg.ScratchDiskMiB,
				KernelArgs:      defaultKernelArgs,
				KernelDigest:    kernelDigest,
				InitramfsDigest: initramfsDigest,
				RootfsDigest:    rootfsDigest,
				GuestPort:       cfg.GuestPort,
				HealthPort:      cfg.HealthPort,
				Network: snapshotNetworkIdentityManifest{
					Mode:        "cni",
					Profile:     cfg.CNIProfile,
					NetworkName: cfg.CNINetworkName,
					IfName:      cfg.CNIIfName,
					VMIfName:    cfg.CNIVMIfName,
				},
			},
		},
		RuntimeState: snapshotRuntimeStateManifest{
			Network: snapshotNetworkManifest{
				Mode:        "cni",
				Profile:     cfg.CNIProfile,
				NetworkName: cfg.CNINetworkName,
				IfName:      cfg.CNIIfName,
				VMIfName:    cfg.CNIVMIfName,
				GuestIPCIDR: "192.168.127.2/24",
			},
		},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return manifestBytes, vm.CheckpointIdentity{
		RuntimeBackend:      "firecracker",
		RuntimeID:           runtimeID,
		RuntimeArch:         runtime.GOARCH,
		RuntimeABI:          runtimeABI,
		KernelDigest:        kernelDigest,
		InitramfsDigest:     initramfsDigest,
		RootfsDigest:        rootfsDigest,
		RuntimeConfigDigest: sha256sum.DigestBytes(manifestBytes),
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

type checkpointableTestSession struct {
	closed         bool
	snapshotCalled bool
}

func (s *checkpointableTestSession) Stream() io.ReadWriteCloser {
	return readWriteNopCloser{}
}

func (s *checkpointableTestSession) OpenStream(context.Context) (io.ReadWriteCloser, error) {
	return readWriteNopCloser{}, nil
}

func (s *checkpointableTestSession) Close(context.Context) error {
	s.closed = true
	return nil
}

func (s *checkpointableTestSession) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (s *checkpointableTestSession) CreateSnapshot(context.Context, vm.SnapshotRequest) (vm.SnapshotArtifact, error) {
	s.snapshotCalled = true
	return vm.SnapshotArtifact{}, nil
}

func (s *checkpointableTestSession) Resume(context.Context) error {
	return nil
}

type readWriteNopCloser struct{}

func (readWriteNopCloser) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (readWriteNopCloser) Write(p []byte) (int, error) {
	return len(p), nil
}

func (readWriteNopCloser) Close() error {
	return nil
}
