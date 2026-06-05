//go:build linux

package firecracker

import (
	"bufio"
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
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	fc "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	fcvsock "github.com/firecracker-microvm/firecracker-go-sdk/vsock"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/vm"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

const defaultKernelArgs = "console=ttyS0 reboot=k panic=1 root=/dev/vda rootfstype=ext4 ro init=/init"
const stopTimeout = 10 * time.Second
const runtimeABI = "helmr.firecracker.snapshot.v0"
const apiSocketName = "api.sock"
const vsockSocketName = "vsock.sock"
const scratchDiskName = "scratch.ext4"

var nextGuestCID atomic.Uint32
var dialVsock = fcvsock.DialContext

type Connector struct {
	cfg Config
}

func NewConnector(cfg Config) (*Connector, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Connector{cfg: cfg}, nil
}

func (c *Connector) RuntimeCapabilities() (RuntimeCapabilities, error) {
	kernelDigest, err := digestFile(c.cfg.KernelPath)
	if err != nil {
		return RuntimeCapabilities{}, fmt.Errorf("digest guest kernel: %w", err)
	}
	initramfsDigest, err := digestFile(c.cfg.InitramfsPath)
	if err != nil {
		return RuntimeCapabilities{}, fmt.Errorf("digest guest initramfs: %w", err)
	}
	rootfsDigest, err := digestFile(c.cfg.RootfsPath)
	if err != nil {
		return RuntimeCapabilities{}, fmt.Errorf("digest guest rootfs: %w", err)
	}
	runtimeID, err := compute.RuntimeIdentityDigest(compute.RuntimeSelector{
		Arch:            runtime.GOARCH,
		ABI:             runtimeABI,
		KernelDigest:    kernelDigest,
		InitramfsDigest: initramfsDigest,
		RootfsDigest:    rootfsDigest,
		CNIProfile:      c.cfg.CNIProfile,
	})
	if err != nil {
		return RuntimeCapabilities{}, err
	}
	return RuntimeCapabilities{
		ID:              runtimeID,
		Arch:            runtime.GOARCH,
		ABI:             runtimeABI,
		KernelDigest:    kernelDigest,
		InitramfsDigest: initramfsDigest,
		RootfsDigest:    rootfsDigest,
		CNIProfile:      c.cfg.CNIProfile,
		VCPUCount:       c.cfg.VCPUCount,
		MemoryMiB:       c.cfg.MemoryMiB,
	}, nil
}

func commandAvailable(path string) bool {
	return checkCommand(filepath.Base(path), path) == nil
}

func (c *Connector) Connect(ctx context.Context) (vm.Session, error) {
	return c.start(ctx, "", "", "", nil)
}

func (c *Connector) Restore(ctx context.Context, request vm.RestoreRequest) (vm.Session, error) {
	if len(request.Memory) != 1 {
		return nil, fmt.Errorf("firecracker restore requires exactly one memory file, got %d", len(request.Memory))
	}
	if len(request.MemoryMediaTypes) != 1 {
		return nil, fmt.Errorf("firecracker restore requires exactly one memory media type, got %d", len(request.MemoryMediaTypes))
	}
	if strings.TrimSpace(request.VMState) == "" {
		return nil, errors.New("firecracker restore vm state path is required")
	}
	if request.VMStateMediaType != cas.CheckpointVMStateMediaType {
		return nil, fmt.Errorf("firecracker restore vm state media type %q is not supported", request.VMStateMediaType)
	}
	manifest, err := c.validateRestoreIdentity(request.ID, request.Manifest, request.Checkpoint)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.ScratchDisk) == "" {
		return nil, errors.New("firecracker restore scratch disk path is required")
	}
	if request.ScratchDiskMediaType != cas.CheckpointScratchDiskMediaType {
		return nil, fmt.Errorf("firecracker restore scratch disk media type %q is not supported", request.ScratchDiskMediaType)
	}
	if request.MemoryMediaTypes[0] != cas.CheckpointMemoryMediaType {
		return nil, fmt.Errorf("firecracker restore memory media type %q is not supported", request.MemoryMediaTypes[0])
	}
	expectedScratchSize := manifest.RecoveryPoint.Runtime.ScratchDiskMiB * 1024 * 1024
	expectedMemorySize := manifest.RecoveryPoint.Runtime.MemoryMiB * 1024 * 1024
	var rawScratch string
	var rawMemory string
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		path, err := c.unpackRestoreArtifact(groupCtx, request.ScratchDisk, filepackScratchRole, "scratch.ext4", expectedScratchSize)
		if err != nil {
			return fmt.Errorf("unpack checkpoint scratch disk: %w", err)
		}
		rawScratch = path
		return nil
	})
	group.Go(func() error {
		path, err := c.unpackRestoreArtifact(groupCtx, request.Memory[0], filepackMemoryRole, "memory.mem", expectedMemorySize)
		if err != nil {
			return fmt.Errorf("unpack checkpoint memory: %w", err)
		}
		rawMemory = path
		return nil
	})
	if err := group.Wait(); err != nil {
		removeFiles([]string{rawScratch, rawMemory})
		return nil, err
	}
	cleanup := []string{rawScratch, rawMemory}
	session, err := c.start(ctx, rawMemory, request.VMState, rawScratch, &manifest.RuntimeState.Network)
	if err != nil {
		removeFiles(cleanup)
		return nil, err
	}
	return restoreCleanupSession{CheckpointableSession: session, paths: cleanup}, nil
}

