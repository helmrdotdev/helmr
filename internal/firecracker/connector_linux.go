//go:build linux

package firecracker

import (
	"bufio"
	"context"
	"encoding/binary"
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"uuid"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/operations"
	"github.com/firecracker-microvm/firecracker-go-sdk/vsock"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/firecracker/datapath"
	"github.com/helmrdotdev/helmr/internal/runtimeid"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

const stopTimeout = 10 * time.Second
const maxGuestHealthResponseBytes = 4096
const ext4SuperblockOffset = 1024
const ext4SuperblockBytes = 1024
const ext4Magic = 0xef53

var nextGuestCID atomic.Uint32
var dialVsock = vsock.DialContext

type Connector struct {
	cfg         Config
	artifacts   runtimeArtifacts
	kernelArgs  string
	datapath    *datapath.Manager
	hostRuntime *hostRuntimeEvidenceStore
}

// QualifiedRuntime is the only Firecracker value that implements workload VM
// interfaces. A raw Connector is a candidate host runtime until Qualify proves
// the exact jailer, device, network, VMM, and Guest health path.
type QualifiedRuntime struct {
	connector    *Connector
	evidence     HostRuntimeEvidence
	capabilities RuntimeCapabilities
}

func NewConnector(cfg Config) (*Connector, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	artifacts, err := loadRuntimeArtifacts(cfg)
	if err != nil {
		return nil, err
	}
	return &Connector{
		cfg:         cfg,
		artifacts:   artifacts,
		kernelArgs:  defaultKernelArgs,
		datapath:    datapath.NewManager(),
		hostRuntime: newHostRuntimeEvidenceStore(),
	}, nil
}

func (c *Connector) Qualify(ctx context.Context) (*QualifiedRuntime, error) {
	if c == nil {
		return nil, errors.New("the Firecracker runtime candidate is nil")
	}
	if err := c.preflight(ctx); err != nil {
		return nil, fmt.Errorf("preflight Firecracker runtime candidate: %w", err)
	}
	evidence, err := c.hostRuntimeEvidence(ctx)
	if err != nil {
		return nil, fmt.Errorf("inspect Firecracker host runtime: %w", err)
	}
	if err := c.probeGuest(ctx); err != nil {
		return nil, fmt.Errorf("prove jailed Firecracker Guest readiness: %w", err)
	}
	capabilities, err := c.runtimeCapabilities()
	if err != nil {
		return nil, fmt.Errorf("inspect Firecracker runtime capabilities: %w", err)
	}
	return &QualifiedRuntime{
		connector: c, evidence: evidence, capabilities: capabilities,
	}, nil
}

func (runtime *QualifiedRuntime) DatapathHealth() error {
	if runtime == nil || runtime.connector == nil {
		return errors.New("the qualified Firecracker runtime is nil")
	}
	return runtime.connector.datapathHealth()
}

func (c *Connector) datapathHealth() error {
	if c == nil {
		return errors.New("the Firecracker connector is nil")
	}
	return c.datapath.Health()
}

func (runtime *QualifiedRuntime) RuntimeCapabilities() RuntimeCapabilities {
	return runtime.capabilities
}

func (runtime *QualifiedRuntime) HostRuntimeEvidence() HostRuntimeEvidence {
	return runtime.evidence
}

func (c *Connector) runtimeCapabilities() (RuntimeCapabilities, error) {
	capabilities, err := runtimeArtifactCapabilities(c.artifacts)
	if err != nil {
		return RuntimeCapabilities{}, err
	}
	capabilities.VCPUCount = c.cfg.VCPUCount
	capabilities.MemoryMiB = c.cfg.MemoryMiB
	return capabilities, nil
}

// ProbeGuest proves that the exact bound host runtime can cross the jailer,
// device, network, VMM, and Guest health boundaries before the Worker
// advertises capacity to the Control Plane.
func (c *Connector) probeGuest(ctx context.Context) error {
	if c == nil {
		return errors.New("the Firecracker connector is nil")
	}
	probeCtx, cancelProbe := context.WithTimeout(
		ctx,
		c.cfg.InitTimeout+c.cfg.HealthTimeout+stopTimeout,
	)
	defer cancelProbe()
	identity, err := c.hostRuntime.runtimeIdentity()
	if err != nil {
		return fmt.Errorf("resolve startup probe runtime identity: %w", err)
	}
	ownerID := uuid.NewV7().String()
	session, err := c.connect(probeCtx, vm.ConnectRequest{
		ID:        ownerID,
		OwnerKind: vm.OwnerRuntime,
		Binding: vm.WorkloadBinding{
			WorkerEpoch:       1,
			OwnerID:           ownerID,
			Generation:        1,
			RuntimeInstanceID: ownerID,
			RuntimeIdentityID: identity.ID,
		},
	})
	if err != nil {
		return fmt.Errorf("start the Firecracker startup probe Guest: %w", err)
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
	defer cancel()
	if err := session.Close(cleanupCtx); err != nil {
		return fmt.Errorf("clean the Firecracker startup probe Guest: %w", err)
	}
	return nil
}

func (c *Connector) connect(ctx context.Context, request vm.ConnectRequest) (vm.Session, error) {
	owner := vm.Owner{Kind: request.OwnerKind, ID: request.ID}
	if err := request.Binding.Validate(owner); err != nil {
		return nil, fmt.Errorf("the Firecracker workload binding: %w", err)
	}
	child, err := c.connectorForRequest(request)
	if err != nil {
		return nil, err
	}
	return child.start(
		ctx,
		request.ID,
		request.OwnerKind,
		request.Binding,
		"",
		"",
		"",
		nil,
		request.Topology,
		request.ReadOnlyDrives,
		nil,
		false,
	)
}

func (c *Connector) connectorForRequest(
	request vm.ConnectRequest,
) (*Connector, error) {
	cfg := c.cfg
	if request.OwnerKind != vm.OwnerRuntime {
		return nil, errors.New("the Firecracker owner kind is invalid")
	}
	if len(request.ReadOnlyDrives) != 0 {
		return nil, errors.New("runtime attachment cannot add read-only drives")
	}
	if request.Resources != (compute.ResourceVector{}) {
		return nil, errors.New("runtime attachment cannot change resources")
	}
	child := *c
	child.cfg = cfg
	child.kernelArgs = runtimeKernelArgs(request.Topology, nil)
	return &child, nil
}

func isProgramDriveSet(drives []vm.ReadOnlyDrive) bool {
	if len(drives) != 2 {
		return false
	}
	present := make(map[string]bool, len(drives))
	for _, drive := range drives {
		switch drive.ID {
		case vm.ProgramRuntimeDrive,
			vm.ProgramDrive:
			present[drive.ID] = true
		default:
			return false
		}
	}
	return present[vm.ProgramRuntimeDrive] &&
		present[vm.ProgramDrive]
}

func (runtime *QualifiedRuntime) Materialize(ctx context.Context, request vm.MaterializeRequest) (vm.Session, error) {
	return runtime.connector.materialize(ctx, request)
}

func (c *Connector) materialize(ctx context.Context, request vm.MaterializeRequest) (vm.Session, error) {
	if err := c.validateMaterializeRequest(request); err != nil {
		return nil, err
	}
	cfg, err := c.configForMaterializeRequest(request)
	if err != nil {
		return nil, err
	}
	child := *c
	child.cfg = cfg
	child.kernelArgs = runtimeKernelArgs(request.Topology, request.ReadOnlyDrives)
	return child.start(
		ctx,
		request.ID,
		request.OwnerKind,
		request.Binding,
		"",
		"",
		"",
		nil,
		request.Topology,
		request.ReadOnlyDrives,
		request.RecordPhase,
		false,
	)
}

func (runtime *QualifiedRuntime) Cleanup(ctx context.Context, owner vm.Owner) error {
	return runtime.connector.cleanup(ctx, owner)
}

type connectorCleaner struct{ connector *Connector }

func (cleaner connectorCleaner) Cleanup(ctx context.Context, owner vm.Owner) error {
	return cleaner.connector.cleanup(ctx, owner)
}

func (c *Connector) cleanup(ctx context.Context, owner vm.Owner) error {
	if err := owner.Validate(); err != nil {
		return cleanupUnproven(owner, err)
	}
	statePath := filepath.Join(c.cfg.StateDir, owner.ID)
	jailerPath := filepath.Join(c.cfg.JailerChrootBaseDir, "firecracker", owner.ID)
	pids, err := exactRuntimePIDs(owner.ID)
	if err != nil {
		return cleanupUnproven(owner, fmt.Errorf("inventory Firecracker processes: %w", err))
	}
	netns, err := c.runtimeNetNSExists(ctx, owner.ID)
	if err != nil {
		return cleanupUnproven(owner, err)
	}
	stateExists, err := pathExists(statePath)
	if err != nil {
		return cleanupUnproven(owner, fmt.Errorf("inspect Firecracker state: %w", err))
	}
	jailerExists, err := pathExists(jailerPath)
	if err != nil {
		return cleanupUnproven(owner, fmt.Errorf("inspect Firecracker jailer state: %w", err))
	}
	if !stateExists && !jailerExists && !netns && len(pids) == 0 {
		return nil
	}
	if !stateExists {
		return cleanupUnproven(owner, errors.New("the Firecracker ownership marker is missing"))
	}
	if err := validateOwnerMarker(statePath, owner); err != nil {
		return cleanupUnproven(owner, err)
	}
	for _, pid := range pids {
		if err := stopExactRuntimePID(ctx, pid); err != nil {
			return cleanupUnproven(owner, fmt.Errorf("stop Firecracker process %d: %w", pid, err))
		}
	}
	if err := c.cleanupNetworkAttachment(ctx, owner); err != nil {
		return cleanupUnproven(owner, err)
	}
	if err := os.RemoveAll(jailerPath); err != nil {
		return cleanupUnproven(owner, fmt.Errorf("remove Firecracker jailer state: %w", err))
	}
	remaining, err := exactRuntimePIDs(owner.ID)
	if err != nil {
		return cleanupUnproven(owner, fmt.Errorf("verify Firecracker processes absent: %w", err))
	}
	if len(remaining) != 0 {
		return cleanupUnproven(owner, fmt.Errorf("verify Firecracker processes absent: pids=%v", remaining))
	}
	if exists, verifyErr := c.runtimeNetNSExists(ctx, owner.ID); verifyErr != nil {
		return cleanupUnproven(owner, fmt.Errorf("verify Firecracker netns absent: %w", verifyErr))
	} else if exists {
		return cleanupUnproven(owner, errors.New("verify Firecracker netns absent: namespace remains"))
	}
	if exists, verifyErr := pathExists(jailerPath); verifyErr != nil {
		return cleanupUnproven(owner, fmt.Errorf("verify Firecracker jailer state absent: %w", verifyErr))
	} else if exists {
		return cleanupUnproven(owner, errors.New("verify Firecracker jailer state absent: path remains"))
	}
	if err := removeStateRootLast(statePath, owner); err != nil {
		return cleanupUnproven(owner, err)
	}
	if exists, verifyErr := pathExists(statePath); verifyErr != nil {
		return cleanupUnproven(owner, fmt.Errorf("verify Firecracker state absent: %w", verifyErr))
	} else if exists {
		return cleanupUnproven(owner, errors.New("verify Firecracker state absent: path remains"))
	}
	return nil
}

func cleanupUnproven(owner vm.Owner, cause error) error {
	return &vm.CleanupUnprovenError{Owner: owner, Cause: cause}
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}

func validateOwnerMarker(statePath string, owner vm.Owner) error {
	info, err := os.Stat(statePath)
	if err != nil {
		return fmt.Errorf("inspect Firecracker ownership root: %w", err)
	}
	if !info.IsDir() {
		return errors.New("the Firecracker ownership root is not a directory")
	}
	marker, err := os.ReadFile(filepath.Join(statePath, "owner"))
	if err != nil {
		return fmt.Errorf("read Firecracker ownership marker: %w", err)
	}
	if string(marker) != string(owner.Kind)+"\n"+owner.ID+"\n" {
		return errors.New("the Firecracker ownership marker does not match exact owner")
	}
	return nil
}

func removeStateRootLast(statePath string, owner vm.Owner) error {
	entries, err := os.ReadDir(statePath)
	if err != nil {
		return fmt.Errorf("inventory Firecracker state: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() == "owner" {
			continue
		}
		if err := os.RemoveAll(filepath.Join(statePath, entry.Name())); err != nil {
			return fmt.Errorf("remove Firecracker state entry %q: %w", entry.Name(), err)
		}
	}
	markerPath := filepath.Join(statePath, "owner")
	if err := os.Remove(markerPath); err != nil {
		return fmt.Errorf("remove Firecracker ownership marker: %w", err)
	}
	if err := os.Remove(statePath); err != nil {
		restoreErr := os.WriteFile(markerPath, []byte(string(owner.Kind)+"\n"+owner.ID+"\n"), 0o600)
		return errors.Join(fmt.Errorf("remove Firecracker state root: %w", err), restoreErr)
	}
	return nil
}

func (c *Connector) runtimeNetNSExists(ctx context.Context, runtimeID string) (bool, error) {
	output, err := exec.CommandContext(ctx, c.cfg.IPPath, "netns", "list").Output()
	if err != nil {
		return false, fmt.Errorf("inventory Firecracker cleanup netns: %w", err)
	}
	for line := range strings.SplitSeq(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 0 && fields[0] == runtimeID {
			return true, nil
		}
	}
	return false, nil
}

func exactRuntimePIDs(runtimeID string) ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, entry := range entries {
		pid, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil {
			continue
		}
		cmdline, readErr := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if readErr != nil {
			continue
		}
		args := strings.Split(strings.TrimSuffix(string(cmdline), "\x00"), "\x00")
		if len(args) == 0 || (filepath.Base(args[0]) != "firecracker" && filepath.Base(args[0]) != "jailer") {
			continue
		}
		for _, arg := range args[1:] {
			if arg == runtimeID {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids, nil
}

func stopExactRuntimePID(ctx context.Context, pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		if err := process.Signal(syscall.Signal(0)); errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return process.Kill()
		case <-ticker.C:
		}
	}
}

func (c *Connector) validateMaterializeRequest(request vm.MaterializeRequest) error {
	if request.OwnerKind != vm.OwnerRuntime {
		return errors.New("the Firecracker materialize owner must be runtime")
	}
	if err := request.Binding.Validate(vm.Owner{Kind: request.OwnerKind, ID: request.ID}); err != nil {
		return fmt.Errorf("the Firecracker workload binding: %w", err)
	}
	if err := validateReadOnlyDrives(request.ReadOnlyDrives); err != nil {
		return err
	}
	if len(request.ReadOnlyDrives) != 0 && !isProgramDriveSet(request.ReadOnlyDrives) {
		return errors.New("runtime read-only drives must be the complete program drive set")
	}
	if isProgramDriveSet(request.ReadOnlyDrives) {
		if err := validateProgramDriveIdentities(request.ReadOnlyDrives); err != nil {
			return err
		}
	}
	rootfsDigest := c.artifacts.Rootfs.Digest
	if rootfsDigest != strings.TrimSpace(request.RootfsDigest) {
		return fmt.Errorf("workspaceMount rootfs digest %s does not match declared digest %s", rootfsDigest, request.RootfsDigest)
	}
	if strings.TrimSpace(request.WorkspaceMountPath) != "/workspace" {
		return fmt.Errorf("the Firecracker materialize workspace mount path %q is not supported", request.WorkspaceMountPath)
	}
	requestedVCPUs, err := VCPUCountForMilliCPU(request.Resources.MilliCPU)
	if err != nil {
		return fmt.Errorf("derive materialize VM vCPU count: %w", err)
	}
	if request.VMVCPUCount <= 0 || int64(request.VMVCPUCount) != requestedVCPUs {
		return fmt.Errorf(
			"materialize VM vCPU count %d does not match %d milliCPU-derived vCPUs %d",
			request.VMVCPUCount,
			request.Resources.MilliCPU,
			requestedVCPUs,
		)
	}
	if !sha256sum.ValidDigest(request.CPUConfigDigest) {
		return errors.New("materialize guest CPU configuration digest is not canonical")
	}
	targetRuntime, targetCPUConfigDigest, _, err := c.boundSessionRuntime(requestedVCPUs)
	if err != nil {
		return fmt.Errorf("resolve materialize host runtime: %w", err)
	}
	if request.Binding.RuntimeIdentityID != targetRuntime.ID {
		return fmt.Errorf(
			"materialize runtime identity %s does not match target host runtime %s",
			request.Binding.RuntimeIdentityID,
			targetRuntime.ID,
		)
	}
	if request.CPUConfigDigest != targetCPUConfigDigest {
		return fmt.Errorf(
			"materialize guest CPU configuration digest %s does not match target digest %s for %d vCPUs",
			request.CPUConfigDigest,
			targetCPUConfigDigest,
			requestedVCPUs,
		)
	}
	return nil
}

func runtimeKernelArgs(
	topology vm.RuntimeTopology,
	readOnlyDrives []vm.ReadOnlyDrive,
) string {
	args := defaultKernelArgs
	if topology.Substrate != nil {
		args += " " + runtimeSubstrateKernelFlag
	}
	if isProgramDriveSet(readOnlyDrives) {
		args += " " + runtimeProgramKernelFlag
	}
	return args
}

func (c *Connector) configForMaterializeRequest(request vm.MaterializeRequest) (Config, error) {
	return c.configForResources(request.Resources, "materialize")
}

func (c *Connector) configForResources(resources compute.ResourceVector, operation string) (Config, error) {
	cfg := c.cfg
	if err := resources.Validate(true); err != nil {
		return Config{}, fmt.Errorf("%s resources: %w", operation, err)
	}
	if resources.MemoryMiB > 0 {
		if resources.MemoryMiB > cfg.MemoryMiB {
			return Config{}, fmt.Errorf("%s requested memory %d MiB exceeds worker VM memory capacity %d MiB", operation, resources.MemoryMiB, cfg.MemoryMiB)
		}
		cfg.MemoryMiB = resources.MemoryMiB
	}
	if resources.MilliCPU > 0 {
		requestedVCPUs, err := VCPUCountForMilliCPU(resources.MilliCPU)
		if err != nil {
			return Config{}, fmt.Errorf("%s requested cpu: %w", operation, err)
		}
		if requestedVCPUs > cfg.VCPUCount {
			return Config{}, fmt.Errorf("%s requested cpu %d milliCPU exceeds worker VM vCPU capacity %d", operation, resources.MilliCPU, cfg.VCPUCount)
		}
		cfg.VCPUCount = requestedVCPUs
	}
	if resources.DiskMiB > 0 {
		if resources.DiskMiB > cfg.ScratchDiskMiB {
			return Config{}, fmt.Errorf("%s requested disk %d MiB exceeds worker VM scratch disk capacity %d MiB", operation, resources.DiskMiB, cfg.ScratchDiskMiB)
		}
		cfg.ScratchDiskMiB = resources.DiskMiB
	}
	return cfg, nil
}

func (c *Connector) kernelArgsValue() string {
	if strings.TrimSpace(c.kernelArgs) == "" {
		return defaultKernelArgs
	}
	return c.kernelArgs
}

func (runtime *QualifiedRuntime) Restore(ctx context.Context, request vm.RestoreRequest) (vm.Session, error) {
	return runtime.connector.restore(ctx, request)
}

func (c *Connector) restore(ctx context.Context, request vm.RestoreRequest) (vm.Session, error) {
	if err := request.Binding.Validate(vm.Owner{Kind: request.OwnerKind, ID: request.RuntimeInstanceID}); err != nil {
		return nil, fmt.Errorf("the Firecracker workload binding: %w", err)
	}
	targetRuntime, err := c.hostRuntime.runtimeIdentity()
	if err != nil {
		return nil, fmt.Errorf("resolve target host runtime identity: %w", err)
	}
	if _, err := c.hostRuntime.firecrackerExecutable(); err != nil {
		return nil, fmt.Errorf("resolve target Firecracker executable: %w", err)
	}
	if request.Binding.RuntimeIdentityID != targetRuntime.ID {
		return nil, fmt.Errorf(
			"restore workload runtime identity %s does not match target host runtime %s",
			request.Binding.RuntimeIdentityID,
			targetRuntime.ID,
		)
	}
	if len(request.Memory) != 1 {
		return nil, fmt.Errorf("the Firecracker restore requires exactly one memory file, got %d", len(request.Memory))
	}
	if len(request.MemoryMediaTypes) != 1 {
		return nil, fmt.Errorf("the Firecracker restore requires exactly one memory media type, got %d", len(request.MemoryMediaTypes))
	}
	if strings.TrimSpace(request.VMState) == "" {
		return nil, errors.New("the Firecracker restore vm state path is required")
	}
	if request.VMStateMediaType != cas.CheckpointVMStateMediaType {
		return nil, fmt.Errorf("the Firecracker restore vm state media type %q is not supported", request.VMStateMediaType)
	}
	recordPhase := request.RecordPhase
	started := time.Now()
	if err := validateReadOnlyDrives(request.ReadOnlyDrives); err != nil {
		return nil, err
	}
	if len(request.ReadOnlyDrives) != 0 && !isProgramDriveSet(request.ReadOnlyDrives) {
		return nil, errors.New("the Firecracker restore read-only drives must be the complete program drive set")
	}
	if isProgramDriveSet(request.ReadOnlyDrives) {
		if err := validateProgramDriveIdentities(request.ReadOnlyDrives); err != nil {
			return nil, err
		}
	}
	kernelArgs := runtimeKernelArgs(request.Topology, request.ReadOnlyDrives)
	manifest, restoreCfg, err := c.validateRestoreIdentity(
		request.ID,
		request.Manifest,
		request.Checkpoint,
		request.Topology,
		kernelArgs,
		request.ReadOnlyDrives,
	)
	recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_validate_identity", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)), ErrorClass: vm.RuntimeErrorClass(err)})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.ScratchDisk) == "" {
		return nil, errors.New("the Firecracker restore scratch disk path is required")
	}
	if request.ScratchDiskMediaType != cas.CheckpointScratchDiskMediaType {
		return nil, fmt.Errorf("the Firecracker restore scratch disk media type %q is not supported", request.ScratchDiskMediaType)
	}
	if request.MemoryMediaTypes[0] != cas.CheckpointMemoryMediaType {
		return nil, fmt.Errorf("the Firecracker restore memory media type %q is not supported", request.MemoryMediaTypes[0])
	}
	owner := vm.Owner{Kind: request.OwnerKind, ID: request.RuntimeInstanceID}
	ownerDir, err := createOwnerStateRoot(c.cfg.StateDir, owner)
	if err != nil {
		return nil, err
	}
	expectedScratchSize := manifest.RecoveryPoint.Runtime.ScratchDiskMiB * 1024 * 1024
	expectedMemorySize := manifest.RecoveryPoint.Runtime.MemoryMiB * 1024 * 1024
	var rawScratch string
	var rawMemory string
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		path, phase, err := c.unpackRestoreArtifact(groupCtx, ownerDir, request.ScratchDisk, filepackScratchRole, scratchDiskName, expectedScratchSize, cas.CheckpointScratchDiskMediaType)
		recordRuntimePhase(recordPhase, phase)
		if err != nil {
			return fmt.Errorf("unpack checkpoint scratch disk: %w", err)
		}
		rawScratch = path
		return nil
	})
	group.Go(func() error {
		path, phase, err := c.unpackRestoreArtifact(groupCtx, ownerDir, request.Memory[0], filepackMemoryRole, restoreMemoryName, expectedMemorySize, cas.CheckpointMemoryMediaType)
		recordRuntimePhase(recordPhase, phase)
		if err != nil {
			return fmt.Errorf("unpack checkpoint memory: %w", err)
		}
		rawMemory = path
		return nil
	})
	if err := group.Wait(); err != nil {
		removeFiles([]string{rawScratch, rawMemory})
		return nil, errors.Join(err, removeStateRootLast(ownerDir, owner))
	}
	child := *c
	child.cfg = restoreCfg
	child.kernelArgs = kernelArgs
	session, err := child.start(ctx, request.RuntimeInstanceID, request.OwnerKind, request.Binding, rawMemory, request.VMState, rawScratch, &manifest.RuntimeState.Network, request.Topology, request.ReadOnlyDrives, recordPhase, true)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func (c *Connector) validateRestoreIdentity(
	checkpointID string,
	manifestBytes []byte,
	identity vm.CheckpointIdentity,
	topology vm.RuntimeTopology,
	kernelArgs string,
	readOnlyDrives []vm.ReadOnlyDrive,
) (snapshotManifest, Config, error) {
	var manifest snapshotManifest
	targetRuntime, err := c.hostRuntime.runtimeIdentity()
	if err != nil {
		return manifest, Config{}, fmt.Errorf("resolve target host runtime identity: %w", err)
	}
	if identity.RuntimeBackend != "firecracker" {
		return manifest, Config{}, fmt.Errorf("checkpoint runtime backend %q is not supported", identity.RuntimeBackend)
	}
	workerArchitecture := targetRuntime.Arch
	if identity.RuntimeArch != workerArchitecture {
		return manifest, Config{}, fmt.Errorf("checkpoint runtime arch %q does not match worker arch %q", identity.RuntimeArch, workerArchitecture)
	}
	if identity.VMRuntimeContract != targetRuntime.Contract {
		return manifest, Config{}, fmt.Errorf("checkpoint runtime contract %q does not match worker contract %q", identity.VMRuntimeContract, targetRuntime.Contract)
	}
	if len(manifestBytes) == 0 {
		return manifest, Config{}, errors.New("checkpoint manifest is required")
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return manifest, Config{}, fmt.Errorf("decode checkpoint manifest: %w", err)
	}
	if manifest.RecoveryPoint.ID != checkpointID {
		return manifest, Config{}, fmt.Errorf("checkpoint manifest recovery point id %q does not match restore id %q", manifest.RecoveryPoint.ID, checkpointID)
	}
	kernelDigest := targetRuntime.KernelDigest
	if identity.KernelDigest != kernelDigest {
		return manifest, Config{}, fmt.Errorf("checkpoint kernel digest %s does not match worker kernel digest %s", identity.KernelDigest, kernelDigest)
	}
	initramfsDigest := targetRuntime.InitramfsDigest
	if identity.InitramfsDigest != initramfsDigest {
		return manifest, Config{}, fmt.Errorf("checkpoint initramfs digest %s does not match worker initramfs digest %s", identity.InitramfsDigest, initramfsDigest)
	}
	rootfsDigest := targetRuntime.RootfsDigest
	if identity.RootfsDigest != rootfsDigest {
		return manifest, Config{}, fmt.Errorf("checkpoint rootfs digest %s does not match worker rootfs digest %s", identity.RootfsDigest, rootfsDigest)
	}
	if identity.RuntimeConfigDigest != sha256sum.DigestBytes(manifestBytes) {
		return manifest, Config{}, fmt.Errorf("checkpoint runtime config digest %s does not match checkpoint manifest digest %s", identity.RuntimeConfigDigest, sha256sum.DigestBytes(manifestBytes))
	}
	runtimeID := targetRuntime.ID
	if identity.RuntimeID != runtimeID {
		return manifest, Config{}, fmt.Errorf("checkpoint runtime id %s does not match worker runtime id %s", identity.RuntimeID, runtimeID)
	}
	if identity.VMVCPUCount <= 0 {
		return manifest, Config{}, fmt.Errorf("checkpoint VM vCPU count %d is invalid", identity.VMVCPUCount)
	}
	if int64(identity.VMVCPUCount) != manifest.RecoveryPoint.Runtime.VCPUCount {
		return manifest, Config{}, fmt.Errorf(
			"checkpoint VM vCPU count %d does not match checkpoint manifest vCPU count %d",
			identity.VMVCPUCount,
			manifest.RecoveryPoint.Runtime.VCPUCount,
		)
	}
	if !sha256sum.ValidDigest(identity.CPUConfigDigest) {
		return manifest, Config{}, errors.New("checkpoint guest CPU configuration digest is not canonical")
	}
	if identity.CPUConfigDigest != manifest.RecoveryPoint.Runtime.CPUConfigDigest {
		return manifest, Config{}, fmt.Errorf(
			"checkpoint guest CPU configuration digest %s does not match checkpoint manifest digest %s",
			identity.CPUConfigDigest,
			manifest.RecoveryPoint.Runtime.CPUConfigDigest,
		)
	}
	restoreCfg, err := c.configForRestoreManifest(manifest)
	if err != nil {
		return manifest, Config{}, err
	}
	targetCPUConfigDigest, err := c.hostRuntime.cpuConfigDigest(int64(identity.VMVCPUCount))
	if err != nil {
		return manifest, Config{}, fmt.Errorf("resolve target guest CPU configuration: %w", err)
	}
	if identity.CPUConfigDigest != targetCPUConfigDigest {
		return manifest, Config{}, fmt.Errorf(
			"checkpoint guest CPU configuration digest %s does not match target digest %s for %d vCPUs",
			identity.CPUConfigDigest,
			targetCPUConfigDigest,
			identity.VMVCPUCount,
		)
	}
	if err := validateRuntimeManifest(
		restoreCfg,
		manifest,
		runtimeID,
		kernelDigest,
		initramfsDigest,
		rootfsDigest,
		identity.CPUConfigDigest,
		topology.Substrate,
		kernelArgs,
		readOnlyDrives,
	); err != nil {
		return manifest, Config{}, err
	}
	return manifest, restoreCfg, nil
}

