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
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	fc "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	fcvsock "github.com/firecracker-microvm/firecracker-go-sdk/vsock"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

func TestSnapshotRuntimeConfigIncludesCNIIdentity(t *testing.T) {
	cfg := (Config{}).WithDefaults()
	machine := &fc.Machine{
		Cfg: fc.Config{
			NetworkInterfaces: fc.NetworkInterfaces{{
				StaticConfiguration: &fc.StaticNetworkConfiguration{
					IPConfiguration: &fc.IPConfiguration{
						IPAddr: net.IPNet{
							IP:   net.IPv4(192, 168, 127, 2),
							Mask: net.CIDRMask(24, 32),
						},
					},
				},
			}},
		},
	}

	runtimeID, err := runtimeIdentityDigest(runtime.GOARCH, runtimeABI, "sha256:kernel", "sha256:initramfs", "sha256:rootfs", cfg.CNIProfile)
	if err != nil {
		t.Fatal(err)
	}
	digest, manifestBytes, err := snapshotRuntimeConfig(cfg, machine, "checkpoint-1", runtimeID, "sha256:kernel", "sha256:initramfs", "sha256:rootfs")
	if err != nil {
		t.Fatal(err)
	}
	if digest != cas.DigestBytes(manifestBytes) {
		t.Fatalf("digest = %q, want %q", digest, cas.DigestBytes(manifestBytes))
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

type testWrappedErrors []error

func (e testWrappedErrors) Error() string {
	return "wrapped errors"
}

func (e testWrappedErrors) WrappedErrors() []error {
	return []error(e)
}

func TestSnapshotRuntimeConfigRequiresCNIIP(t *testing.T) {
	cfg := (Config{}).WithDefaults()
	runtimeID, err := runtimeIdentityDigest(runtime.GOARCH, runtimeABI, "sha256:kernel", "sha256:initramfs", "sha256:rootfs", cfg.CNIProfile)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = snapshotRuntimeConfig(cfg, &fc.Machine{}, "checkpoint-1", runtimeID, "sha256:kernel", "sha256:initramfs", "sha256:rootfs")
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
	runtimeID, err := runtimeIdentityDigest(runtime.GOARCH, runtimeABI, kernelDigest, initramfsDigest, rootfsDigest, cfg.CNIProfile)
	if err != nil {
		t.Fatal(err)
	}
	connector := &Connector{cfg: cfg}

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
		{name: "manifest vcpu", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.VCPUCount++ }, want: "checkpoint manifest machine shape"},
		{name: "manifest memory", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.MemoryMiB++ }, want: "checkpoint manifest machine shape"},
		{name: "manifest scratch disk", editManifest: func(m *snapshotManifest) { m.RecoveryPoint.Runtime.ScratchDiskMiB++ }, want: "checkpoint manifest scratch disk size"},
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
				RuntimeConfigDigest: cas.DigestBytes(manifestBytes),
			}
			if tt.editIdentity != nil {
				tt.editIdentity(&identity)
			}

			_, err := connector.validateRestoreIdentity(checkpointID, manifestBytes, identity)
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
	dialVsock = func(context.Context, string, uint32, ...fcvsock.DialOption) (net.Conn, error) {
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
	if err := connector.waitForHealth(context.Background(), "vsock.sock"); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("dial attempts = %d, want 2", attempts)
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
	memoryPath := filepath.Join(sourceDir, "checkpoint.mem")
	statePath := filepath.Join(sourceDir, "checkpoint.vmstate")
	for _, path := range []string{rootfsPath, scratchDiskPath, memoryPath, statePath} {
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	machine := &fc.Machine{
		Cfg: fc.Config{
			JailerCfg: &fc.JailerConfig{
				ExecFile:      "/usr/bin/firecracker",
				ChrootBaseDir: chrootBase,
				ID:            vmID,
				UID:           fc.Int(os.Getuid()),
				GID:           fc.Int(os.Getgid()),
			},
			Drives: []models.Drive{{
				DriveID:    fc.String("rootfs"),
				PathOnHost: fc.String(rootfsPath),
			}, {
				DriveID:    fc.String("scratch"),
				PathOnHost: fc.String(scratchDiskPath),
			}},
			Snapshot: fc.SnapshotConfig{},
		},
		Handlers: fc.Handlers{
			FcInit: fc.HandlerList{}.Append(fc.Handler{
				Name: fc.CreateLogFilesHandlerName,
				Fn: func(context.Context, *fc.Machine) error {
					return nil
				},
			}),
		},
	}
	fc.WithLogger(logrus.NewEntry(logrus.New()))(machine)
	opt := withJailedRestoreFiles(rootfsPath, scratchDiskPath, memoryPath, statePath)
	opt(machine)
	if err := machine.Handlers.FcInit.Run(context.Background(), machine); err != nil {
		t.Fatal(err)
	}

	if got := fc.StringValue(machine.Cfg.Drives[0].PathOnHost); got != filepath.Base(rootfsPath) {
		t.Fatalf("rootfs drive path = %q", got)
	}
	if got := fc.StringValue(machine.Cfg.Drives[1].PathOnHost); got != scratchDiskName {
		t.Fatalf("scratch drive path = %q", got)
	}
	for _, name := range []string{filepath.Base(rootfsPath), scratchDiskName, filepath.Base(memoryPath), filepath.Base(statePath)} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatalf("expected %s linked into jail: %v", name, err)
		}
	}
}

func TestWithSnapshotRestoreSkipsVsockReconfiguration(t *testing.T) {
	machine := &fc.Machine{}
	fc.WithLogger(logrus.NewEntry(logrus.New()))(machine)

	withSnapshotRestore("/checkpoint.mem", "/checkpoint.vmstate")(machine)

	if machine.Cfg.Snapshot.MemFilePath != "/checkpoint.mem" {
		t.Fatalf("memory path = %q", machine.Cfg.Snapshot.MemFilePath)
	}
	if machine.Cfg.Snapshot.SnapshotPath != "/checkpoint.vmstate" {
		t.Fatalf("state path = %q", machine.Cfg.Snapshot.SnapshotPath)
	}
	if !machine.Handlers.FcInit.Has(fc.LoadSnapshotHandlerName) {
		t.Fatal("expected snapshot load handler")
	}
	if machine.Handlers.FcInit.Has(fc.AddVsocksHandlerName) {
		t.Fatal("restore must not re-add vsock devices after loading a snapshot")
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
	if err := session.Close(); err != nil {
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
	rootfsPath := filepath.Join(dir, "rootfs")
	if err := os.WriteFile(kernelPath, []byte("kernel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rootfsPath, []byte("rootfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := (Config{
		KernelPath: kernelPath,
		RootfsPath: rootfsPath,
		VCPUCount:  2,
		MemoryMiB:  256,
	}).WithDefaults()
	return cfg
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

func (s *checkpointableTestSession) Close() error {
	s.closed = true
	return nil
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