func (c *Connector) validateRestoreIdentity(checkpointID string, manifestBytes []byte, identity vm.CheckpointIdentity) (snapshotManifest, error) {
	var manifest snapshotManifest
	if identity.RuntimeBackend != "firecracker" {
		return manifest, fmt.Errorf("checkpoint runtime backend %q is not supported", identity.RuntimeBackend)
	}
	if identity.RuntimeArch != runtime.GOARCH {
		return manifest, fmt.Errorf("checkpoint runtime arch %q does not match worker arch %q", identity.RuntimeArch, runtime.GOARCH)
	}
	if identity.RuntimeABI != runtimeABI {
		return manifest, fmt.Errorf("checkpoint runtime abi %q does not match worker abi %q", identity.RuntimeABI, runtimeABI)
	}
	if len(manifestBytes) == 0 {
		return manifest, errors.New("checkpoint manifest is required")
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return manifest, fmt.Errorf("decode checkpoint manifest: %w", err)
	}
	if manifest.RecoveryPoint.ID != checkpointID {
		return manifest, fmt.Errorf("checkpoint manifest recovery point id %q does not match restore id %q", manifest.RecoveryPoint.ID, checkpointID)
	}
	kernelDigest, err := digestFile(c.cfg.KernelPath)
	if err != nil {
		return manifest, fmt.Errorf("digest guest kernel: %w", err)
	}
	if identity.KernelDigest != kernelDigest {
		return manifest, fmt.Errorf("checkpoint kernel digest %s does not match worker kernel digest %s", identity.KernelDigest, kernelDigest)
	}
	initramfsDigest, err := digestFile(c.cfg.InitramfsPath)
	if err != nil {
		return manifest, fmt.Errorf("digest guest initramfs: %w", err)
	}
	if identity.InitramfsDigest != initramfsDigest {
		return manifest, fmt.Errorf("checkpoint initramfs digest %s does not match worker initramfs digest %s", identity.InitramfsDigest, initramfsDigest)
	}
	rootfsDigest, err := digestFile(c.cfg.RootfsPath)
	if err != nil {
		return manifest, fmt.Errorf("digest guest rootfs: %w", err)
	}
	if identity.RootfsDigest != rootfsDigest {
		return manifest, fmt.Errorf("checkpoint rootfs digest %s does not match worker rootfs digest %s", identity.RootfsDigest, rootfsDigest)
	}
	if identity.RuntimeConfigDigest != cas.DigestBytes(manifestBytes) {
		return manifest, fmt.Errorf("checkpoint runtime config digest %s does not match checkpoint manifest digest %s", identity.RuntimeConfigDigest, cas.DigestBytes(manifestBytes))
	}
	runtimeID, err := compute.RuntimeIdentityDigest(compute.RuntimeSelector{
		Arch:            runtime.GOARCH,
		ABI:             runtimeABI,
		KernelDigest:    kernelDigest,
		InitramfsDigest: initramfsDigest,
		RootfsDigest:    rootfsDigest,
		CNIProfile:      c.cfg.CNIProfile,
	})
	if err != nil {
		return manifest, err
	}
	if identity.RuntimeID != runtimeID {
		return manifest, fmt.Errorf("checkpoint runtime id %s does not match worker runtime id %s", identity.RuntimeID, runtimeID)
	}
	if err := validateRuntimeManifest(c.cfg, manifest, runtimeID, kernelDigest, initramfsDigest, rootfsDigest); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func (c *Connector) networkInterface(restoreNetwork *snapshotNetworkManifest) fc.NetworkInterface {
	cni := &fc.CNIConfiguration{
		NetworkName: c.cfg.CNINetworkName,
		ConfDir:     c.cfg.CNIConfDir,
		BinPath:     []string{c.cfg.CNIBinDir},
		CacheDir:    c.cfg.CNICacheDir,
		IfName:      c.cfg.CNIIfName,
		VMIfName:    c.cfg.CNIVMIfName,
	}
	if restoreNetwork != nil && restoreNetwork.GuestIPCIDR != "" {
		cni.Args = [][2]string{{"IP", restoreNetwork.GuestIPCIDR}}
	}
	return fc.NetworkInterface{CNIConfiguration: cni}
}

func (c *Connector) unpackRestoreArtifact(ctx context.Context, artifactPath string, role string, suffix string, expectedLogicalSize int64) (string, error) {
	if err := os.MkdirAll(c.cfg.StateDir, 0o700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(c.cfg.StateDir, "restore-*."+suffix)
	if err != nil {
		return "", err
	}
	targetPath := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(targetPath)
		return "", err
	}
	_ = os.Remove(targetPath)
	if err := unpackRuntimeFile(ctx, artifactPath, targetPath, role, expectedLogicalSize); err != nil {
		_ = os.Remove(targetPath)
		return "", err
	}
	return targetPath, nil
}

type restoreCleanupSession struct {
	vm.CheckpointableSession
	paths []string
}

func (s restoreCleanupSession) Close() error {
	err := s.CheckpointableSession.Close()
	removeFiles(s.paths)
	return err
}

func removeFiles(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

func (c *Connector) start(ctx context.Context, snapshotMemoryPath string, snapshotStatePath string, scratchDiskRestorePath string, restoreNetwork *snapshotNetworkManifest) (vm.CheckpointableSession, error) {
	instanceID := uuid.NewString()
	instanceDir := filepath.Join(c.cfg.StateDir, instanceID)
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		return nil, fmt.Errorf("create firecracker instance dir: %w", err)
	}
	cleanupInstanceDir := func() { _ = os.RemoveAll(instanceDir) }
	scratchDiskPath := filepath.Join(instanceDir, scratchDiskName)
	if strings.TrimSpace(scratchDiskRestorePath) != "" {
		scratchDiskPath = scratchDiskRestorePath
	} else if err := c.createScratchDisk(ctx, scratchDiskPath); err != nil {
		cleanupInstanceDir()
		return nil, err
	}
	if err := c.prepareScratchDiskForJailer(scratchDiskPath); err != nil {
		cleanupInstanceDir()
		return nil, err
	}
	jailRoot := jailRootPath(c.cfg, instanceID)
	cleanup := func() {
		cleanupInstanceDir()
		_ = os.RemoveAll(filepath.Dir(jailRoot))
	}

	vsockHostPath := filepath.Join(jailRoot, vsockSocketName)
	guestCID := allocateGuestCID()
	machineCfg := fc.Config{
		VMID:            instanceID,
		SocketPath:      apiSocketName,
		LogLevel:        "Info",
		KernelImagePath: c.cfg.KernelPath,
		InitrdPath:      c.cfg.InitramfsPath,
		KernelArgs:      defaultKernelArgs,
		NetNS:           filepath.Join("/var/run/netns", instanceID),
		Seccomp: fc.SeccompConfig{
			Enabled: true,
		},
		JailerCfg: &fc.JailerConfig{
			UID:            fc.Int(c.cfg.JailerUID),
			GID:            fc.Int(c.cfg.JailerGID),
			ID:             instanceID,
			NumaNode:       fc.Int(c.cfg.JailerNumaNode),
			ExecFile:       c.cfg.FirecrackerPath,
			JailerBinary:   c.cfg.JailerPath,
			ChrootBaseDir:  c.cfg.JailerChrootBaseDir,
			ChrootStrategy: fc.NewNaiveChrootStrategy(c.cfg.KernelPath),
			CgroupVersion:  c.cfg.CgroupVersion,
			Stdin:          nil,
			Stdout:         os.Stderr,
			Stderr:         os.Stderr,
		},
		Drives: []models.Drive{{
			DriveID:      fc.String("rootfs"),
			PathOnHost:   fc.String(c.cfg.RootfsPath),
			IsRootDevice: fc.Bool(true),
			IsReadOnly:   fc.Bool(true),
		}, {
			DriveID:      fc.String("scratch"),
			PathOnHost:   fc.String(scratchDiskPath),
			IsRootDevice: fc.Bool(false),
			IsReadOnly:   fc.Bool(false),
		}},
		VsockDevices: []fc.VsockDevice{{
			ID:   "guest-vsock",
			Path: vsockSocketName,
			CID:  guestCID,
		}},
		NetworkInterfaces: fc.NetworkInterfaces{c.networkInterface(restoreNetwork)},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  fc.Int64(c.cfg.VCPUCount),
			MemSizeMib: fc.Int64(c.cfg.MemoryMiB),
			Smt:        fc.Bool(false),
		},
	}
	opts := []fc.Opt{}
	restoring := snapshotMemoryPath != "" || snapshotStatePath != ""
	if restoring {
		opts = append(opts, withSnapshotRestore(snapshotMemoryPath, snapshotStatePath))
		opts = append(opts, withJailedRestoreFiles(c.cfg.RootfsPath, scratchDiskPath, snapshotMemoryPath, snapshotStatePath))
	}
	opts = append(opts, c.withTapOwner())
	opts = append(opts, c.withNetworkPolicy(instanceID))
	machine, err := fc.NewMachine(ctx, machineCfg, opts...)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("create firecracker machine: %w", err)
	}
	machineCtx, machineCancel := context.WithCancel(context.Background())
	if err := machine.Start(machineCtx); err != nil {
		_ = stopMachine(machine)
		machineCancel()
		_ = c.cleanupNetworkPolicy(context.Background(), instanceID)
		cleanup()
		return nil, fmt.Errorf("start firecracker machine: %w", err)
	}
	started := true
	defer func() {
		if !started {
			_ = stopMachine(machine)
			machineCancel()
			_ = c.cleanupNetworkPolicy(context.Background(), instanceID)
			cleanup()
		}
	}()
	if restoring {
		if err := machine.ResumeVM(ctx); err != nil {
			started = false
			return nil, fmt.Errorf("resume restored firecracker machine: %w", err)
		}
	}
	if !restoring {
		if err := c.waitForHealth(ctx, vsockHostPath); err != nil {
			started = false
			return nil, err
		}
	}
	conn, err := c.connectGuestPort(ctx, vsockHostPath)
	if err != nil {
		started = false
		return nil, err
	}
	return &guestSession{
		stream:        conn,
		machine:       machine,
		machineCancel: machineCancel,
		cfg:           c.cfg,
		instanceDir:   instanceDir,
		jailRoot:      jailRoot,
		scratchDisk:   scratchDiskPath,
		cleanup:       cleanup,
		networkPolicyCleanup: func() error {
			return c.cleanupNetworkPolicy(context.Background(), instanceID)
		},
	}, nil
}