func (c *Connector) configForRestoreManifest(manifest snapshotManifest) (Config, error) {
	cfg := c.cfg
	runtimeManifest := manifest.RecoveryPoint.Runtime
	if runtimeManifest.VCPUCount <= 0 {
		return Config{}, fmt.Errorf("checkpoint manifest vcpu count %d is invalid", runtimeManifest.VCPUCount)
	}
	if runtimeManifest.MemoryMiB <= 0 {
		return Config{}, fmt.Errorf("checkpoint manifest memory %d MiB is invalid", runtimeManifest.MemoryMiB)
	}
	if runtimeManifest.ScratchDiskMiB <= 0 {
		return Config{}, fmt.Errorf("checkpoint manifest scratch disk size %d MiB is invalid", runtimeManifest.ScratchDiskMiB)
	}
	if runtimeManifest.VCPUCount > cfg.VCPUCount {
		return Config{}, fmt.Errorf("checkpoint manifest vcpu count %d exceeds worker capacity %d", runtimeManifest.VCPUCount, cfg.VCPUCount)
	}
	if runtimeManifest.MemoryMiB > cfg.MemoryMiB {
		return Config{}, fmt.Errorf("checkpoint manifest memory %d MiB exceeds worker capacity %d MiB", runtimeManifest.MemoryMiB, cfg.MemoryMiB)
	}
	if runtimeManifest.ScratchDiskMiB > cfg.ScratchDiskMiB {
		return Config{}, fmt.Errorf("checkpoint manifest scratch disk size %d MiB exceeds worker capacity %d MiB", runtimeManifest.ScratchDiskMiB, cfg.ScratchDiskMiB)
	}
	cfg.VCPUCount = runtimeManifest.VCPUCount
	cfg.MemoryMiB = runtimeManifest.MemoryMiB
	cfg.ScratchDiskMiB = runtimeManifest.ScratchDiskMiB
	return cfg, nil
}