func (c *Connector) createScratchDisk(ctx context.Context, scratchDiskPath string) error {
	file, err := os.OpenFile(scratchDiskPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create scratch disk: %w", err)
	}
	size := c.cfg.ScratchDiskMiB * 1024 * 1024
	truncateErr := file.Truncate(size)
	closeErr := file.Close()
	if truncateErr != nil {
		_ = os.Remove(scratchDiskPath)
		return fmt.Errorf("size scratch disk: %w", truncateErr)
	}
	if closeErr != nil {
		_ = os.Remove(scratchDiskPath)
		return fmt.Errorf("close scratch disk: %w", closeErr)
	}
	cmd := exec.CommandContext(ctx, c.cfg.MkfsExt4Path, "-F", "-q", scratchDiskPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(scratchDiskPath)
		return fmt.Errorf("format scratch disk: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (c *Connector) prepareScratchDiskForJailer(scratchDiskPath string) error {
	if err := os.Chown(scratchDiskPath, c.cfg.JailerUID, c.cfg.JailerGID); err != nil {
		return fmt.Errorf("chown scratch disk for jailer: %w", err)
	}
	if err := os.Chmod(scratchDiskPath, 0o600); err != nil {
		return fmt.Errorf("chmod scratch disk for jailer: %w", err)
	}
	return nil
}

func (c *Connector) withTapOwner() fc.Opt {
	return func(machine *fc.Machine) {
		machine.Handlers.FcInit = machine.Handlers.FcInit.AppendAfter(fc.SetupNetworkHandlerName, fc.Handler{
			Name: "helmr.SetTapOwner",
			Fn: func(ctx context.Context, machine *fc.Machine) error {
				for _, iface := range machine.Cfg.NetworkInterfaces {
					if iface.StaticConfiguration == nil || iface.StaticConfiguration.HostDevName == "" {
						continue
					}
					if err := setTapOwner(machine.Cfg.NetNS, iface.StaticConfiguration.HostDevName, c.cfg.JailerUID, c.cfg.JailerGID); err != nil {
						return err
					}
				}
				return nil
			},
		})
	}
}

func setTapOwner(netNSPath string, tapName string, uid int, gid int) error {
	if strings.TrimSpace(netNSPath) == "" {
		return setTapOwnerInCurrentNetNS(tapName, uid, gid)
	}
	netNS, err := ns.GetNS(netNSPath)
	if err != nil {
		return fmt.Errorf("open network namespace %q: %w", netNSPath, err)
	}
	defer netNS.Close()
	return netNS.Do(func(ns.NetNS) error {
		return setTapOwnerInCurrentNetNS(tapName, uid, gid)
	})
}

func setTapOwnerInCurrentNetNS(tapName string, uid int, gid int) error {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open /dev/net/tun: %w", err)
	}
	defer unix.Close(fd)

	ifr, err := unix.NewIfreq(tapName)
	if err != nil {
		return fmt.Errorf("build tap ifreq %q: %w", tapName, err)
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		return fmt.Errorf("open tap device %q: %w", tapName, err)
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETOWNER, uid); err != nil {
		return fmt.Errorf("set tap %q owner uid %d: %w", tapName, uid, err)
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETGROUP, gid); err != nil {
		return fmt.Errorf("set tap %q owner gid %d: %w", tapName, gid, err)
	}
	return nil
}

func (c *Connector) waitForHealth(ctx context.Context, vsockPath string) error {
	healthCtx, cancel := context.WithTimeout(ctx, c.cfg.HealthTimeout)
	defer cancel()
	var lastErr error
	for {
		conn, err := dialVsock(healthCtx, vsockPath, c.cfg.HealthPort)
		if err != nil {
			lastErr = err
			if healthCtx.Err() != nil {
				return fmt.Errorf("guest health probe timed out after %s: %w", c.cfg.HealthTimeout, errors.Join(healthCtx.Err(), err))
			}
			if err := sleepHealthRetry(healthCtx); err != nil {
				return fmt.Errorf("guest health probe timed out after %s: %w", c.cfg.HealthTimeout, errors.Join(err, lastErr))
			}
			continue
		}
		if deadline, ok := healthCtx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		}
		response, readErr := readHealth(conn)
		closeErr := conn.Close()
		if readErr != nil {
			lastErr = readErr
			if healthCtx.Err() != nil {
				return fmt.Errorf("guest health probe timed out after %s: %w", c.cfg.HealthTimeout, errors.Join(healthCtx.Err(), readErr))
			}
			if err := sleepHealthRetry(healthCtx); err != nil {
				return fmt.Errorf("guest health probe timed out after %s: %w", c.cfg.HealthTimeout, errors.Join(err, lastErr))
			}
			continue
		}
		if closeErr != nil {
			return fmt.Errorf("close guest health connection: %w", closeErr)
		}
		if response.Status == "ok" && response.Component == "guestd" {
			return nil
		}
		if response.Status != "starting" {
			return fmt.Errorf("guest health status=%q component=%q message=%q", response.Status, response.Component, response.Message)
		}
		if err := sleepHealthRetry(healthCtx); err != nil {
			return err
		}
	}
}