func (c *Connector) unpackRestoreArtifact(ctx context.Context, ownerDir string, artifactPath string, role string, suffix string, expectedLogicalSize int64, mediaType string) (string, vm.RuntimePhase, error) {
	started := time.Now()
	phase := vm.RuntimePhase{
		Name:      "restore_unpack_" + strings.ReplaceAll(role, "-", "_") + "_filepack",
		Role:      role,
		MediaType: mediaType,
	}
	if role == filepackScratchRole {
		phase.Name = "restore_unpack_scratch_filepack"
	}
	file, err := os.CreateTemp(ownerDir, "restore-*."+suffix)
	if err != nil {
		phase.DurationMs = vm.RuntimeDurationMilliseconds(time.Since(started))
		phase.ErrorClass = vm.RuntimeErrorClass(err)
		return "", phase, err
	}
	targetPath := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(targetPath)
		phase.DurationMs = vm.RuntimeDurationMilliseconds(time.Since(started))
		phase.ErrorClass = vm.RuntimeErrorClass(err)
		return "", phase, err
	}
	_ = os.Remove(targetPath)
	stats, err := unpackRuntimeFile(ctx, artifactPath, targetPath, role, expectedLogicalSize)
	phase.DurationMs = vm.RuntimeDurationMilliseconds(time.Since(started))
	if err == nil || stats.LogicalBytes != 0 || stats.EncodedChunks != 0 || stats.UnpackWrittenBytes != 0 {
		phase.Filepack = &stats
	}
	if err != nil {
		_ = os.Remove(targetPath)
		phase.ErrorClass = vm.RuntimeErrorClass(err)
		return "", phase, err
	}
	return targetPath, phase, nil
}

func removeFiles(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

func (c *Connector) start(ctx context.Context, instanceID string, ownerKind vm.OwnerKind, binding vm.WorkloadBinding, snapshotMemoryPath string, snapshotStatePath string, scratchDiskRestorePath string, restoreNetwork *snapshotNetworkManifest, topology vm.RuntimeTopology, readOnlyDrives []vm.ReadOnlyDrive, recordPhase func(vm.RuntimePhase), ownerPrepared bool) (vm.CheckpointableSession, error) {
	session, err := c.prepareSession(ctx, instanceID, ownerKind, binding, snapshotMemoryPath, snapshotStatePath, scratchDiskRestorePath, restoreNetwork, topology, readOnlyDrives, recordPhase, ownerPrepared)
	if err != nil {
		return nil, err
	}
	if _, err := session.Open(ctx); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		return nil, errors.Join(err, session.Close(cleanupCtx))
	}
	return session, nil
}