func (c *Connector) connectGuestPort(ctx context.Context, vsockPath string) (io.ReadWriteCloser, error) {
	connectCtx, cancel := context.WithTimeout(ctx, c.cfg.HealthTimeout)
	defer cancel()
	var lastErr error
	for {
		conn, err := dialVsock(connectCtx, vsockPath, c.cfg.GuestPort)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if connectCtx.Err() != nil {
			return nil, fmt.Errorf("guest port %d connection timed out after %s: %w", c.cfg.GuestPort, c.cfg.HealthTimeout, errors.Join(connectCtx.Err(), lastErr))
		}
		if err := sleepHealthRetry(connectCtx); err != nil {
			return nil, fmt.Errorf("guest port %d connection timed out after %s: %w", c.cfg.GuestPort, c.cfg.HealthTimeout, errors.Join(err, lastErr))
		}
	}
}

func sleepHealthRetry(ctx context.Context) error {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type healthResponse struct {
	Status    string `json:"status"`
	Component string `json:"component"`
	Message   string `json:"message,omitempty"`
}

func readHealth(conn io.ReadWriter) (healthResponse, error) {
	req, err := http.NewRequest(http.MethodGet, "http://guestd/", nil)
	if err != nil {
		return healthResponse{}, fmt.Errorf("build guest health request: %w", err)
	}
	req.Close = true
	if err := req.Write(conn); err != nil {
		return healthResponse{}, fmt.Errorf("write guest health request: %w", err)
	}
	httpResponse, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return healthResponse{}, fmt.Errorf("read guest health response: %w", err)
	}
	defer httpResponse.Body.Close()
	body, err := io.ReadAll(httpResponse.Body)
	if err != nil {
		return healthResponse{}, fmt.Errorf("read guest health response: %w", err)
	}
	if httpResponse.StatusCode != http.StatusOK {
		return healthResponse{}, fmt.Errorf("guest health returned HTTP %s: %s", httpResponse.Status, strings.TrimSpace(string(body)))
	}
	var response healthResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return healthResponse{}, fmt.Errorf("decode guest health response: %w", err)
	}
	return response, nil
}

type guestSession struct {
	stream               io.ReadWriteCloser
	machine              *fc.Machine
	machineCancel        context.CancelFunc
	cfg                  Config
	instanceDir          string
	jailRoot             string
	scratchDisk          string
	cleanup              func()
	networkPolicyCleanup func() error
	paused               atomic.Bool
	once                 sync.Once
	err                  error
}

func (s *guestSession) Stream() io.ReadWriteCloser {
	return s.stream
}

func (s *guestSession) Close() error {
	s.once.Do(func() {
		streamErr := s.stream.Close()
		if errors.Is(streamErr, net.ErrClosed) || errors.Is(streamErr, os.ErrClosed) {
			streamErr = nil
		}
		stopErr := stopMachine(s.machine)
		if s.machineCancel != nil {
			s.machineCancel()
		}
		var networkPolicyErr error
		if s.networkPolicyCleanup != nil {
			networkPolicyErr = s.networkPolicyCleanup()
		}
		s.cleanup()
		s.err = errors.Join(streamErr, networkPolicyErr, stopErr)
	})
	return s.err
}

func (s *guestSession) CreateSnapshot(ctx context.Context, request vm.SnapshotRequest) (vm.SnapshotArtifact, error) {
	checkpointID := safeSnapshotID(request.ID)
	memName := checkpointID + ".mem"
	stateName := checkpointID + ".vmstate"
	memPath := filepath.Join(s.jailRoot, memName)
	statePath := filepath.Join(s.jailRoot, stateName)
	if err := s.machine.PauseVM(ctx); err != nil {
		return vm.SnapshotArtifact{}, fmt.Errorf("pause firecracker vm: %w", err)
	}
	s.paused.Store(true)
	if err := s.machine.CreateSnapshot(ctx, path.Join("/", memName), path.Join("/", stateName)); err != nil {
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, fmt.Errorf("create firecracker snapshot: %w", err)
	}
	cleanupRawSnapshot := true
	defer func() {
		if cleanupRawSnapshot {
			_ = os.Remove(memPath)
			_ = os.Remove(statePath)
		}
	}()
	kernelDigest, err := digestFile(s.cfg.KernelPath)
	if err != nil {
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, fmt.Errorf("digest guest kernel: %w", err)
	}
	initramfsDigest, err := digestFile(s.cfg.InitramfsPath)
	if err != nil {
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, fmt.Errorf("digest guest initramfs: %w", err)
	}
	rootfsDigest, err := digestFile(s.cfg.RootfsPath)
	if err != nil {
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, fmt.Errorf("digest guest rootfs: %w", err)
	}
	runtimeID, err := compute.RuntimeIdentityDigest(compute.RuntimeSelector{
		Arch:            runtime.GOARCH,
		ABI:             runtimeABI,
		KernelDigest:    kernelDigest,
		InitramfsDigest: initramfsDigest,
		RootfsDigest:    rootfsDigest,
		CNIProfile:      s.cfg.CNIProfile,
	})
	if err != nil {
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, err
	}
	configDigest, manifest, err := snapshotRuntimeConfig(s.cfg, s.machine, checkpointID, runtimeID, kernelDigest, initramfsDigest, rootfsDigest)
	if err != nil {
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, err
	}
	var scratchPack string
	var memoryPack string
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		path, err := s.packSnapshotRuntimeFile(groupCtx, s.scratchDisk, filepackScratchRole, checkpointID+".scratch.filepack")
		if err != nil {
			return fmt.Errorf("pack checkpoint scratch disk: %w", err)
		}
		scratchPack = path
		return nil
	})
	group.Go(func() error {
		path, err := s.packSnapshotRuntimeFile(groupCtx, memPath, filepackMemoryRole, checkpointID+".memory.filepack")
		if err != nil {
			return fmt.Errorf("pack checkpoint memory: %w", err)
		}
		memoryPack = path
		return nil
	})
	if err := group.Wait(); err != nil {
		removeFiles([]string{scratchPack, memoryPack})
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, err
	}
	_ = os.Remove(memPath)
	cleanupRawSnapshot = false
	return vm.SnapshotArtifact{
		RuntimeBackend:      "firecracker",
		RuntimeArch:         runtime.GOARCH,
		RuntimeABI:          runtimeABI,
		RuntimeID:           runtimeID,
		KernelDigest:        kernelDigest,
		InitramfsDigest:     initramfsDigest,
		RootfsDigest:        rootfsDigest,
		RuntimeConfigDigest: configDigest,
		VMState:             vm.SnapshotFile{Path: statePath, MediaType: cas.CheckpointVMStateMediaType},
		ScratchDisk:         vm.SnapshotFile{Path: scratchPack, MediaType: cas.CheckpointScratchDiskMediaType},
		Memory:              []vm.SnapshotFile{{Path: memoryPack, MediaType: cas.CheckpointMemoryMediaType}},
		Manifest:            manifest,
	}, nil
}