func (c *Connector) prepareSession(ctx context.Context, instanceID string, ownerKind vm.OwnerKind, binding vm.WorkloadBinding, snapshotMemoryPath string, snapshotStatePath string, scratchDiskRestorePath string, restoreNetwork *snapshotNetworkManifest, topology vm.RuntimeTopology, readOnlyDrives []vm.ReadOnlyDrive, recordPhase func(vm.RuntimePhase), ownerPrepared bool) (_ *guestSession, retErr error) {
	if err := validateCPUTemplateLaunch(c.cfg.CPUTemplateSelector); err != nil {
		return nil, err
	}
	runtimeIdentity, cpuConfigDigest, firecrackerPath, err := c.boundSessionRuntime(c.cfg.VCPUCount)
	if err != nil {
		return nil, err
	}
	launchCfg := c.cfg
	launchCfg.FirecrackerPath = firecrackerPath
	instanceID = strings.TrimSpace(instanceID)
	owner := vm.Owner{Kind: ownerKind, ID: instanceID}
	if err := owner.Validate(); err != nil {
		return nil, fmt.Errorf("the Firecracker owner: %w", err)
	}
	if err := binding.Validate(owner); err != nil {
		return nil, fmt.Errorf("the Firecracker workload binding: %w", err)
	}
	if binding.RuntimeIdentityID != runtimeIdentity.ID {
		return nil, fmt.Errorf(
			"workload runtime identity %s does not match bound host runtime %s",
			binding.RuntimeIdentityID,
			runtimeIdentity.ID,
		)
	}
	instanceDir := filepath.Join(c.cfg.StateDir, instanceID)
	if ownerPrepared {
		if err := validateOwnerMarker(instanceDir, owner); err != nil {
			return nil, fmt.Errorf("validate prepared Firecracker ownership evidence: %w", err)
		}
	} else {
		var err error
		instanceDir, err = createOwnerStateRoot(c.cfg.StateDir, owner)
		if err != nil {
			return nil, err
		}
	}
	defer func() {
		if retErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
			defer cancel()
			retErr = errors.Join(retErr, c.cleanup(cleanupCtx, owner))
		}
	}()
	scratchDiskPath := filepath.Join(instanceDir, scratchDiskName)
	if strings.TrimSpace(scratchDiskRestorePath) != "" {
		scratchDiskPath = scratchDiskRestorePath
	} else {
		phaseStarted := time.Now()
		err := c.createScratchDisk(ctx, scratchDiskPath)
		recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "materialize_create_scratch_disk", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
		if err != nil {
			return nil, err
		}
	}
	phaseStarted := time.Now()
	if err := c.prepareScratchDiskForJailer(scratchDiskPath); err != nil {
		recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_prepare_scratch_for_jailer", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
		return nil, err
	}
	recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_prepare_scratch_for_jailer", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted))})
	substrateDiskPath := ""
	if topology.Substrate != nil {
		if err := validateRuntimeSubstrateSource(topology.Substrate); err != nil {
			return nil, err
		}
		phaseStarted = time.Now()
		var err error
		substrateDiskPath, err = topology.Substrate.Source.MaterializeInto(
			ctx,
			instanceDir,
			substrateDiskName,
			c.cfg.JailerUID,
			c.cfg.JailerGID,
		)
		if err != nil {
			recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "prepare_substrate_for_jailer", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
			return nil, err
		}
		projected := cloneRuntimeSubstrate(topology.Substrate)
		projected.Path = substrateDiskPath
		projected.Source = nil
		topology.Substrate = projected
		recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "prepare_substrate_for_jailer", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted))})
	}
	restoring := snapshotMemoryPath != "" || snapshotStatePath != ""
	readOnlyDrivePaths := map[string]string(nil)
	if restoring && len(readOnlyDrives) != 0 {
		readOnlyDrivePaths, err = prepareRestoreReadOnlyDrivePaths(
			instanceDir,
			readOnlyDrives,
			c.cfg.JailerUID,
			c.cfg.JailerGID,
		)
		if err != nil {
			return nil, err
		}
	}
	jailRoot := jailRootPath(launchCfg, instanceID)

	vsockHostPath := filepath.Join(jailRoot, vsockSocketName)
	guestCID := allocateGuestCID()
	var chrootStrategy firecracker.HandlersAdapter = firecracker.NewNaiveChrootStrategy(
		c.cfg.KernelPath,
	)
	if len(readOnlyDrives) != 0 {
		chrootStrategy = sealedDriveChrootStrategy{
			kernelImagePath: c.cfg.KernelPath,
			drives:          readOnlyDrives,
		}
	}
	runtimeDescriptor := CanonicalVMRuntimeDescriptor()
	machineCfg := firecracker.Config{
		VMID:            instanceID,
		SocketPath:      apiSocketName,
		LogLevel:        "Info",
		KernelImagePath: c.cfg.KernelPath,
		InitrdPath:      c.cfg.InitramfsPath,
		KernelArgs:      c.kernelArgsValue(),
		Seccomp: firecracker.SeccompConfig{
			Enabled: true,
		},
		JailerCfg: &firecracker.JailerConfig{
			UID:            firecracker.Int(c.cfg.JailerUID),
			GID:            firecracker.Int(c.cfg.JailerGID),
			ID:             instanceID,
			NumaNode:       firecracker.Int(c.cfg.JailerNumaNode),
			ExecFile:       launchCfg.FirecrackerPath,
			JailerBinary:   c.cfg.JailerPath,
			ChrootBaseDir:  c.cfg.JailerChrootBaseDir,
			ChrootStrategy: chrootStrategy,
			CgroupVersion:  c.cfg.CgroupVersion,
			Stdin:          nil,
			Stdout:         os.Stderr,
			Stderr:         os.Stderr,
		},
		Drives: runtimeDrivesWithReadOnlyPaths(
			c.cfg.RootfsPath,
			scratchDiskPath,
			substrateDiskPath,
			readOnlyDrives,
			readOnlyDrivePaths,
		),
		VsockDevices: []firecracker.VsockDevice{runtimeVsockDevice(runtimeDescriptor, guestCID)},
		MachineCfg:   runtimeMachineConfiguration(runtimeDescriptor, c.cfg),
	}
	machineCfg.NetNS = filepath.Join("/var/run/netns", instanceID)
	machineCfg.NetworkInterfaces = firecracker.NetworkInterfaces{staticNetworkInterface(c.cfg.NetworkResolverIPv4)}
	var networkBinding *installedNetworkBinding
	defer func() {
		if retErr != nil && networkBinding != nil {
			retErr = errors.Join(retErr, networkBinding.Close())
		}
	}()
	opts := []firecracker.Opt{}
	if restoring {
		opts = append(opts, withSnapshotRestore(snapshotMemoryPath, snapshotStatePath))
		opts = append(opts, withJailedRestoreFiles(c.cfg.RootfsPath, scratchDiskPath, substrateDiskPath, snapshotMemoryPath, snapshotStatePath))
		if len(readOnlyDrives) != 0 {
			opts = append(opts, withRestoreSealedDrives(sealedDriveChrootStrategy{
				kernelImagePath: c.cfg.KernelPath,
				drives:          readOnlyDrives,
			}))
		}
	}
	opts = append(opts, c.withTapOwner())
	opts = append(opts, c.withNetworkBinding(owner, binding, &networkBinding))
	// firecracker-go-sdk binds this context to the jailer/firecracker process.
	// Keep it separate from the startup request so prepared sessions can outlive
	// a background warm command after boot succeeds.
	machineCtx, machineCancel := context.WithCancel(context.Background())
	phaseStarted = time.Now()
	machine, err := newSDKMachine(machineCtx, machineCfg, c.cfg.InitTimeout, opts...)
	recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_create_firecracker_machine", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
	if err != nil {
		machineCancel()
		return nil, fmt.Errorf("create Firecracker machine: %w", err)
	}
	machine.Logger().Printf("starting Firecracker machine")
	phaseStarted = time.Now()
	if err := startMachineContext(ctx, machine, machineCtx, machineCancel); err != nil {
		recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_start_firecracker_machine", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
		_ = stopMachine(context.Background(), machine)
		return nil, fmt.Errorf("start Firecracker machine: %w", err)
	}
	recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_start_firecracker_machine", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted))})
	machineExit := watchMachineExit(machine)
	machine.Logger().Printf("Firecracker machine start returned")
	started := true
	defer func() {
		if !started {
			stopErr := stopSessionMachine(context.Background(), machine, machineExit)
			machineCancel()
			retErr = errors.Join(retErr, stopErr)
		}
	}()
	if restoring {
		phaseStarted = time.Now()
		if err := validateRestoredNetworkConfig(*restoreNetwork, snapshotNetworkConfig(c.cfg)); err != nil {
			recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_validate_network", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
			started = false
			return nil, err
		}
		recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_validate_network", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted))})
		phaseStarted = time.Now()
		if err := machine.ResumeVM(ctx); err != nil {
			recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_resume_firecracker_snapshot", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
			started = false
			return nil, fmt.Errorf("resume restored Firecracker machine: %w", err)
		}
		recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_resume_firecracker_snapshot", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted))})
	}
	machine.Logger().Printf("waiting for guest health")
	phaseStarted = time.Now()
	err = c.waitForHealth(ctx, vsockHostPath, machineExit, machine.Logger().Printf)
	recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_wait_guest_health", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
	if err != nil {
		started = false
		return nil, err
	}
	machine.Logger().Printf("guest health ready")
	session := &guestSession{
		machine:         machine,
		machineCancel:   machineCancel,
		machineExit:     machineExit,
		cfg:             launchCfg,
		kernelArgs:      c.kernelArgsValue(),
		runtimeIdentity: runtimeIdentity,
		cpuConfigDigest: cpuConfigDigest,
		vsockHostPath:   vsockHostPath,
		instanceDir:     instanceDir,
		jailRoot:        jailRoot,
		scratchDisk:     scratchDiskPath,
		topology:        topology,
		readOnlyDrives:  append([]vm.ReadOnlyDrive(nil), readOnlyDrives...),
		owner:           owner,
		cleaner:         connectorCleaner{connector: c},
		networkBinding:  networkBinding,
	}
	session.watchNetworkFailure()
	return session, nil
}

func (c *Connector) boundSessionRuntime(vcpuCount int64) (runtimeid.Profile, string, string, error) {
	runtimeIdentity, err := c.hostRuntime.runtimeIdentity()
	if err != nil {
		return runtimeid.Profile{}, "", "", fmt.Errorf("resolve host runtime identity: %w", err)
	}
	cpuConfigDigest, err := c.hostRuntime.cpuConfigDigest(vcpuCount)
	if err != nil {
		return runtimeid.Profile{}, "", "", fmt.Errorf("resolve guest CPU configuration for %d vCPUs: %w", vcpuCount, err)
	}
	firecrackerPath, err := c.hostRuntime.firecrackerExecutable()
	if err != nil {
		return runtimeid.Profile{}, "", "", fmt.Errorf("resolve pinned Firecracker executable: %w", err)
	}
	return runtimeIdentity, cpuConfigDigest, firecrackerPath, nil
}

func startMachineContext(ctx context.Context, machine *firecracker.Machine, machineCtx context.Context, machineCancel context.CancelFunc) error {
	result := make(chan error, 1)
	go func() {
		result <- machine.Start(machineCtx)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		machineCancel()
		return ctx.Err()
	}
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
	cmd := exec.CommandContext(
		ctx,
		c.cfg.MkfsExt4Path,
		"-F",
		"-q",
		"-m",
		"0",
		scratchDiskPath,
	)
	cmd.Env = []string{
		"LC_ALL=C.UTF-8",
		"LANG=C.UTF-8",
		"TZ=UTC",
		"MKE2FS_CONFIG=" + c.cfg.Mke2fsConfigPath,
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(scratchDiskPath)
		return fmt.Errorf("format scratch disk: %w: %s", err, strings.TrimSpace(string(output)))
	}
	floor := c.scratchUsableFloor()
	if floor > 0 {
		usable, err := ext4FreeBytes(scratchDiskPath)
		if err != nil {
			_ = os.Remove(scratchDiskPath)
			return fmt.Errorf("inspect scratch filesystem: %w", err)
		}
		if usable < floor {
			_ = os.Remove(scratchDiskPath)
			return fmt.Errorf(
				"scratch filesystem has %d usable bytes, build contract requires at least %d",
				usable,
				floor,
			)
		}
	}
	return nil
}

func (c *Connector) scratchUsableFloor() uint64 {
	return 0
}

func ext4FreeBytes(path string) (uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	superblock := make([]byte, ext4SuperblockBytes)
	if _, err := io.ReadFull(
		io.NewSectionReader(file, ext4SuperblockOffset, ext4SuperblockBytes),
		superblock,
	); err != nil {
		return 0, err
	}
	if binary.LittleEndian.Uint16(superblock[56:58]) != ext4Magic {
		return 0, errors.New("scratch filesystem is not ext4")
	}
	logBlockSize := binary.LittleEndian.Uint32(superblock[24:28])
	if logBlockSize > 6 {
		return 0, fmt.Errorf("invalid ext4 block size shift %d", logBlockSize)
	}
	freeBlocks := uint64(binary.LittleEndian.Uint32(superblock[12:16])) |
		uint64(binary.LittleEndian.Uint32(superblock[0x158:0x15c]))<<32
	blockSize := uint64(1024) << logBlockSize
	if freeBlocks > ^uint64(0)/blockSize {
		return 0, errors.New("ext4 free byte count overflows")
	}
	return freeBlocks * blockSize, nil
}

func runtimeDrives(
	rootfsPath string,
	scratchDiskPath string,
	substrateDiskPath string,
	readOnlyDrives []vm.ReadOnlyDrive,
) []models.Drive {
	return runtimeDrivesWithReadOnlyPaths(
		rootfsPath,
		scratchDiskPath,
		substrateDiskPath,
		readOnlyDrives,
		nil,
	)
}

func runtimeDrivesWithReadOnlyPaths(
	rootfsPath string,
	scratchDiskPath string,
	substrateDiskPath string,
	readOnlyDrives []vm.ReadOnlyDrive,
	readOnlyDrivePaths map[string]string,
) []models.Drive {
	drives := []models.Drive{{
		DriveID:      firecracker.String(rootfsDriveID),
		PathOnHost:   firecracker.String(rootfsPath),
		IsRootDevice: firecracker.Bool(true),
		IsReadOnly:   firecracker.Bool(true),
	}, {
		DriveID:      firecracker.String(scratchDriveID),
		PathOnHost:   firecracker.String(scratchDiskPath),
		IsRootDevice: firecracker.Bool(false),
		IsReadOnly:   firecracker.Bool(false),
	}}
	if strings.TrimSpace(substrateDiskPath) != "" {
		drives = append(drives, models.Drive{
			DriveID:      firecracker.String(substrateDriveID),
			PathOnHost:   firecracker.String(substrateDiskPath),
			IsRootDevice: firecracker.Bool(false),
			IsReadOnly:   firecracker.Bool(true),
		})
	}
	byID := make(map[string]vm.ReadOnlyDrive, len(readOnlyDrives))
	for _, drive := range readOnlyDrives {
		byID[drive.ID] = drive
	}
	for _, id := range readOnlyDriveOrder {
		if _, exists := byID[id]; exists {
			pathOnHost := readOnlyDriveName(id)
			if stagedPath, staged := readOnlyDrivePaths[id]; staged {
				pathOnHost = stagedPath
			}
			drives = append(drives, models.Drive{
				DriveID:      firecracker.String(id),
				PathOnHost:   firecracker.String(pathOnHost),
				IsRootDevice: firecracker.Bool(false),
				IsReadOnly:   firecracker.Bool(true),
			})
		}
	}
	return drives
}