func (s *guestSession) packSnapshotRuntimeFile(ctx context.Context, sourcePath string, role string, name string) (string, error) {
	targetPath := filepath.Join(filepath.Dir(s.scratchDisk), name)
	if err := packRuntimeFile(ctx, sourcePath, targetPath, role); err != nil {
		return "", err
	}
	return targetPath, nil
}

func (s *guestSession) Resume(ctx context.Context) error {
	if !s.paused.Load() {
		return nil
	}
	if err := s.machine.ResumeVM(ctx); err != nil {
		return fmt.Errorf("resume firecracker vm: %w", err)
	}
	s.paused.Store(false)
	return nil
}

func stopMachine(machine *fc.Machine) error {
	stopErr := machine.StopVMM()
	waitCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	waitErr := machine.Wait(waitCtx)
	return errors.Join(stopErr, ignoreExpectedStopErrors(waitErr))
}

func ignoreExpectedStopErrors(err error) error {
	if err == nil {
		return nil
	}
	type wrappedErrors interface {
		WrappedErrors() []error
	}
	var wrapped wrappedErrors
	if errors.As(err, &wrapped) {
		var out error
		for _, nested := range wrapped.WrappedErrors() {
			out = errors.Join(out, ignoreExpectedStopErrors(nested))
		}
		return out
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil {
		if status, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() && status.Signal() == syscall.SIGTERM {
			return nil
		}
	}
	return err
}

func safeSnapshotID(id string) string {
	if id == "" {
		return uuid.NewString()
	}
	out := make([]byte, 0, len(id))
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			out = append(out, byte(r))
		}
	}
	if len(out) == 0 {
		return uuid.NewString()
	}
	return string(out)
}

func digestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
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
	Backend         string                          `json:"backend"`
	ID              string                          `json:"id"`
	Arch            string                          `json:"arch"`
	ABI             string                          `json:"abi"`
	VCPUCount       int64                           `json:"vcpu_count"`
	MemoryMiB       int64                           `json:"memory_mib"`
	ScratchDiskMiB  int64                           `json:"scratch_disk_mib"`
	KernelArgs      string                          `json:"kernel_args"`
	KernelDigest    string                          `json:"kernel_digest"`
	InitramfsDigest string                          `json:"initramfs_digest"`
	RootfsDigest    string                          `json:"rootfs_digest"`
	GuestPort       uint32                          `json:"guest_port"`
	HealthPort      uint32                          `json:"health_port"`
	Network         snapshotNetworkIdentityManifest `json:"network"`
}

type snapshotRuntimeStateManifest struct {
	Network snapshotNetworkManifest `json:"network"`
}

type snapshotNetworkIdentityManifest struct {
	Mode        string `json:"mode"`
	Profile     string `json:"profile"`
	NetworkName string `json:"network_name"`
	IfName      string `json:"if_name"`
	VMIfName    string `json:"vm_if_name"`
}