func prepareRestoreReadOnlyDrivePaths(
	ownerDir string,
	drives []vm.ReadOnlyDrive,
	uid int,
	gid int,
) (map[string]string, error) {
	if !filepath.IsAbs(ownerDir) {
		return nil, errors.New("restore owner directory must be absolute")
	}
	paths := make(map[string]string, len(drives))
	byID := make(map[string]vm.ReadOnlyDrive, len(drives))
	for _, drive := range drives {
		byID[drive.ID] = drive
	}
	for _, id := range readOnlyDriveOrder {
		drive, exists := byID[id]
		if !exists {
			continue
		}
		name := readOnlyDriveName(id)
		if err := drive.Source.LinkInto(ownerDir, name, uid, gid); err != nil {
			return nil, fmt.Errorf(
				"link sealed restore drive %q into owner state: %w",
				id,
				err,
			)
		}
		path := filepath.Join(ownerDir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return nil, fmt.Errorf("inspect sealed restore drive %q: %w", id, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("sealed restore drive %q is not regular", id)
		}
		paths[id] = path
	}
	return paths, nil
}

func runtimeMachineConfiguration(descriptor VMRuntimeDescriptor, cfg Config) models.MachineConfiguration {
	return models.MachineConfiguration{
		VcpuCount:       firecracker.Int64(cfg.VCPUCount),
		MemSizeMib:      firecracker.Int64(cfg.MemoryMiB),
		Smt:             firecracker.Bool(descriptor.Machine.SMT),
		TrackDirtyPages: descriptor.Machine.TrackDirtyPages,
	}
}

func runtimeVsockDevice(descriptor VMRuntimeDescriptor, guestCID uint32) firecracker.VsockDevice {
	return firecracker.VsockDevice{
		ID: descriptor.Devices.Vsock.ID, Path: descriptor.Paths.VsockSocket, CID: guestCID,
	}
}

func validateReadOnlyDrives(drives []vm.ReadOnlyDrive) error {
	ids := make(map[string]struct{}, len(drives))
	for index, drive := range drives {
		switch drive.ID {
		case vm.ProgramRuntimeDrive,
			vm.ProgramDrive:
		default:
			return fmt.Errorf(
				"read-only drive %d ID %q is invalid",
				index,
				drive.ID,
			)
		}
		if drive.Source == nil {
			return fmt.Errorf("read-only drive %q source is nil", drive.ID)
		}
		if _, exists := ids[drive.ID]; exists {
			return fmt.Errorf("read-only drive ID %q is duplicated", drive.ID)
		}
		ids[drive.ID] = struct{}{}
	}
	return nil
}

func validateProgramDriveIdentities(drives []vm.ReadOnlyDrive) error {
	if !isProgramDriveSet(drives) {
		return errors.New("managed program requires the complete read-only drive set")
	}
	for _, drive := range drives {
		if _, err := cas.ObjectKey("", drive.Digest); err != nil {
			return fmt.Errorf("managed program drive %q digest: %w", drive.ID, err)
		}
		if drive.SizeBytes <= 0 {
			return fmt.Errorf("managed program drive %q size must be positive", drive.ID)
		}
		if strings.TrimSpace(drive.MediaType) == "" || drive.MediaType != strings.TrimSpace(drive.MediaType) {
			return fmt.Errorf("managed program drive %q media type must be canonical", drive.ID)
		}
	}
	return nil
}

func readOnlyDriveName(id string) string {
	return id + readOnlyDriveSuffix
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

func (c *Connector) withTapOwner() firecracker.Opt {
	return func(machine *firecracker.Machine) {
		machine.Handlers.FcInit = machine.Handlers.FcInit.AppendAfter(firecracker.SetupNetworkHandlerName, firecracker.Handler{
			Name: "helmr.SetTapOwner",
			Fn: func(ctx context.Context, machine *firecracker.Machine) error {
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

func (c *Connector) waitForHealth(ctx context.Context, vsockPath string, machineExit *machineExit, logf func(string, ...interface{})) error {
	healthCtx, cancel := context.WithTimeout(ctx, c.cfg.HealthTimeout)
	defer cancel()
	stats := newHealthProbeStats()
	for {
		if err, ok := machineExit.Err(); ok {
			stats.machineExited = true
			stats.lastErr = err
			result := stats.failureError("the Firecracker machine exited during guest health wait", err)
			stats.log(logf, "failed")
			return result
		}
		stats.attempts++
		attemptStarted := time.Now()
		attemptCtx, cancelAttempt := healthAttemptContext(healthCtx, c.cfg.HealthAttemptTimeout)
		conn, err := dialVsock(attemptCtx, vsockPath, c.cfg.HealthPort)
		if err != nil {
			cancelAttempt()
			stats.recordError("dial", err)
			stats.logFailedAttempt(logf, time.Since(attemptStarted))
			if healthCtx.Err() != nil {
				result := stats.timeoutError(c.cfg.HealthTimeout, healthCtx.Err(), err, machineExit)
				stats.log(logf, "failed")
				return result
			}
			if err := sleepHealthRetry(healthCtx); err != nil {
				result := stats.timeoutError(c.cfg.HealthTimeout, err, stats.lastErr, machineExit)
				stats.log(logf, "failed")
				return result
			}
			continue
		}
		if deadline, ok := attemptCtx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		}
		response, readErr := readHealth(conn)
		closeErr := conn.Close()
		cancelAttempt()
		if readErr != nil {
			stats.recordError(healthProbeErrorBucket(readErr), readErr)
			stats.logFailedAttempt(logf, time.Since(attemptStarted))
			if healthCtx.Err() != nil {
				result := stats.timeoutError(c.cfg.HealthTimeout, healthCtx.Err(), readErr, machineExit)
				stats.log(logf, "failed")
				return result
			}
			if err := sleepHealthRetry(healthCtx); err != nil {
				result := stats.timeoutError(c.cfg.HealthTimeout, err, stats.lastErr, machineExit)
				stats.log(logf, "failed")
				return result
			}
			continue
		}
		if closeErr != nil {
			stats.recordError("close", closeErr)
			stats.logFailedAttempt(logf, time.Since(attemptStarted))
			result := stats.failureError("close guest health connection", closeErr)
			stats.log(logf, "failed")
			return result
		}
		if response.Status == "ok" && response.Component == "guestd" {
			stats.log(logf, "ready")
			return nil
		}
		if response.Status != "starting" {
			stats.recordStatus(response.Status)
			stats.logFailedAttempt(logf, time.Since(attemptStarted))
			result := stats.failureError(fmt.Sprintf("guest health status=%q component=%q message=%q", response.Status, response.Component, response.Message), nil)
			stats.log(logf, "failed")
			return result
		}
		stats.recordStarting(response)
		if err := sleepHealthRetry(healthCtx); err != nil {
			result := stats.timeoutError(c.cfg.HealthTimeout, err, stats.lastErr, machineExit)
			stats.log(logf, "failed")
			return result
		}
	}
}

func healthAttemptContext(ctx context.Context, attemptTimeout time.Duration) (context.Context, context.CancelFunc) {
	if attemptTimeout <= 0 {
		attemptTimeout = DefaultHealthAttemptTimeout
	}
	return context.WithTimeout(ctx, attemptTimeout)
}

func (c *Connector) connectGuestPort(ctx context.Context, vsockPath string, machineExit *machineExit) (vm.Stream, error) {
	return c.connectGuestPortAt(ctx, vsockPath, c.cfg.GuestPort, machineExit)
}

func (c *Connector) connectGuestPortAt(ctx context.Context, vsockPath string, port uint32, machineExit *machineExit) (vm.Stream, error) {
	connectCtx, cancel := context.WithTimeout(ctx, c.cfg.HealthTimeout)
	defer cancel()
	if machineExit != nil {
		go func() {
			select {
			case <-machineExit.done:
				cancel()
			case <-connectCtx.Done():
			}
		}()
	}
	var lastErr error
	for {
		if err, ok := machineExit.Err(); ok {
			return nil, fmt.Errorf("the Firecracker machine exited before guest port %d connection: %w", port, err)
		}
		conn, err := dialVsock(connectCtx, vsockPath, port)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if connectCtx.Err() != nil {
			if exitErr, ok := machineExit.Err(); ok {
				return nil, fmt.Errorf("the Firecracker machine exited before guest port %d connection: %w", port, exitErr)
			}
			return nil, fmt.Errorf("guest port %d connection timed out after %s: %w", port, c.cfg.HealthTimeout, errors.Join(connectCtx.Err(), lastErr))
		}
		if err := sleepHealthRetry(connectCtx); err != nil {
			if exitErr, ok := machineExit.Err(); ok {
				return nil, fmt.Errorf("the Firecracker machine exited before guest port %d connection: %w", port, exitErr)
			}
			return nil, fmt.Errorf("guest port %d connection timed out after %s: %w", port, c.cfg.HealthTimeout, errors.Join(err, lastErr))
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
		return healthResponse{}, fmt.Errorf("guest health request: %w", err)
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
	body, err := io.ReadAll(io.LimitReader(httpResponse.Body, maxGuestHealthResponseBytes+1))
	if err != nil {
		return healthResponse{}, fmt.Errorf("read guest health response: %w", err)
	}
	if len(body) > maxGuestHealthResponseBytes {
		return healthResponse{}, fmt.Errorf("read guest health response: body exceeds %d bytes", maxGuestHealthResponseBytes)
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

type healthProbeStats struct {
	started           time.Time
	attempts          int
	dialErrors        int
	writeErrors       int
	readErrors        int
	statusErrors      int
	decodeErrors      int
	closeErrors       int
	startingResponses int
	machineExited     bool
	lastBucket        string
	lastStatus        string
	lastErr           error
}

func newHealthProbeStats() *healthProbeStats {
	return &healthProbeStats{started: time.Now()}
}

func (s *healthProbeStats) elapsed() time.Duration {
	if s == nil || s.started.IsZero() {
		return 0
	}
	return time.Since(s.started)
}

func (s *healthProbeStats) recordError(bucket string, err error) {
	if s == nil {
		return
	}
	if strings.TrimSpace(bucket) == "" {
		bucket = "unknown"
	}
	switch bucket {
	case "dial":
		s.dialErrors++
	case "write":
		s.writeErrors++
	case "read":
		s.readErrors++
	case "status":
		s.statusErrors++
	case "decode":
		s.decodeErrors++
	case "close":
		s.closeErrors++
	}
	s.lastBucket = bucket
	s.lastErr = err
}

func (s *healthProbeStats) recordStarting(response healthResponse) {
	if s == nil {
		return
	}
	s.startingResponses++
	s.lastBucket = "starting"
	s.lastStatus = response.Status
	s.lastErr = fmt.Errorf("guest health status=%q component=%q message=%q", response.Status, response.Component, response.Message)
}

func (s *healthProbeStats) recordStatus(status string) {
	if s == nil {
		return
	}
	s.statusErrors++
	s.lastBucket = "status"
	s.lastStatus = status
	s.lastErr = fmt.Errorf("guest health status=%q", status)
}

func (s *healthProbeStats) timeoutError(timeout time.Duration, err error, lastErr error, machineExit *machineExit) error {
	if s == nil {
		return fmt.Errorf("guest health probe timed out after %s: %w", timeout, errors.Join(err, lastErr))
	}
	if exitErr, ok := machineExit.Err(); ok {
		s.machineExited = true
		lastErr = errors.Join(lastErr, fmt.Errorf("the Firecracker machine exited: %w", exitErr))
	}
	return fmt.Errorf("guest health probe timed out after %s (%s): %w", timeout, s.summary(), errors.Join(err, lastErr))
}

func (s *healthProbeStats) failureError(message string, err error) error {
	if s == nil {
		if err == nil {
			return errors.New(message)
		}
		return fmt.Errorf("%s: %w", message, err)
	}
	if err == nil {
		return fmt.Errorf("%s (%s)", message, s.summary())
	}
	return fmt.Errorf("%s (%s): %w", message, s.summary(), err)
}

func (s *healthProbeStats) log(logf func(string, ...interface{}), status string) {
	if s == nil || logf == nil {
		return
	}
	logf("guest health probe %s %s", status, s.summary())
}

func (s *healthProbeStats) logFailedAttempt(logf func(string, ...interface{}), duration time.Duration) {
	if s == nil || logf == nil {
		return
	}
	lastErr := ""
	if s.lastErr != nil {
		lastErr = strings.ReplaceAll(s.lastErr.Error(), "\n", " ")
	}
	logf("guest health probe attempt %s attempt=%d duration_ms=%d bucket=%q error=%q",
		"failed",
		s.attempts,
		vm.RuntimeDurationMilliseconds(duration),
		s.lastBucket,
		lastErr,
	)
}

func (s *healthProbeStats) summary() string {
	if s == nil {
		return ""
	}
	lastErr := ""
	if s.lastErr != nil {
		lastErr = strings.ReplaceAll(s.lastErr.Error(), "\n", " ")
	}
	return fmt.Sprintf("attempts=%d elapsed_ms=%d dial_errors=%d write_errors=%d read_errors=%d status_errors=%d decode_errors=%d close_errors=%d starting_responses=%d machine_exited=%t last_bucket=%q last_status=%q last_error=%q",
		s.attempts,
		vm.RuntimeDurationMilliseconds(s.elapsed()),
		s.dialErrors,
		s.writeErrors,
		s.readErrors,
		s.statusErrors,
		s.decodeErrors,
		s.closeErrors,
		s.startingResponses,
		s.machineExited,
		s.lastBucket,
		s.lastStatus,
		lastErr,
	)
}

func healthProbeErrorBucket(err error) string {
	if err == nil {
		return ""
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "write guest health request"):
		return "write"
	case strings.Contains(message, "decode guest health response"):
		return "decode"
	case strings.Contains(message, "guest health returned http"):
		return "status"
	case strings.Contains(message, "read guest health response"):
		return "read"
	default:
		return "unknown"
	}
}

type guestSession struct {
	mu              sync.Mutex
	stream          vm.Stream
	opened          bool
	closed          bool
	machine         *firecracker.Machine
	machineCancel   context.CancelFunc
	machineExit     *machineExit
	cfg             Config
	kernelArgs      string
	runtimeIdentity runtimeid.Profile
	cpuConfigDigest string
	vsockHostPath   string
	instanceDir     string
	jailRoot        string
	scratchDisk     string
	topology        vm.RuntimeTopology
	readOnlyDrives  []vm.ReadOnlyDrive
	owner           vm.Owner
	cleaner         vm.Cleaner
	networkBinding  *installedNetworkBinding
	paused          atomic.Bool
	once            sync.Once
	machineStopOnce sync.Once
	machineStopErr  error
	networkErr      error
	err             error
}

func (s *guestSession) Stream() vm.Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream
}

func (s *guestSession) Open(ctx context.Context) (vm.Session, error) {
	if ctx == nil {
		return nil, errors.New("prepared session open context is nil")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("the Firecracker prepared session is closed")
	}
	if s.opened {
		s.mu.Unlock()
		return nil, errors.New("the Firecracker prepared session is already opened")
	}
	s.opened = true
	s.mu.Unlock()

	stream, err := (&Connector{cfg: s.cfg}).connectGuestPort(
		ctx,
		s.vsockHostPath,
		s.machineExit,
	)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.Join(
			errors.New("the Firecracker prepared session closed while opening"),
			stream.Close(),
		)
	}
	s.stream = stream
	s.mu.Unlock()
	return s, nil
}

func (s *guestSession) OpenStream(ctx context.Context) (vm.Stream, error) {
	return (&Connector{cfg: s.cfg}).connectGuestPort(ctx, s.vsockHostPath, s.machineExit)
}

func (s *guestSession) RunNetworkStatus(
	ctx context.Context,
) (vm.RunNetworkStatus, error) {
	return (&Connector{cfg: s.cfg}).readRunNetworkStatus(
		ctx,
		s.owner.ID,
	)
}

func (s *guestSession) Wait(ctx context.Context) error {
	if s.machineExit == nil {
		return errors.New("the Firecracker session exit watcher is not configured")
	}
	waitErr := s.machineExit.Wait(ctx)
	s.mu.Lock()
	networkErr := s.networkErr
	s.mu.Unlock()
	return errors.Join(waitErr, networkErr)
}

func (s *guestSession) watchNetworkFailure() {
	if s.networkBinding == nil || s.machineExit == nil {
		return
	}
	go func() {
		select {
		case failure := <-s.networkBinding.Failure():
			if failure == nil {
				failure = errors.New("network binding failed without a cause")
			}
			s.mu.Lock()
			s.networkErr = fmt.Errorf("the Firecracker datapath binding failed: %w", failure)
			s.mu.Unlock()
			stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
			defer cancel()
			_ = s.stopMachine(stopCtx)
		case <-s.machineExit.done:
		}
	}()
}

func (s *guestSession) stopMachine(ctx context.Context) error {
	s.machineStopOnce.Do(func() {
		s.machineStopErr = stopSessionMachine(ctx, s.machine, s.machineExit)
	})
	return s.machineStopErr
}

func (s *guestSession) Close(ctx context.Context) error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		stream := s.stream
		s.mu.Unlock()
		var deactivateErr error
		if s.networkBinding != nil {
			deactivateErr = s.networkBinding.Deactivate()
		}
		stopErr := s.stopMachine(ctx)
		if s.machineCancel != nil {
			s.machineCancel()
		}
		var streamErr error
		if stream != nil {
			streamErr = closeGuestStream(ctx, stream)
		}
		if errors.Is(streamErr, net.ErrClosed) || errors.Is(streamErr, os.ErrClosed) {
			streamErr = nil
		}
		var bindingCloseErr error
		if s.networkBinding != nil {
			bindingCloseErr = s.networkBinding.Close()
		}
		var cleanupErr error
		if bindingCloseErr == nil {
			if s.cleaner != nil {
				cleanupErr = s.cleaner.Cleanup(ctx, s.owner)
			} else {
				cleanupErr = cleanupUnproven(s.owner, errors.New("the Firecracker session cleaner is not configured"))
			}
		}
		s.err = errors.Join(
			streamErr,
			deactivateErr,
			stopErr,
			cleanupErr,
			bindingCloseErr,
		)
	})
	return s.err
}

func cleanupGuestSessionResources(cleanup func()) {
	if cleanup == nil {
		return
	}
	cleanup()
}

func closeGuestStream(ctx context.Context, stream io.Closer) error {
	ctx, cancel := closeContext(ctx, stopTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- stream.Close()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("close guest stream: %w", ctx.Err())
	}
}

func (s *guestSession) CreateSnapshot(ctx context.Context, request vm.SnapshotRequest) (vm.SnapshotArtifact, error) {
	checkpointID := safeSnapshotID(request.ID)
	memName := checkpointID + snapshotMemorySuffix
	stateName := checkpointID + snapshotStateSuffix
	memPath := filepath.Join(s.jailRoot, memName)
	statePath := filepath.Join(s.jailRoot, stateName)
	var phases []vm.RuntimePhase
	recordPhase := func(name string, started time.Time) {
		phases = append(phases, vm.RuntimePhase{Name: name, DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started))})
	}
	started := time.Now()
	if err := s.machine.PauseVM(ctx); err != nil {
		return vm.SnapshotArtifact{}, fmt.Errorf("pause Firecracker vm: %w", err)
	}
	recordPhase("firecracker_pause_vm", started)
	s.paused.Store(true)
	started = time.Now()
	if err := s.machine.CreateSnapshot(
		ctx,
		path.Join("/", memName),
		path.Join("/", stateName),
		explicitFullSnapshot,
	); err != nil {
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, fmt.Errorf("create Firecracker snapshot: %w", err)
	}
	recordPhase("firecracker_create_snapshot", started)
	cleanupRawSnapshot := true
	defer func() {
		if cleanupRawSnapshot {
			_ = os.Remove(memPath)
			_ = os.Remove(statePath)
		}
	}()
	runtimeIdentity := s.runtimeIdentity
	expectedRuntimeID, err := runtimeIdentity.ExpectedID()
	if err != nil {
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, err
	}
	if runtimeIdentity.ID != expectedRuntimeID {
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, errors.New("bound host runtime identity is not canonical")
	}
	workerArchitecture := runtimeIdentity.Arch
	runtimeID := runtimeIdentity.ID
	kernelDigest := runtimeIdentity.KernelDigest
	initramfsDigest := runtimeIdentity.InitramfsDigest
	rootfsDigest := runtimeIdentity.RootfsDigest
	started = time.Now()
	configDigest, manifest, err := snapshotRuntimeConfig(
		s.cfg,
		checkpointID,
		runtimeID,
		s.cpuConfigDigest,
		kernelDigest,
		initramfsDigest,
		rootfsDigest,
		s.kernelArgs,
		s.topology,
		s.readOnlyDrives,
	)
	if err != nil {
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, err
	}
	recordPhase("runtime_config_digest", started)
	var scratchFile vm.SnapshotFile
	var memoryFile vm.SnapshotFile
	var scratchPhase vm.RuntimePhase
	var memoryPhase vm.RuntimePhase
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		file, phase, err := s.packSnapshotRuntimeFile(groupCtx, s.scratchDisk, filepackScratchRole, checkpointID+snapshotScratchPackSuffix, cas.CheckpointScratchDiskMediaType)
		if err != nil {
			return fmt.Errorf("pack checkpoint scratch disk: %w", err)
		}
		scratchFile = file
		scratchPhase = phase
		return nil
	})
	group.Go(func() error {
		file, phase, err := s.packSnapshotRuntimeFile(groupCtx, memPath, filepackMemoryRole, checkpointID+snapshotMemoryPackSuffix, cas.CheckpointMemoryMediaType)
		if err != nil {
			return fmt.Errorf("pack checkpoint memory: %w", err)
		}
		memoryFile = file
		memoryPhase = phase
		return nil
	})
	if err := group.Wait(); err != nil {
		removeFiles([]string{scratchFile.Path, memoryFile.Path})
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, err
	}
	phases = append(phases, scratchPhase, memoryPhase)
	_ = os.Remove(memPath)
	cleanupRawSnapshot = false
	return vm.SnapshotArtifact{
		RuntimeBackend:      "firecracker",
		RuntimeArch:         workerArchitecture,
		VMRuntimeContract:   runtimeIdentity.Contract,
		RuntimeID:           runtimeID,
		KernelDigest:        kernelDigest,
		InitramfsDigest:     initramfsDigest,
		RootfsDigest:        rootfsDigest,
		RuntimeConfigDigest: configDigest,
		VMVCPUCount:         int32(s.cfg.VCPUCount),
		CPUConfigDigest:     s.cpuConfigDigest,
		Substrate:           cloneRuntimeSubstrate(s.topology.Substrate),
		VMState:             vm.SnapshotFile{Path: statePath, MediaType: cas.CheckpointVMStateMediaType},
		ScratchDisk:         scratchFile,
		Memory:              []vm.SnapshotFile{memoryFile},
		Manifest:            manifest,
		Phases:              phases,
	}, nil
}

func (s *guestSession) packSnapshotRuntimeFile(ctx context.Context, sourcePath string, role string, name string, mediaType string) (vm.SnapshotFile, vm.RuntimePhase, error) {
	targetPath := filepath.Join(filepath.Dir(s.scratchDisk), name)
	started := time.Now()
	stats, err := packRuntimeFile(ctx, sourcePath, targetPath, role)
	if err != nil {
		return vm.SnapshotFile{}, vm.RuntimePhase{}, err
	}
	phaseName := "pack_" + strings.ReplaceAll(role, "-", "_") + "_filepack"
	if role == filepackScratchRole {
		phaseName = "pack_scratch_filepack"
	}
	return vm.SnapshotFile{Path: targetPath, MediaType: mediaType, Filepack: &stats}, vm.RuntimePhase{
		Name:       phaseName,
		DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)),
		Role:       role,
		MediaType:  mediaType,
		Filepack:   &stats,
	}, nil
}

func (s *guestSession) Resume(ctx context.Context) error {
	if !s.paused.Load() {
		return nil
	}
	if err := s.machine.ResumeVM(ctx); err != nil {
		return fmt.Errorf("resume Firecracker vm: %w", err)
	}
	s.paused.Store(false)
	return nil
}

func recordRuntimePhase(record func(vm.RuntimePhase), phase vm.RuntimePhase) {
	if record == nil || strings.TrimSpace(phase.Name) == "" {
		return
	}
	record(phase)
}

func stopMachine(ctx context.Context, machine *firecracker.Machine) error {
	pid, pidErr := machine.PID()
	stopErr := machine.StopVMM()
	waitCtx, cancel := closeContext(ctx, stopTimeout)
	defer cancel()
	waitErr := machine.Wait(waitCtx)
	if errors.Is(waitErr, context.DeadlineExceeded) && pidErr == nil {
		if process, err := os.FindProcess(pid); err != nil {
			waitErr = errors.Join(waitErr, fmt.Errorf("find Firecracker process %d: %w", pid, err))
		} else if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
			waitErr = errors.Join(waitErr, fmt.Errorf("kill Firecracker process %d: %w", pid, err))
		} else {
			killWaitCtx, killCancel := context.WithTimeout(context.Background(), stopTimeout)
			waitErr = machine.Wait(killWaitCtx)
			killCancel()
			waitErr = ignoreStopSignalError(waitErr, syscall.SIGKILL)
		}
	}
	return errors.Join(stopErr, ignoreExpectedStopErrors(waitErr))
}

type machineExit struct {
	done chan struct{}
	err  error
}

func watchMachineExit(machine *firecracker.Machine) *machineExit {
	exit := &machineExit{done: make(chan struct{})}
	go func() {
		exit.err = machine.Wait(context.Background())
		close(exit.done)
	}()
	return exit
}

func (e *machineExit) Wait(ctx context.Context) error {
	if e == nil {
		return errors.New("the Firecracker machine exit watcher is not configured")
	}
	select {
	case <-e.done:
		return e.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *machineExit) Err() (error, bool) {
	if e == nil {
		return nil, false
	}
	select {
	case <-e.done:
		return e.err, true
	default:
		return nil, false
	}
}

func stopSessionMachine(ctx context.Context, machine *firecracker.Machine, exit *machineExit) error {
	pid, pidErr := machine.PID()
	stopErr := machine.StopVMM()
	waitCtx, cancel := closeContext(ctx, stopTimeout)
	defer cancel()
	waitErr := exit.Wait(waitCtx)
	if errors.Is(waitErr, context.DeadlineExceeded) && pidErr == nil {
		if process, err := os.FindProcess(pid); err != nil {
			waitErr = errors.Join(waitErr, fmt.Errorf("find Firecracker process %d: %w", pid, err))
		} else if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
			waitErr = errors.Join(waitErr, fmt.Errorf("kill Firecracker process %d: %w", pid, err))
		} else {
			killWaitCtx, killCancel := context.WithTimeout(context.Background(), stopTimeout)
			waitErr = exit.Wait(killWaitCtx)
			killCancel()
			waitErr = ignoreStopSignalError(waitErr, syscall.SIGKILL)
		}
	}
	return errors.Join(stopErr, ignoreExpectedStopErrors(waitErr))
}

func closeContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
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
	if ignoreStopSignalError(err, syscall.SIGTERM) == nil {
		return nil
	}
	return err
}

func ignoreStopSignalError(err error, signal syscall.Signal) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil {
		if status, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() && status.Signal() == signal {
			return nil
		}
	}
	return err
}

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
	Backend          string                    `json:"backend"`
	DescriptorDigest string                    `json:"runtime_descriptor_digest"`
	ID               string                    `json:"id"`
	Arch             string                    `json:"arch"`
	Contract         string                    `json:"contract"`
	VCPUCount        int64                     `json:"vcpu_count"`
	CPUConfigDigest  string                    `json:"cpu_config_digest"`
	MemoryMiB        int64                     `json:"memory_mib"`
	ScratchDiskMiB   int64                     `json:"scratch_disk_mib"`
	KernelArgs       string                    `json:"kernel_args"`
	KernelDigest     string                    `json:"kernel_digest"`
	InitramfsDigest  string                    `json:"initramfs_digest"`
	RootfsDigest     string                    `json:"rootfs_digest"`
	Substrate        *snapshotRuntimeSubstrate `json:"substrate,omitempty"`
	Program          *snapshotProgramManifest  `json:"program,omitempty"`
	GuestPort        uint32                    `json:"guest_port"`
	HealthPort       uint32                    `json:"health_port"`
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