type snapshotNetworkManifest struct {
	Mode        string `json:"mode"`
	Profile     string `json:"profile"`
	NetworkName string `json:"network_name"`
	IfName      string `json:"if_name"`
	VMIfName    string `json:"vm_if_name"`
	GuestIPCIDR string `json:"guest_ip_cidr,omitempty"`
}

func snapshotRuntimeConfig(cfg Config, machine *fc.Machine, checkpointID string, runtimeID string, kernelDigest string, initramfsDigest string, rootfsDigest string) (string, []byte, error) {
	network := snapshotNetworkConfig(cfg, machine)
	if network.GuestIPCIDR == "" {
		return "", nil, errors.New("firecracker CNI guest IP is required for checkpoint restore")
	}
	manifest, err := json.Marshal(snapshotManifest{
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
					Mode:        network.Mode,
					Profile:     network.Profile,
					NetworkName: network.NetworkName,
					IfName:      network.IfName,
					VMIfName:    network.VMIfName,
				},
			},
		},
		RuntimeState: snapshotRuntimeStateManifest{
			Network: network,
		},
	})
	if err != nil {
		return "", nil, fmt.Errorf("encode firecracker snapshot manifest: %w", err)
	}
	return cas.DigestBytes(manifest), manifest, nil
}

func snapshotNetworkConfig(cfg Config, machine *fc.Machine) snapshotNetworkManifest {
	network := snapshotNetworkManifest{
		Mode:        "cni",
		Profile:     cfg.CNIProfile,
		NetworkName: cfg.CNINetworkName,
		IfName:      cfg.CNIIfName,
		VMIfName:    cfg.CNIVMIfName,
	}
	if machine == nil || len(machine.Cfg.NetworkInterfaces) == 0 {
		return network
	}
	static := machine.Cfg.NetworkInterfaces[0].StaticConfiguration
	if static == nil || static.IPConfiguration == nil {
		return network
	}
	network.GuestIPCIDR = static.IPConfiguration.IPAddr.String()
	return network
}

func validateRuntimeManifest(cfg Config, manifest snapshotManifest, runtimeID string, kernelDigest string, initramfsDigest string, rootfsDigest string) error {
	runtimeManifest := manifest.RecoveryPoint.Runtime
	if runtimeManifest.Backend != "firecracker" {
		return fmt.Errorf("checkpoint manifest runtime backend %q is not supported", runtimeManifest.Backend)
	}
	if runtimeManifest.Arch != runtime.GOARCH {
		return fmt.Errorf("checkpoint manifest runtime arch %q does not match worker arch %q", runtimeManifest.Arch, runtime.GOARCH)
	}
	if runtimeManifest.ABI != runtimeABI {
		return fmt.Errorf("checkpoint manifest runtime abi %q does not match worker abi %q", runtimeManifest.ABI, runtimeABI)
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
	if runtimeManifest.VCPUCount != cfg.VCPUCount || runtimeManifest.MemoryMiB != cfg.MemoryMiB {
		return fmt.Errorf("checkpoint manifest machine shape vcpu=%d memory=%d does not match worker vcpu=%d memory=%d", runtimeManifest.VCPUCount, runtimeManifest.MemoryMiB, cfg.VCPUCount, cfg.MemoryMiB)
	}
	if runtimeManifest.ScratchDiskMiB != cfg.ScratchDiskMiB {
		return fmt.Errorf("checkpoint manifest scratch disk size %d MiB does not match worker scratch disk size %d MiB", runtimeManifest.ScratchDiskMiB, cfg.ScratchDiskMiB)
	}
	if runtimeManifest.KernelArgs != defaultKernelArgs || runtimeManifest.GuestPort != cfg.GuestPort || runtimeManifest.HealthPort != cfg.HealthPort {
		return errors.New("checkpoint manifest runtime ports or kernel args do not match worker runtime")
	}
	networkIdentity := runtimeManifest.Network
	if networkIdentity.Mode != "cni" {
		return fmt.Errorf("checkpoint manifest network mode %q is not supported", networkIdentity.Mode)
	}
	if networkIdentity.Profile != cfg.CNIProfile || networkIdentity.NetworkName != cfg.CNINetworkName || networkIdentity.IfName != cfg.CNIIfName || networkIdentity.VMIfName != cfg.CNIVMIfName {
		return errors.New("checkpoint manifest CNI configuration does not match worker CNI configuration")
	}
	network := manifest.RuntimeState.Network
	if network.Mode != "cni" {
		return fmt.Errorf("checkpoint manifest network mode %q is not supported", network.Mode)
	}
	if network.Profile != cfg.CNIProfile || network.NetworkName != cfg.CNINetworkName || network.IfName != cfg.CNIIfName || network.VMIfName != cfg.CNIVMIfName {
		return errors.New("checkpoint manifest CNI configuration does not match worker CNI configuration")
	}
	if network.GuestIPCIDR == "" {
		return errors.New("checkpoint manifest guest_ip_cidr is required for CNI restore")
	}
	return nil
}

func allocateGuestCID() uint32 {
	return 2 + nextGuestCID.Add(1)
}

func jailRootPath(cfg Config, id string) string {
	return filepath.Join(cfg.JailerChrootBaseDir, filepath.Base(cfg.FirecrackerPath), id, "root")
}

func withSnapshotRestore(memoryPath string, statePath string) fc.Opt {
	return func(machine *fc.Machine) {
		fc.WithSnapshot(memoryPath, statePath)(machine)
		machine.Handlers.FcInit = machine.Handlers.FcInit.Remove(fc.AddVsocksHandlerName)
	}
}

func withJailedRestoreFiles(rootfsPath string, scratchDiskPath string, memoryPath string, statePath string) fc.Opt {
	return func(machine *fc.Machine) {
		machine.Handlers.Validation = machine.Handlers.Validation.Append(fc.JailerConfigValidationHandler)
		machine.Handlers.FcInit = machine.Handlers.FcInit.AppendAfter(fc.CreateLogFilesHandlerName, fc.Handler{
			Name: "fcinit.LinkHelmrRestoreFilesToRootFS",
			Fn: func(ctx context.Context, machine *fc.Machine) error {
				root := jailRootPath(Config{
					FirecrackerPath:     machine.Cfg.JailerCfg.ExecFile,
					JailerChrootBaseDir: machine.Cfg.JailerCfg.ChrootBaseDir,
				}, machine.Cfg.JailerCfg.ID)
				if err := linkIntoJail(rootfsPath, root, filepath.Base(rootfsPath)); err != nil {
					return fmt.Errorf("link rootfs into jail: %w", err)
				}
				for i := range machine.Cfg.Drives {
					if fc.StringValue(machine.Cfg.Drives[i].PathOnHost) == rootfsPath {
						machine.Cfg.Drives[i].PathOnHost = fc.String(filepath.Base(rootfsPath))
					}
				}
				if err := linkIntoJailForVMM(scratchDiskPath, root, scratchDiskName, *machine.Cfg.JailerCfg.UID, *machine.Cfg.JailerCfg.GID); err != nil {
					return fmt.Errorf("link scratch disk into jail: %w", err)
				}
				for i := range machine.Cfg.Drives {
					if fc.StringValue(machine.Cfg.Drives[i].PathOnHost) == scratchDiskPath {
						machine.Cfg.Drives[i].PathOnHost = fc.String(scratchDiskName)
					}
				}
				if err := linkIntoJailForVMM(memoryPath, root, filepath.Base(memoryPath), *machine.Cfg.JailerCfg.UID, *machine.Cfg.JailerCfg.GID); err != nil {
					return fmt.Errorf("link snapshot memory into jail: %w", err)
				}
				if err := linkIntoJailForVMM(statePath, root, filepath.Base(statePath), *machine.Cfg.JailerCfg.UID, *machine.Cfg.JailerCfg.GID); err != nil {
					return fmt.Errorf("link snapshot state into jail: %w", err)
				}
				machine.Cfg.Snapshot.MemFilePath = path.Join("/", filepath.Base(memoryPath))
				machine.Cfg.Snapshot.SnapshotPath = path.Join("/", filepath.Base(statePath))
				return nil
			},
		})
	}
}

func linkIntoJailForVMM(source string, root string, name string, uid int, gid int) error {
	if err := linkIntoJail(source, root, name); err != nil {
		return err
	}
	return chownJailFile(filepath.Join(root, name), uid, gid)
}

func linkIntoJail(source string, root string, name string) error {
	dest := filepath.Join(root, name)
	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Link(source, dest); err == nil {
		return nil
	}
	if err := cloneSparseFile(source, dest); err == nil {
		return nil
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	return errors.Join(copyErr, closeErr)
}

func cloneSparseFile(source string, dest string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	output, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	closed := false
	cleanup := true
	defer func() {
		if !closed {
			_ = output.Close()
		}
		if cleanup {
			_ = os.Remove(dest)
		}
	}()
	if err := output.Truncate(info.Size()); err != nil {
		return err
	}
	if err := copySparseFile(input, output, info.Size()); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		closed = true
		return err
	}
	closed = true
	cleanup = false
	return nil
}

func copySparseFile(input *os.File, output *os.File, logicalSize int64) error {
	offset := int64(0)
	buffer := make([]byte, 4<<20)
	for offset < logicalSize {
		dataStart, dataEnd, nextOffset, sparse, err := nextDataRange(input, offset, logicalSize)
		if err != nil {
			return err
		}
		if !sparse {
			return copySparseRange(input, output, buffer, offset, logicalSize)
		}
		if dataStart < dataEnd {
			if err := copySparseRange(input, output, buffer, dataStart, dataEnd); err != nil {
				return err
			}
		}
		offset = nextOffset
	}
	return nil
}

func copySparseRange(input *os.File, output *os.File, buffer []byte, start int64, end int64) error {
	for offset := start; offset < end; {
		remaining := end - offset
		n := int64(len(buffer))
		if remaining < n {
			n = remaining
		}
		chunk := buffer[:n]
		if err := readFullAt(input, chunk, offset); err != nil {
			return err
		}
		if !allZero(chunk) {
			if _, err := output.WriteAt(chunk, offset); err != nil {
				return err
			}
		}
		offset += n
	}
	return nil
}

func chownJailFile(path string, uid int, gid int) error {
	if err := os.Chown(path, uid, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