type snapshotRuntimeSubstrate struct {
	Digest    string `json:"digest"`
	Format    string `json:"format"`
	Contract  string `json:"contract"`
	SizeBytes int64  `json:"size_bytes"`
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

func snapshotSubstrateManifest(substrate *vm.RuntimeSubstrate) (*snapshotRuntimeSubstrate, error) {
	if substrate == nil {
		return nil, nil
	}
	if err := validateRuntimeSubstrateTopology(substrate); err != nil {
		return nil, err
	}
	return &snapshotRuntimeSubstrate{
		Digest:    strings.TrimSpace(substrate.Digest),
		Format:    strings.TrimSpace(substrate.Format),
		Contract:  strings.TrimSpace(substrate.Contract),
		SizeBytes: substrate.SizeBytes,
	}, nil
}

func cloneRuntimeSubstrate(substrate *vm.RuntimeSubstrate) *vm.RuntimeSubstrate {
	if substrate == nil {
		return nil
	}
	clone := *substrate
	return &clone
}

func validateRuntimeSubstrateTopology(substrate *vm.RuntimeSubstrate) error {
	if substrate == nil {
		return nil
	}
	if strings.TrimSpace(substrate.Path) == "" {
		return errors.New("runtime substrate path is required")
	}
	return validateRuntimeSubstrateIdentity(substrate)
}

func validateRuntimeSubstrateIdentity(substrate *vm.RuntimeSubstrate) error {
	if substrate == nil {
		return nil
	}
	if strings.TrimSpace(substrate.Digest) == "" {
		return errors.New("runtime substrate digest is required")
	}
	if strings.TrimSpace(substrate.Format) != "ext4" {
		return fmt.Errorf("runtime substrate format %q is not supported", substrate.Format)
	}
	if strings.TrimSpace(substrate.Contract) == "" {
		return errors.New("runtime substrate contract is required")
	}
	if substrate.SizeBytes <= 0 {
		return errors.New("runtime substrate size must be positive")
	}
	return nil
}

func validateRuntimeSubstrateSource(substrate *vm.RuntimeSubstrate) error {
	if substrate == nil {
		return nil
	}
	if strings.TrimSpace(substrate.Path) != "" {
		return errors.New("runtime substrate cache path must not cross the connector boundary")
	}
	if substrate.Source == nil {
		return errors.New("runtime substrate materialization source is required")
	}
	return validateRuntimeSubstrateIdentity(substrate)
}

func snapshotRuntimeConfig(cfg Config, checkpointID string, runtimeID string, cpuConfigDigest string, kernelDigest string, initramfsDigest string, rootfsDigest string, kernelArgs string, topology vm.RuntimeTopology, readOnlyDrives ...[]vm.ReadOnlyDrive) (string, []byte, error) {
	if !sha256sum.ValidDigest(runtimeID) {
		return "", nil, errors.New("canonical bound host runtime ID is required for checkpoint restore")
	}
	workerArchitecture, err := runtimeid.ArchitectureFromGo(runtime.GOARCH)
	if err != nil {
		return "", nil, err
	}
	network := snapshotNetworkConfig(cfg)
	if err := validateSnapshotNetwork(network); err != nil {
		return "", nil, fmt.Errorf("build checkpoint network manifest: %w", err)
	}
	substrate, err := snapshotSubstrateManifest(topology.Substrate)
	if err != nil {
		return "", nil, err
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
				Contract:         runtimeid.Contract,
				VCPUCount:        cfg.VCPUCount,
				CPUConfigDigest:  cpuConfigDigest,
				MemoryMiB:        cfg.MemoryMiB,
				ScratchDiskMiB:   cfg.ScratchDiskMiB,
				KernelArgs:       kernelArgs,
				KernelDigest:     kernelDigest,
				InitramfsDigest:  initramfsDigest,
				RootfsDigest:     rootfsDigest,
				Substrate:        substrate,
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
	expectedSubstrate *vm.RuntimeSubstrate,
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
	workerArchitecture, err := runtimeid.ArchitectureFromGo(runtime.GOARCH)
	if err != nil {
		return err
	}
	if runtimeManifest.Arch != workerArchitecture {
		return fmt.Errorf("checkpoint manifest runtime arch %q does not match worker arch %q", runtimeManifest.Arch, workerArchitecture)
	}
	if runtimeManifest.Contract != runtimeid.Contract {
		return fmt.Errorf("checkpoint manifest runtime contract %q does not match worker contract %q", runtimeManifest.Contract, runtimeid.Contract)
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
	if err := validateRuntimeSubstrateManifest(runtimeManifest.Substrate, expectedSubstrate); err != nil {
		return err
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

func validateRuntimeSubstrateManifest(manifest *snapshotRuntimeSubstrate, expected *vm.RuntimeSubstrate) error {
	switch {
	case manifest == nil && expected == nil:
		return nil
	case manifest == nil:
		return errors.New("checkpoint manifest has no runtime substrate but restore request provided one")
	case expected == nil:
		return errors.New("checkpoint manifest requires runtime substrate but restore request did not provide one")
	}
	if err := validateRuntimeSubstrateIdentity(expected); err != nil {
		return err
	}
	if manifest.Digest != strings.TrimSpace(expected.Digest) {
		return fmt.Errorf("checkpoint manifest substrate digest %s does not match restore substrate digest %s", manifest.Digest, expected.Digest)
	}
	if manifest.Format != strings.TrimSpace(expected.Format) {
		return fmt.Errorf("checkpoint manifest substrate format %s does not match restore substrate format %s", manifest.Format, expected.Format)
	}
	if manifest.Contract != strings.TrimSpace(expected.Contract) {
		return fmt.Errorf("checkpoint manifest substrate contract %s does not match restore substrate contract %s", manifest.Contract, expected.Contract)
	}
	if manifest.SizeBytes != expected.SizeBytes {
		return fmt.Errorf("checkpoint manifest substrate size %d does not match restore substrate size %d", manifest.SizeBytes, expected.SizeBytes)
	}
	return nil
}

func allocateGuestCID() uint32 {
	return CanonicalVMRuntimeDescriptor().Devices.Vsock.GuestCIDStart - 1 + nextGuestCID.Add(1)
}

func jailRootPath(cfg Config, id string) string {
	return filepath.Join(cfg.JailerChrootBaseDir, filepath.Base(cfg.FirecrackerPath), id, "root")
}

func withSnapshotRestore(memoryPath string, statePath string) firecracker.Opt {
	return func(machine *firecracker.Machine) {
		firecracker.WithSnapshot(memoryPath, statePath, func(config *firecracker.SnapshotConfig) {
			config.EnableDiffSnapshots = CanonicalVMRuntimeDescriptor().Snapshot.LoadEnableDiffSnapshots
			config.ResumeVM = CanonicalVMRuntimeDescriptor().Snapshot.LoadResumeVM
		})(machine)
		machine.Handlers.FcInit = machine.Handlers.FcInit.Remove(firecracker.AddVsocksHandlerName)
	}
}

func explicitFullSnapshot(parameters *operations.CreateSnapshotParams) {
	if parameters.Body != nil {
		parameters.Body.SnapshotType = CanonicalVMRuntimeDescriptor().Snapshot.CreateType
	}
}

type sealedDriveChrootStrategy struct {
	kernelImagePath string
	drives          []vm.ReadOnlyDrive
}

func (strategy sealedDriveChrootStrategy) AdaptHandlers(
	handlers *firecracker.Handlers,
) error {
	if !handlers.FcInit.Has(firecracker.CreateLogFilesHandlerName) {
		return firecracker.ErrRequiredHandlerMissing
	}
	base := firecracker.LinkFilesHandler(filepath.Base(strategy.kernelImagePath))
	base.Fn = strategy.linkFiles(base.Fn)
	handlers.FcInit = handlers.FcInit.AppendAfter(
		firecracker.CreateLogFilesHandlerName,
		base,
	)
	return nil
}

func withRestoreSealedDrives(strategy sealedDriveChrootStrategy) firecracker.Opt {
	return func(machine *firecracker.Machine) {
		// firecracker.WithSnapshot replaces FcInit after the jailer chroot
		// strategy has adapted it. Restore the sealed-drive handler after the
		// snapshot and ordinary restore-file options have established their
		// handler list. AppendAfter places this handler before the ordinary
		// restore-file handler, so its SDK link step still sees absolute paths.
		base := firecracker.LinkFilesHandler(filepath.Base(strategy.kernelImagePath))
		base.Fn = strategy.linkFiles(base.Fn)
		machine.Handlers.FcInit = machine.Handlers.FcInit.AppendAfter(
			firecracker.CreateLogFilesHandlerName,
			base,
		)
	}
}

func (strategy sealedDriveChrootStrategy) linkFiles(
	linkOrdinary func(context.Context, *firecracker.Machine) error,
) func(context.Context, *firecracker.Machine) error {
	return func(ctx context.Context, machine *firecracker.Machine) error {
		sources := make(map[string]vm.ReadOnlyDriveSource, len(strategy.drives))
		for _, drive := range strategy.drives {
			sources[drive.ID] = drive.Source
		}
		ordinary := make([]models.Drive, 0, len(machine.Cfg.Drives))
		sealed := make(map[string]models.Drive, len(strategy.drives))
		for _, drive := range machine.Cfg.Drives {
			id := firecracker.StringValue(drive.DriveID)
			if _, exists := sources[id]; exists {
				sealed[id] = drive
				continue
			}
			ordinary = append(ordinary, drive)
		}
		machine.Cfg.Drives = ordinary
		if err := linkOrdinary(ctx, machine); err != nil {
			return err
		}

		root := jailRootPath(Config{
			FirecrackerPath:     machine.Cfg.JailerCfg.ExecFile,
			JailerChrootBaseDir: machine.Cfg.JailerCfg.ChrootBaseDir,
		}, machine.Cfg.JailerCfg.ID)
		for _, id := range readOnlyDriveOrder {
			source, exists := sources[id]
			if !exists {
				continue
			}
			drive, exists := sealed[id]
			if !exists {
				return fmt.Errorf("sealed drive %q is absent from machine config", id)
			}
			name := readOnlyDriveName(id)
			if err := source.LinkInto(
				root,
				name,
				*machine.Cfg.JailerCfg.UID,
				*machine.Cfg.JailerCfg.GID,
			); err != nil {
				return fmt.Errorf("link sealed drive %q into jail: %w", id, err)
			}
			drive.PathOnHost = firecracker.String(name)
			machine.Cfg.Drives = append(machine.Cfg.Drives, drive)
		}
		return nil
	}
}

func withJailedRestoreFiles(rootfsPath string, scratchDiskPath string, substrateDiskPath string, memoryPath string, statePath string) firecracker.Opt {
	return func(machine *firecracker.Machine) {
		machine.Handlers.Validation = machine.Handlers.Validation.Append(firecracker.JailerConfigValidationHandler)
		machine.Handlers.FcInit = machine.Handlers.FcInit.AppendAfter(firecracker.CreateLogFilesHandlerName, firecracker.Handler{
			Name: "fcinit.LinkHelmrRestoreFilesToRootFS",
			Fn: func(ctx context.Context, machine *firecracker.Machine) error {
				root := jailRootPath(Config{
					FirecrackerPath:     machine.Cfg.JailerCfg.ExecFile,
					JailerChrootBaseDir: machine.Cfg.JailerCfg.ChrootBaseDir,
				}, machine.Cfg.JailerCfg.ID)
				if err := linkIntoJail(rootfsPath, root, filepath.Base(rootfsPath)); err != nil {
					return fmt.Errorf("link rootfs into jail: %w", err)
				}
				for i := range machine.Cfg.Drives {
					if firecracker.StringValue(machine.Cfg.Drives[i].PathOnHost) == rootfsPath {
						machine.Cfg.Drives[i].PathOnHost = firecracker.String(filepath.Base(rootfsPath))
					}
				}
				if err := linkIntoJailForVMM(scratchDiskPath, root, scratchDiskName, *machine.Cfg.JailerCfg.UID, *machine.Cfg.JailerCfg.GID); err != nil {
					return fmt.Errorf("link scratch disk into jail: %w", err)
				}
				for i := range machine.Cfg.Drives {
					if firecracker.StringValue(machine.Cfg.Drives[i].PathOnHost) == scratchDiskPath {
						machine.Cfg.Drives[i].PathOnHost = firecracker.String(scratchDiskName)
					}
				}
				if strings.TrimSpace(substrateDiskPath) != "" {
					substrateName := filepath.Base(substrateDiskPath)
					if err := linkIntoJailForVMM(substrateDiskPath, root, substrateName, *machine.Cfg.JailerCfg.UID, *machine.Cfg.JailerCfg.GID); err != nil {
						return fmt.Errorf("link substrate disk into jail: %w", err)
					}
					for i := range machine.Cfg.Drives {
						if firecracker.StringValue(machine.Cfg.Drives[i].PathOnHost) == substrateDiskPath {
							machine.Cfg.Drives[i].PathOnHost = firecracker.String(substrateName)
						}
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
