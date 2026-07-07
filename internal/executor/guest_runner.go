package executor

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
)

var (
	restoreAttachTimeout     = 30 * time.Second
	checkpointSuspendTimeout = 5 * time.Minute
)

const (
	waitMetadataJSONMaxBytes = 64 * 1024
	waitTagsMaxCount         = 32
	waitTagMaxBytes          = 128
)

type GuestRunner struct {
	Connector             vm.Connector
	CAS                   cas.Store
	CheckpointEncryptor   *checkpoint.Encryptor
	WorkspaceMounts       WorkspaceMountSessionRegistry
	Events                RuntimeEventSink
	TempDir               string
	ArtifactCacheDir      string
	ArtifactCacheMaxBytes int64
	Substrates            RuntimeSubstrateResolver
	RuntimeSubstrates     RuntimeSubstrateRegistrar
	Log                   *slog.Logger
	Stdout                io.Writer
	Stderr                io.Writer
}

type RuntimeEventSink interface {
	AppendLog(context.Context, api.WorkerRunLease, api.WorkerLogStream, uint64, []byte) (api.WorkerEventResponse, error)
	RecordLogEntry(context.Context, api.WorkerRunLease, string) (api.WorkerEventResponse, error)
	AppendOutputStream(context.Context, api.WorkerOutputStreamAppendRequest) (api.AppendStreamRecordResponse, error)
	ReadInputStream(context.Context, api.WorkerActiveStreamReadRequest) (api.WorkerActiveStreamReadResponse, error)
	UpdateRunMetadata(context.Context, api.WorkerUpdateRunMetadataRequest) (api.WorkerEventResponse, error)
	CreateRuntimeToken(context.Context, api.WorkerCreateTokenRequest) (api.TokenResponse, error)
}

func (r GuestRunner) Run(ctx context.Context, request Request) (Result, error) {
	if r.Connector == nil {
		return Result{}, errors.New("guest connector is required")
	}
	if request.Leases == nil {
		return Result{}, errors.New("worker run lease provider is required")
	}
	if request.Run.Restore != nil {
		return r.restore(ctx, request)
	}
	if strings.TrimSpace(request.DeploymentSource.ProjectRoot) == "" {
		return Result{}, errors.New("checked-out deployment source project root is required")
	}
	deploymentSourceRoot, err := runtimeSourceRoot(request.DeploymentSource)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(request.Workspace.Digest) == "" {
		return Result{}, errors.New("workspace artifact digest is required")
	}
	if workspaceMountID := strings.TrimSpace(request.Run.Workspace.WorkspaceMountID); workspaceMountID != "" {
		if r.WorkspaceMounts == nil {
			return Result{}, errors.New("workspace mount session registry is required")
		}
		phaseStarted := time.Now()
		workspaceMountSession, err := r.WorkspaceMounts.OpenWorkspaceMountSession(ctx, workspaceMountID)
		r.logRunPhase(request, "guest run workspace mount stream opened", "duration_ms", time.Since(phaseStarted).Milliseconds(), "workspace_mount_id", workspaceMountID, "error", errorString(err))
		if err != nil {
			return Result{}, err
		}
		session := workspaceMountSession.Session
		defer session.Close(context.Background())
		stream := session.Stream()
		phaseStarted = time.Now()
		inputMetadata, err := r.writeWorkspaceRunInput(ctx, stream, request, deploymentSourceRoot, workspaceMountSession.ChannelToken)
		r.logRunPhase(request, "guest run input written", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorString(err))
		if err != nil {
			return Result{}, err
		}
		phaseStarted = time.Now()
		result, err := r.readRunEvents(ctx, session, request, inputMetadata)
		r.logRunPhase(request, "guest run events read", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorString(err))
		return result, err
	}
	return Result{}, errors.New("workspace mount id is required for worker run execution")
}

func (r GuestRunner) runDirect(ctx context.Context, request Request) (Result, error) {
	if strings.TrimSpace(request.DeploymentSource.ProjectRoot) == "" {
		return Result{}, errors.New("checked-out deployment source project root is required")
	}
	deploymentSourceRoot, err := runtimeSourceRoot(request.DeploymentSource)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(request.Artifact.ImageTarPath) == "" {
		return Result{}, errors.New("runtime image artifact is required")
	}
	if strings.TrimSpace(request.Workspace.Path) == "" {
		return Result{}, errors.New("workspace artifact path is required")
	}
	phaseStarted := time.Now()
	session, err := r.Connector.Connect(ctx, vm.ConnectRequest{Network: request.Run.Requirements.Network})
	r.logRunPhase(request, "guest run connector opened", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorString(err))
	if err != nil {
		return Result{}, fmt.Errorf("connect guest runtime: %w", err)
	}
	defer session.Close(context.Background())
	stream := session.Stream()
	phaseStarted = time.Now()
	inputMetadata, err := r.writeRuntimeInput(ctx, stream, request, deploymentSourceRoot)
	r.logRunPhase(request, "guest run input written", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorString(err))
	if err != nil {
		return Result{}, err
	}
	phaseStarted = time.Now()
	result, err := r.readRunEvents(ctx, session, request, inputMetadata)
	r.logRunPhase(request, "guest run events read", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorString(err))
	return result, err
}

func (r GuestRunner) restore(ctx context.Context, request Request) (Result, error) {
	if request.Leases == nil {
		return Result{}, errors.New("worker run lease provider is required")
	}
	restoring, ok := r.Connector.(vm.RestoringConnector)
	if !ok {
		return Result{}, errors.New("guest connector does not support checkpoint restore")
	}
	if r.CAS == nil {
		return Result{}, errors.New("checkpoint CAS is required")
	}
	restore := request.Run.Restore
	if strings.TrimSpace(restore.CheckpointID) == "" {
		return Result{}, errors.New("restore checkpoint_id is required")
	}
	if strings.TrimSpace(restore.RunWait.ID) == "" {
		return Result{}, errors.New("restore run wait id is required")
	}
	if err := validateRestoreIdentity(restore.Checkpoint); err != nil {
		return Result{}, err
	}
	runtimeState := restore.Checkpoint.RuntimeState
	if err := requireCheckpointArtifact(runtimeState.VMStateArtifact, "runtime_state.vm_state_artifact"); err != nil {
		return Result{}, err
	}
	if err := requireCheckpointArtifact(runtimeState.ScratchDiskArtifact, "runtime_state.scratch_disk_artifact"); err != nil {
		return Result{}, err
	}
	if len(runtimeState.MemoryArtifacts) != 1 {
		return Result{}, fmt.Errorf("restore checkpoint requires exactly one memory artifact, got %d", len(runtimeState.MemoryArtifacts))
	}
	if err := requireCheckpointArtifact(runtimeState.MemoryArtifacts[0], "runtime_state.memory_artifacts[0]"); err != nil {
		return Result{}, err
	}
	restorePhases := &runtimePhaseCollector{}
	topology, cleanupTopology, err := r.restoreRuntimeTopology(ctx, request, restorePhases)
	if err != nil {
		r.logCheckpointRestorePhases(request, restorePhases.Snapshot(), "failed", vm.RuntimeErrorClass(err))
		return Result{}, err
	}
	defer cleanupTopology()
	configArtifact := runtimeState.ConfigArtifact
	stateArtifact := runtimeState.VMStateArtifact
	scratchArtifact := runtimeState.ScratchDiskArtifact
	memoryArtifact := runtimeState.MemoryArtifacts[0]
	var manifestPath string
	var state string
	var scratchDisk string
	var memory string
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	group.Go(func() error {
		path, err := r.materializeCheckpointObjectPhase(groupCtx, restorePhases, configArtifact.Digest, "manifest", "restore_materialize_manifest", "manifest", configArtifact.MediaType)
		if err != nil {
			return err
		}
		manifestPath = path
		return nil
	})
	group.Go(func() error {
		path, err := r.materializeCheckpointObjectPhase(groupCtx, restorePhases, stateArtifact.Digest, "vmstate", "restore_materialize_vm_state", "vmstate", stateArtifact.MediaType)
		if err != nil {
			return err
		}
		state = path
		return nil
	})
	group.Go(func() error {
		path, err := r.materializeCheckpointObjectPhase(groupCtx, restorePhases, scratchArtifact.Digest, "scratch-disk", "restore_materialize_scratch_filepack", "scratch-disk", scratchArtifact.MediaType)
		if err != nil {
			return err
		}
		scratchDisk = path
		return nil
	})
	group.Go(func() error {
		path, err := r.materializeCheckpointObjectPhase(groupCtx, restorePhases, memoryArtifact.Digest, "memory", "restore_materialize_memory_filepack", "memory", memoryArtifact.MediaType)
		if err != nil {
			return err
		}
		memory = path
		return nil
	})
	if err := group.Wait(); err != nil {
		removeFiles([]string{manifestPath, state, scratchDisk, memory})
		r.logCheckpointRestorePhases(request, restorePhases.Snapshot(), "failed", vm.RuntimeErrorClass(err))
		return Result{}, err
	}
	defer removeFiles([]string{manifestPath, state, scratchDisk, memory})
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		restorePhases.Record(vm.RuntimePhase{Name: "restore_read_manifest", ErrorClass: vm.RuntimeErrorClass(err)})
		r.logCheckpointRestorePhases(request, restorePhases.Snapshot(), "failed", vm.RuntimeErrorClass(err))
		return Result{}, fmt.Errorf("read checkpoint manifest: %w", err)
	}
	runtimeInfo := restore.Checkpoint.RecoveryPoint.Runtime
	session, err := restoring.Restore(ctx, vm.RestoreRequest{
		ID:                   restore.CheckpointID,
		VMState:              state,
		VMStateMediaType:     stateArtifact.MediaType,
		ScratchDisk:          scratchDisk,
		ScratchDiskMediaType: scratchArtifact.MediaType,
		Memory:               []string{memory},
		MemoryMediaTypes:     []string{memoryArtifact.MediaType},
		Manifest:             manifest,
		Checkpoint: vm.CheckpointIdentity{
			RuntimeBackend:      runtimeInfo.Backend,
			RuntimeID:           runtimeInfo.ID,
			RuntimeArch:         runtimeInfo.Arch,
			RuntimeABI:          runtimeInfo.ABI,
			KernelDigest:        runtimeInfo.KernelDigest,
			InitramfsDigest:     runtimeInfo.InitramfsDigest,
			RootfsDigest:        runtimeInfo.RootfsDigest,
			RuntimeConfigDigest: runtimeInfo.ConfigDigest,
		},
		Network:     request.Run.Requirements.Network,
		Topology:    topology,
		RecordPhase: restorePhases.Record,
	})
	if err != nil {
		r.logCheckpointRestorePhases(request, restorePhases.Snapshot(), "failed", vm.RuntimeErrorClass(err))
		return Result{}, fmt.Errorf("restore guest runtime: %w", err)
	}
	defer session.Close(context.Background())
	if err := r.attachAndAcknowledgeRestore(ctx, session, request, restorePhases); err != nil {
		r.logCheckpointRestorePhases(request, restorePhases.Snapshot(), "failed", vm.RuntimeErrorClass(err))
		return Result{}, err
	}
	r.logCheckpointRestorePhases(request, restorePhases.Snapshot(), "restored", "")
	return r.readRunEvents(ctx, session, request, runtimeInputMetadata{workspaceBase: restore.Checkpoint.WorkspaceState.Base})
}

func (r GuestRunner) restoreRuntimeTopology(ctx context.Context, request Request, phases *runtimePhaseCollector) (vm.RuntimeTopology, func(), error) {
	cleanup := func() {}
	restore := request.Run.Restore
	if restore == nil || restore.Checkpoint.RecoveryPoint.Runtime.Substrate == nil {
		return vm.RuntimeTopology{}, cleanup, nil
	}
	substrateInfo := restore.Checkpoint.RecoveryPoint.Runtime.Substrate
	substrateArtifact := restore.Checkpoint.RuntimeState.RuntimeSubstrate
	if substrateArtifact == nil {
		return vm.RuntimeTopology{}, cleanup, errors.New("checkpoint runtime substrate is required for restore")
	}
	if strings.TrimSpace(substrateArtifact.Artifact.MediaType) != cas.RuntimeSubstrateMediaType {
		return vm.RuntimeTopology{}, cleanup, fmt.Errorf("checkpoint runtime substrate media type %q is not supported", substrateArtifact.Artifact.MediaType)
	}
	if lookup, ok := r.Substrates.(RuntimeSubstrateDigestLookup); ok {
		started := time.Now()
		result, err := lookup.LookupDigest(ctx, substrateInfo.Digest)
		if err == nil {
			phases.Record(vm.RuntimePhase{
				Name:       "restore_lookup_substrate_cache_hit",
				DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)),
				Role:       "substrate",
				MediaType:  substrateArtifact.Artifact.MediaType,
			})
			started = time.Now()
			topology, cleanup, stageErr := linkCheckpointRuntimeSubstrate(result.Path, substrateInfo, r.tempDir())
			phases.Record(vm.RuntimePhase{
				Name:       "restore_materialize_substrate_cache",
				DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)),
				Role:       "substrate",
				MediaType:  substrateArtifact.Artifact.MediaType,
				ErrorClass: vm.RuntimeErrorClass(stageErr),
			})
			if stageErr != nil {
				cleanup()
			} else {
				return topology, cleanup, nil
			}
		} else {
			phaseName := "restore_lookup_substrate_cache_miss"
			errorClass := ""
			if !errors.Is(err, os.ErrNotExist) {
				phaseName = "restore_lookup_substrate_cache_error"
				errorClass = vm.RuntimeErrorClass(err)
			}
			phases.Record(vm.RuntimePhase{
				Name:       phaseName,
				DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)),
				Role:       "substrate",
				MediaType:  substrateArtifact.Artifact.MediaType,
				ErrorClass: errorClass,
			})
		}
	}
	started := time.Now()
	path, err := r.materializeEncryptedObject(ctx, substrateArtifact.Artifact.Digest, "substrate", runtimeSubstratePurpose(substrateInfo.Digest))
	phases.Record(vm.RuntimePhase{
		Name:       "restore_materialize_substrate_cas",
		DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)),
		Role:       "substrate",
		MediaType:  substrateArtifact.Artifact.MediaType,
		ErrorClass: vm.RuntimeErrorClass(err),
	})
	if err != nil {
		return vm.RuntimeTopology{}, cleanup, err
	}
	cleanup = func() { removeFiles([]string{path}) }
	actualDigest, err := checkpointFileDigest(path)
	if err != nil {
		cleanup()
		return vm.RuntimeTopology{}, func() {}, fmt.Errorf("digest checkpoint runtime substrate: %w", err)
	}
	if actualDigest != strings.TrimSpace(substrateInfo.Digest) {
		cleanup()
		return vm.RuntimeTopology{}, func() {}, fmt.Errorf("checkpoint runtime substrate digest mismatch: manifest %s, artifact %s", substrateInfo.Digest, actualDigest)
	}
	topology, stagedCleanup, err := renameCheckpointRuntimeSubstrate(path, substrateInfo, r.tempDir())
	if err != nil {
		cleanup()
		return vm.RuntimeTopology{}, func() {}, err
	}
	cleanup = stagedCleanup
	return topology, cleanup, nil
}

func linkCheckpointRuntimeSubstrate(sourcePath string, substrateInfo *api.WorkerCheckpointRuntimeSubstrate, tempDir string) (vm.RuntimeTopology, func(), error) {
	actualDigest := strings.TrimSpace(substrateInfo.Digest)
	substrateDir, substratePath, err := checkpointSubstrateJailPath(tempDir, actualDigest)
	if err != nil {
		return vm.RuntimeTopology{}, func() {}, err
	}
	if err := os.Link(sourcePath, substratePath); err != nil {
		_ = os.RemoveAll(substrateDir)
		return vm.RuntimeTopology{}, func() {}, fmt.Errorf("stage checkpoint runtime substrate: %w", err)
	}
	return vm.RuntimeTopology{Substrate: &vm.RuntimeSubstrate{
		Path:       substratePath,
		Digest:     strings.TrimSpace(substrateInfo.Digest),
		Format:     strings.TrimSpace(substrateInfo.Format),
		BuilderABI: strings.TrimSpace(substrateInfo.BuilderABI),
		LayoutABI:  strings.TrimSpace(substrateInfo.LayoutABI),
	}}, func() { _ = os.RemoveAll(substrateDir) }, nil
}

func renameCheckpointRuntimeSubstrate(sourcePath string, substrateInfo *api.WorkerCheckpointRuntimeSubstrate, tempDir string) (vm.RuntimeTopology, func(), error) {
	actualDigest := strings.TrimSpace(substrateInfo.Digest)
	substrateDir, substratePath, err := checkpointSubstrateJailPath(tempDir, actualDigest)
	if err != nil {
		return vm.RuntimeTopology{}, func() {}, err
	}
	if err := os.Rename(sourcePath, substratePath); err != nil {
		_ = os.RemoveAll(substrateDir)
		return vm.RuntimeTopology{}, func() {}, fmt.Errorf("stage checkpoint runtime substrate: %w", err)
	}
	return vm.RuntimeTopology{Substrate: &vm.RuntimeSubstrate{
		Path:       substratePath,
		Digest:     strings.TrimSpace(substrateInfo.Digest),
		Format:     strings.TrimSpace(substrateInfo.Format),
		BuilderABI: strings.TrimSpace(substrateInfo.BuilderABI),
		LayoutABI:  strings.TrimSpace(substrateInfo.LayoutABI),
	}}, func() { _ = os.RemoveAll(substrateDir) }, nil
}

func checkpointSubstrateJailPath(tempDir string, digest string) (string, string, error) {
	hexDigest, ok := strings.CutPrefix(strings.TrimSpace(digest), "sha256:")
	if !ok || len(hexDigest) != 64 {
		return "", "", fmt.Errorf("checkpoint runtime substrate digest %q is not a sha256 digest", digest)
	}
	for _, r := range hexDigest {
		if !(r >= 'a' && r <= 'f' || r >= '0' && r <= '9') {
			return "", "", fmt.Errorf("checkpoint runtime substrate digest %q is not lowercase sha256", digest)
		}
	}
	dir, err := os.MkdirTemp(tempDir, "checkpoint-substrate-*")
	if err != nil {
		return "", "", fmt.Errorf("create checkpoint substrate temp dir: %w", err)
	}
	return dir, filepath.Join(dir, hexDigest+".ext4"), nil
}

func (r GuestRunner) materializeCheckpointObjectPhase(ctx context.Context, phases *runtimePhaseCollector, digest string, suffix string, phaseName string, role string, mediaType string) (string, error) {
	started := time.Now()
	path, err := r.materializeCheckpointObject(ctx, digest, suffix)
	phase := vm.RuntimePhase{
		Name:       phaseName,
		DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)),
		Role:       role,
		MediaType:  mediaType,
	}
	if err != nil {
		phase.ErrorClass = vm.RuntimeErrorClass(err)
	}
	phases.Record(phase)
	return path, err
}

type runtimePhaseCollector struct {
	mu     sync.Mutex
	phases []vm.RuntimePhase
}

func (c *runtimePhaseCollector) Record(phase vm.RuntimePhase) {
	if c == nil || strings.TrimSpace(phase.Name) == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.phases = append(c.phases, phase)
}

func (c *runtimePhaseCollector) Snapshot() []api.WorkerCheckpointPhase {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]api.WorkerCheckpointPhase, 0, len(c.phases))
	for _, phase := range c.phases {
		result = append(result, workerCheckpointPhase(phase))
	}
	return result
}

func (r GuestRunner) logCheckpointRestorePhases(request Request, phases []api.WorkerCheckpointPhase, status string, errorClass string) {
	if len(phases) == 0 && strings.TrimSpace(errorClass) == "" {
		return
	}
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	restore := request.Run.Restore
	checkpointID := ""
	if restore != nil {
		checkpointID = restore.CheckpointID
	}
	attrs := []any{
		"run_id", request.Run.RunID,
		"checkpoint_id", checkpointID,
		"status", status,
		"phases", phases,
	}
	if strings.TrimSpace(errorClass) != "" {
		attrs = append(attrs, "error_class", errorClass)
	}
	log.Info("checkpoint restore telemetry", attrs...)
}

func (r GuestRunner) tempDir() string {
	if strings.TrimSpace(r.TempDir) != "" {
		return r.TempDir
	}
	return os.TempDir()
}

func (r GuestRunner) attachAndAcknowledgeRestore(ctx context.Context, session vm.Session, request Request, phases *runtimePhaseCollector) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	acknowledger, ok := request.WaitHandler.(RestoreAcknowledger)
	if !ok {
		return errors.New("restore acknowledger is required")
	}
	stream := session.Stream()
	restore := request.Run.Restore
	started := time.Now()
	if err := frameio.WriteProtoFrame(stream, &runv0.ResumeAttach{
		CheckpointId: restore.CheckpointID,
		RunWaitId:    restore.RunWait.ID,
		RunLeaseId:   currentRunLease(request).ID,
	}); err != nil {
		phases.Record(vm.RuntimePhase{Name: "restore_attach_guest_resume", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)), ErrorClass: vm.RuntimeErrorClass(err)})
		return fmt.Errorf("write resume attach: %w", err)
	}
	if err := frameio.WriteProtoFrame(stream, &runv0.ResumeDecision{
		RunWaitId:          restore.RunWait.ID,
		Kind:               restore.RunWait.ResumeKind,
		DataJson:           string(restore.RunWait.ResumePayloadJSON),
		RequireConsumedAck: true,
	}); err != nil {
		phases.Record(vm.RuntimePhase{Name: "restore_attach_guest_resume", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)), ErrorClass: vm.RuntimeErrorClass(err)})
		return fmt.Errorf("write resume decision: %w", err)
	}
	ackCtx, cancelAck := context.WithTimeout(ctx, restoreAttachTimeout)
	ack, err := readResumeAck(ackCtx, session)
	cancelAck()
	if err != nil {
		phases.Record(vm.RuntimePhase{Name: "restore_attach_guest_resume", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)), ErrorClass: vm.RuntimeErrorClass(err)})
		return fmt.Errorf("read resume ack: %w", err)
	}
	if ack.RunWaitId != restore.RunWait.ID {
		phases.Record(vm.RuntimePhase{Name: "restore_attach_guest_resume", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)), ErrorClass: "resume_ack_mismatch"})
		return fmt.Errorf("resume ack run wait %q did not match expected %q", ack.RunWaitId, restore.RunWait.ID)
	}
	phases.Record(vm.RuntimePhase{Name: "restore_attach_guest_resume", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started))})
	if err := acknowledger.AcknowledgeRestore(ctx, RestoreAcknowledgement{
		Lease:        currentRunLease(request),
		RunWaitID:    restore.RunWait.ID,
		CheckpointID: restore.CheckpointID,
		Phases:       phases.Snapshot(),
	}); err != nil {
		return fmt.Errorf("acknowledge restore: %w", err)
	}
	return nil
}

func readResumeAck(ctx context.Context, session vm.Session) (*runv0.ResumeAck, error) {
	var ack runv0.ResumeAck
	if err := readProtoFrameContext(ctx, session, &ack); err != nil {
		return nil, err
	}
	return &ack, nil
}

func removeFiles(paths []string) {
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		_ = os.Remove(path)
	}
}

func readProtoFrameContext(ctx context.Context, session vm.Session, message proto.Message) error {
	return readProtoFrameFromReaderContext(ctx, session, session.Stream(), message)
}

func readProtoFrameFromReaderContext(ctx context.Context, session vm.Session, reader io.Reader, message proto.Message) error {
	result := make(chan error, 1)
	go func() {
		result <- frameio.ReadProtoFrame(reader, message)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = session.Close(context.Background())
		return ctx.Err()
	}
}

type runEventReadResult struct {
	event     *runv0.RunEvent
	workspace *workspace.WorkspaceArtifact
	err       error
}

func (r GuestRunner) readRunEventContext(ctx context.Context, session vm.Session, runID string) (*runv0.RunEvent, *workspace.WorkspaceArtifact, error) {
	result := make(chan runEventReadResult, 1)
	go func() {
		event, artifact, err := r.readRunEventOrWorkspaceFrame(ctx, session.Stream(), runID)
		result <- runEventReadResult{event: event, workspace: artifact, err: err}
	}()
	select {
	case value := <-result:
		return value.event, value.workspace, value.err
	case <-ctx.Done():
		_ = session.Close(context.Background())
		return nil, nil, ctx.Err()
	}
}

func (r GuestRunner) readRunEventOrWorkspaceFrame(ctx context.Context, reader io.Reader, runID string) (*runv0.RunEvent, *workspace.WorkspaceArtifact, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(reader, prefix[:]); err != nil {
		return nil, nil, err
	}
	if frameio.IsStreamFramePrefix(prefix[:]) {
		header, bodyLen, err := wire.ReadStreamFrameHeader(io.MultiReader(bytes.NewReader(prefix[:]), reader))
		if err != nil {
			return nil, nil, err
		}
		artifact, err := storeWorkspaceArtifactFrame(ctx, r.CAS, reader, header, bodyLen, runID)
		if err != nil {
			return nil, nil, err
		}
		return nil, &artifact, nil
	}
	size := binary.BigEndian.Uint32(prefix[:])
	if size > frameio.MaxFrameBytes {
		return nil, nil, fmt.Errorf("frameio message frame length %d exceeds max %d", size, frameio.MaxFrameBytes)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, nil, err
	}
	var event runv0.RunEvent
	if err := proto.Unmarshal(body, &event); err != nil {
		return nil, nil, fmt.Errorf("unmarshal frameio proto frame: %w", err)
	}
	return &event, nil, nil
}

type activeRuntimeClock struct {
	limit     time.Duration
	used      time.Duration
	startedAt time.Time
}

func newActiveRuntimeClock(limit time.Duration, used time.Duration) activeRuntimeClock {
	if used < 0 {
		used = 0
	}
	return activeRuntimeClock{limit: limit, used: used, startedAt: time.Now()}
}

func (c activeRuntimeClock) elapsed() time.Duration {
	return c.used + time.Since(c.startedAt)
}

func (c activeRuntimeClock) readContext(ctx context.Context) (context.Context, context.CancelFunc, bool, error) {
	if c.limit <= 0 {
		return ctx, func() {}, false, nil
	}
	remaining := c.limit - c.elapsed()
	if remaining <= 0 {
		return nil, nil, true, context.DeadlineExceeded
	}
	readCtx, cancel := context.WithTimeout(ctx, remaining)
	return readCtx, cancel, true, nil
}

type runtimeInputMetadata struct {
	workspaceBase api.WorkerCheckpointWorkspaceBase
}

func (r GuestRunner) writeRuntimeInput(ctx context.Context, stream io.Writer, request Request, deploymentSourceRoot string) (runtimeInputMetadata, error) {
	if err := ctx.Err(); err != nil {
		return runtimeInputMetadata{}, err
	}
	phaseStarted := time.Now()
	protocolRequest, err := runTaskRequest(request)
	r.logRunPhase(request, "guest run request built", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorString(err))
	if err != nil {
		return runtimeInputMetadata{}, err
	}
	phaseStarted = time.Now()
	if err := wire.WriteFileFrame(stream, wire.StreamHeader{Type: wire.StreamTypeRunImage, RunID: request.Run.RunID}, request.Artifact.ImageTarPath); err != nil {
		r.logRunPhase(request, "guest run image sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", err.Error())
		return runtimeInputMetadata{}, fmt.Errorf("write run image: %w", err)
	}
	r.logRunPhase(request, "guest run image sent", "duration_ms", time.Since(phaseStarted).Milliseconds())
	phaseStarted = time.Now()
	deploymentSourceTar, cleanupDeploymentSource, err := archive.CreateTarWithOptions(deploymentSourceRoot, r.TempDir, archive.TarOptions{
		ExcludePatterns: []string{"**/.git/**"},
	})
	r.logRunPhase(request, "guest run deployment source archived", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorString(err))
	if err != nil {
		return runtimeInputMetadata{}, err
	}
	defer cleanupDeploymentSource()
	phaseStarted = time.Now()
	if err := wire.WriteFileFrame(stream, wire.StreamHeader{Type: wire.StreamTypeDeploymentSource, RunID: request.Run.RunID}, deploymentSourceTar.Path); err != nil {
		r.logRunPhase(request, "guest run deployment source sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", err.Error())
		return runtimeInputMetadata{}, fmt.Errorf("write deployment source: %w", err)
	}
	r.logRunPhase(request, "guest run deployment source sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", deploymentSourceTar.SizeBytes)
	phaseStarted = time.Now()
	if err := frameio.WriteProtoFrame(stream, protocolRequest); err != nil {
		r.logRunPhase(request, "guest run request sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", err.Error())
		return runtimeInputMetadata{}, fmt.Errorf("write run request: %w", err)
	}
	r.logRunPhase(request, "guest run request sent", "duration_ms", time.Since(phaseStarted).Milliseconds())
	phaseStarted = time.Now()
	if err := wire.WriteFileFrame(stream, wire.StreamHeader{Type: wire.StreamTypeWorkspaceArtifact, RunID: request.Run.RunID}, request.Workspace.Path); err != nil {
		r.logRunPhase(request, "guest run workspace artifact sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", err.Error())
		return runtimeInputMetadata{}, fmt.Errorf("write workspace artifact: %w", err)
	}
	r.logRunPhase(request, "guest run workspace artifact sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", request.Workspace.SizeBytes)
	return runtimeInputMetadata{workspaceBase: checkpointWorkspaceBase(request, protocolRequest)}, nil
}

func (r GuestRunner) writeWorkspaceRunInput(ctx context.Context, stream io.Writer, request Request, deploymentSourceRoot string, channelToken string) (runtimeInputMetadata, error) {
	if err := ctx.Err(); err != nil {
		return runtimeInputMetadata{}, err
	}
	workspace := request.Run.Workspace
	workspaceMountID := strings.TrimSpace(workspace.WorkspaceMountID)
	if workspaceMountID == "" {
		return runtimeInputMetadata{}, errors.New("workspace mount id is required")
	}
	if strings.TrimSpace(workspace.ID) == "" {
		return runtimeInputMetadata{}, errors.New("workspace id is required")
	}
	channelToken = strings.TrimSpace(channelToken)
	if channelToken == "" {
		return runtimeInputMetadata{}, errors.New("workspace mount guest channel token is required")
	}
	if workspace.FencingGeneration <= 0 {
		return runtimeInputMetadata{}, errors.New("workspace mount fencing generation is required")
	}
	if strings.TrimSpace(workspace.WriteLeaseID) == "" {
		return runtimeInputMetadata{}, errors.New("workspace write lease id is required")
	}
	if strings.TrimSpace(workspace.WriteFencingToken) == "" {
		return runtimeInputMetadata{}, errors.New("workspace write fencing token is required")
	}
	phaseStarted := time.Now()
	protocolRequest, err := runTaskRequest(request)
	r.logRunPhase(request, "guest run request built", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorString(err))
	if err != nil {
		return runtimeInputMetadata{}, err
	}
	phaseStarted = time.Now()
	if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{
		Type:             wire.StreamTypeWorkspaceRun,
		RunID:            request.Run.RunID,
		WorkspaceID:      workspace.ID,
		WorkspaceMountID: workspaceMountID,
	}, 0); err != nil {
		r.logRunPhase(request, "guest workspace run header sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", err.Error())
		return runtimeInputMetadata{}, fmt.Errorf("write workspace run header: %w", err)
	}
	r.logRunPhase(request, "guest workspace run header sent", "duration_ms", time.Since(phaseStarted).Milliseconds())
	r.logRunPhase(request, "guest workspace run reusing materialized runtime", "h7_enabled", true, "run_image_sent_bytes", 0, "workspace_artifact_sent_bytes", 0)
	phaseStarted = time.Now()
	if err := frameio.WriteProtoFrame(stream, &workspacev0.WorkspaceOperationEnvelope{
		WorkspaceMountId:  workspaceMountID,
		WorkspaceId:       workspace.ID,
		ChannelToken:      channelToken,
		FencingGeneration: uint64(workspace.FencingGeneration),
		WriteLeaseId:      workspace.WriteLeaseID,
		FencingToken:      workspace.WriteFencingToken,
	}); err != nil {
		r.logRunPhase(request, "guest workspace run envelope sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", err.Error())
		return runtimeInputMetadata{}, fmt.Errorf("write workspace run envelope: %w", err)
	}
	r.logRunPhase(request, "guest workspace run envelope sent", "duration_ms", time.Since(phaseStarted).Milliseconds())
	phaseStarted = time.Now()
	deploymentSourceTar, cleanupDeploymentSource, err := archive.CreateTarWithOptions(deploymentSourceRoot, r.TempDir, archive.TarOptions{
		ExcludePatterns: []string{"**/.git/**"},
	})
	r.logRunPhase(request, "guest run deployment source archived", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", errorString(err))
	if err != nil {
		return runtimeInputMetadata{}, err
	}
	defer cleanupDeploymentSource()
	phaseStarted = time.Now()
	if err := wire.WriteFileFrame(stream, wire.StreamHeader{Type: wire.StreamTypeDeploymentSource, RunID: request.Run.RunID}, deploymentSourceTar.Path); err != nil {
		r.logRunPhase(request, "guest run deployment source sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", err.Error())
		return runtimeInputMetadata{}, fmt.Errorf("write deployment source: %w", err)
	}
	r.logRunPhase(request, "guest run deployment source sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "size_bytes", deploymentSourceTar.SizeBytes)
	phaseStarted = time.Now()
	if err := frameio.WriteProtoFrame(stream, protocolRequest); err != nil {
		r.logRunPhase(request, "guest run request sent", "duration_ms", time.Since(phaseStarted).Milliseconds(), "error", err.Error())
		return runtimeInputMetadata{}, fmt.Errorf("write run request: %w", err)
	}
	r.logRunPhase(request, "guest run request sent", "duration_ms", time.Since(phaseStarted).Milliseconds())
	return runtimeInputMetadata{workspaceBase: checkpointWorkspaceBase(request, protocolRequest)}, nil
}

func (r GuestRunner) logRunPhase(request Request, message string, attrs ...any) {
	log := r.Log
	if log == nil {
		log = slog.Default()
	}
	base := []any{
		"run_id", strings.TrimSpace(request.Run.RunID),
		"task_id", strings.TrimSpace(request.Run.TaskID),
		"session_id", strings.TrimSpace(request.Run.SessionID),
		"workspace_mount_id", strings.TrimSpace(request.Run.Workspace.WorkspaceMountID),
	}
	base = append(base, attrs...)
	log.Info(message, base...)
}

func checkpointWorkspaceBase(request Request, protocolRequest *runv0.RunTaskRequest) api.WorkerCheckpointWorkspaceBase {
	workspace := protocolRequest.GetWorkspace()
	base := api.WorkerCheckpointWorkspaceBase{
		ArtifactDigest:    request.Workspace.Digest,
		ArtifactSizeBytes: request.Workspace.SizeBytes,
		ArtifactMediaType: request.Workspace.MediaType,
		ArtifactEncoding:  request.Workspace.Encoding,
	}
	if workspace != nil {
		base.MountPath = workspace.Path
		if workspace.Artifact != nil {
			base.ArtifactDigest = workspace.Artifact.Digest
			base.ArtifactSizeBytes = int64(workspace.Artifact.SizeBytes)
			base.ArtifactMediaType = workspace.Artifact.MediaType
			base.ArtifactEncoding = workspace.Artifact.Encoding
		}
	}
	return base
}

func (r GuestRunner) readRunEvents(ctx context.Context, session vm.Session, request Request, inputMetadata runtimeInputMetadata) (Result, error) {
	stream := session.Stream()
	active := newActiveRuntimeClock(request.Run.MaxDuration, request.Run.ActiveUsed)
	var observedSeq uint64
	var finalWorkspace *workspace.WorkspaceArtifact
	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		readCtx, cancelRead, activeLimited, err := active.readContext(ctx)
		if err != nil {
			return Result{}, runtimeMaxDurationError(request.Run.MaxDuration)
		}
		event, workspaceArtifact, err := r.readRunEventContext(readCtx, session, request.Run.RunID)
		cancelRead()
		if err != nil {
			if activeLimited && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
				return Result{}, runtimeMaxDurationError(request.Run.MaxDuration)
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return Result{}, err
			}
			return Result{}, fmt.Errorf("read run event: %w", err)
		}
		if workspaceArtifact != nil {
			if finalWorkspace != nil {
				return Result{}, errors.New("guest published multiple final workspace artifacts")
			}
			finalWorkspace = workspaceArtifact
			continue
		}
		observedSeq++
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StdoutChunk:
			if r.Stdout != nil {
				if _, err := r.Stdout.Write(value.StdoutChunk); err != nil {
					return Result{}, fmt.Errorf("write stdout event: %w", err)
				}
			}
			if err := r.appendLog(ctx, currentRunLease(request), api.WorkerLogStreamStdout, observedSeq, value.StdoutChunk); err != nil {
				return Result{}, err
			}
		case *runv0.RunEvent_StderrChunk:
			if r.Stderr != nil {
				if _, err := r.Stderr.Write(value.StderrChunk); err != nil {
					return Result{}, fmt.Errorf("write stderr event: %w", err)
				}
			}
			if err := r.appendLog(ctx, currentRunLease(request), api.WorkerLogStreamStderr, observedSeq, value.StderrChunk); err != nil {
				return Result{}, err
			}
		case *runv0.RunEvent_LogEntry:
			if err := r.recordLogEntry(ctx, currentRunLease(request), value.LogEntry); err != nil {
				return Result{}, err
			}
		case *runv0.RunEvent_OutputStreamAppended:
			if err := r.appendOutputStream(ctx, currentRunLease(request), value.OutputStreamAppended); err != nil {
				return Result{}, err
			}
		case *runv0.RunEvent_ActiveStreamReadRequested:
			readCtx, cancelRead, activeLimited, err := active.readContext(ctx)
			if err != nil {
				return Result{}, runtimeMaxDurationError(request.Run.MaxDuration)
			}
			result, err := r.readInputStream(readCtx, currentRunLease(request), value.ActiveStreamReadRequested)
			cancelRead()
			if err != nil {
				if activeLimited && errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
					return Result{}, runtimeMaxDurationError(request.Run.MaxDuration)
				}
				return Result{}, err
			}
			if err := frameio.WriteProtoFrame(stream, result); err != nil {
				return Result{}, fmt.Errorf("write active stream read result: %w", err)
			}
		case *runv0.RunEvent_MetadataUpdated:
			if err := r.updateRunMetadata(ctx, currentRunLease(request), value.MetadataUpdated); err != nil {
				return Result{}, err
			}
		case *runv0.RunEvent_TokenCreateRequested:
			result := r.createToken(ctx, currentRunLease(request), value.TokenCreateRequested)
			if err := frameio.WriteProtoFrame(stream, result); err != nil {
				return Result{}, fmt.Errorf("write token create result: %w", err)
			}
		case *runv0.RunEvent_RunWaitRequested:
			if err := r.handleRunWaitRequested(ctx, stream, session, request, value.RunWaitRequested, active.elapsed(), inputMetadata, &observedSeq); err != nil {
				if errors.Is(err, ErrDetached) {
					return Result{Detached: true, ActiveDuration: active.elapsed()}, nil
				}
				return Result{}, err
			}
		case *runv0.RunEvent_TaskResult:
			if value.TaskResult == nil {
				return Result{}, errors.New("guest task_result event is empty")
			}
			if strings.TrimSpace(value.TaskResult.GetErrorMessage()) != "" {
				return Result{}, errors.New(value.TaskResult.GetErrorMessage())
			}
			result := Result{ExitCode: value.TaskResult.ExitCode, ActiveDuration: active.elapsed()}
			if value.TaskResult.ExitCode == 0 && value.TaskResult.OutputJson != nil {
				output := json.RawMessage(value.TaskResult.GetOutputJson())
				if !json.Valid(output) {
					return Result{}, errors.New("guest task_result output_json must be valid JSON")
				}
				result.Output = append(json.RawMessage(nil), output...)
			}
			if value.TaskResult.ExitCode == 0 {
				result.Workspace = finalWorkspace
			}
			return result, nil
		case nil:
			return Result{}, errors.New("guest run event is empty")
		default:
			return Result{}, fmt.Errorf("unsupported guest run event %T", value)
		}
	}
}

func (r GuestRunner) processCheckpointRunEvent(ctx context.Context, request Request, observedSeq *uint64, event *runv0.RunEvent) error {
	if observedSeq == nil {
		return errors.New("checkpoint run event sequence is required")
	}
	(*observedSeq)++
	switch value := event.GetEvent().(type) {
	case *runv0.RunEvent_StdoutChunk:
		if r.Stdout != nil {
			if _, err := r.Stdout.Write(value.StdoutChunk); err != nil {
				return fmt.Errorf("write stdout event: %w", err)
			}
		}
		return r.appendLog(ctx, currentRunLease(request), api.WorkerLogStreamStdout, *observedSeq, value.StdoutChunk)
	case *runv0.RunEvent_StderrChunk:
		if r.Stderr != nil {
			if _, err := r.Stderr.Write(value.StderrChunk); err != nil {
				return fmt.Errorf("write stderr event: %w", err)
			}
		}
		return r.appendLog(ctx, currentRunLease(request), api.WorkerLogStreamStderr, *observedSeq, value.StderrChunk)
	case *runv0.RunEvent_LogEntry:
		return r.recordLogEntry(ctx, currentRunLease(request), value.LogEntry)
	case *runv0.RunEvent_OutputStreamAppended:
		return r.appendOutputStream(ctx, currentRunLease(request), value.OutputStreamAppended)
	case *runv0.RunEvent_ActiveStreamReadRequested:
		return errors.New("active stream read is not supported while checkpointing")
	case *runv0.RunEvent_MetadataUpdated:
		return r.updateRunMetadata(ctx, currentRunLease(request), value.MetadataUpdated)
	case *runv0.RunEvent_TokenCreateRequested:
		return errors.New("token creation is not supported while checkpointing")
	case *runv0.RunEvent_RunWaitRequested:
		return errors.New("additional run wait requests are not supported while checkpointing")
	case nil:
		return errors.New("guest run event is empty")
	default:
		return fmt.Errorf("unsupported checkpoint interleaved guest run event %T", value)
	}
}

type MaxDurationError struct {
	Limit time.Duration
}

func (e MaxDurationError) Error() string {
	return fmt.Sprintf("runtime max_duration exceeded after %s active time", e.Limit)
}

func runtimeMaxDurationError(limit time.Duration) error {
	return MaxDurationError{Limit: limit}
}

func (r GuestRunner) appendLog(ctx context.Context, claim api.WorkerRunLease, stream api.WorkerLogStream, observedSeq uint64, content []byte) error {
	if r.Events == nil {
		return nil
	}
	if _, err := r.Events.AppendLog(ctx, claim, stream, observedSeq, content); err != nil {
		return fmt.Errorf("append %s log: %w", stream, err)
	}
	return nil
}

func (r GuestRunner) recordLogEntry(ctx context.Context, claim api.WorkerRunLease, entry string) error {
	if r.Events == nil {
		return nil
	}
	if _, err := r.Events.RecordLogEntry(ctx, claim, entry); err != nil {
		return fmt.Errorf("record log entry: %w", err)
	}
	return nil
}

func (r GuestRunner) appendOutputStream(ctx context.Context, claim api.WorkerRunLease, output *runv0.OutputStreamAppended) error {
	if r.Events == nil {
		return nil
	}
	if output == nil {
		return errors.New("guest output_stream_appended is empty")
	}
	stream := strings.TrimSpace(output.GetStream())
	if stream == "" {
		return errors.New("guest output_stream_appended stream is required")
	}
	payload := json.RawMessage(output.GetPayloadJson())
	if len(payload) == 0 {
		payload = json.RawMessage(`null`)
	}
	if !json.Valid(payload) {
		return errors.New("guest output_stream_appended payload_json must be valid JSON")
	}
	if _, err := r.Events.AppendOutputStream(ctx, api.WorkerOutputStreamAppendRequest{
		Lease:       claim,
		Stream:      stream,
		Data:        payload,
		ContentType: output.GetContentType(),
	}); err != nil {
		return fmt.Errorf("append output stream %q: %w", stream, err)
	}
	return nil
}

func (r GuestRunner) readInputStream(ctx context.Context, lease api.WorkerRunLease, request *runv0.ActiveStreamReadRequested) (*runv0.ActiveStreamReadResult, error) {
	correlationID := ""
	if request != nil {
		correlationID = request.GetCorrelationId()
	}
	if r.Events == nil {
		return &runv0.ActiveStreamReadResult{CorrelationId: correlationID, ErrorMessage: new("runtime event sink is required")}, nil
	}
	if request == nil {
		return &runv0.ActiveStreamReadResult{ErrorMessage: new("guest active_stream_read_requested is empty")}, nil
	}
	stream := strings.TrimSpace(request.GetStream())
	if stream == "" {
		return &runv0.ActiveStreamReadResult{CorrelationId: correlationID, ErrorMessage: new("active stream read stream is required")}, nil
	}
	response, err := r.Events.ReadInputStream(ctx, api.WorkerActiveStreamReadRequest{
		Lease:          lease,
		Stream:         stream,
		AfterSequence:  request.GetAfterSequence(),
		CorrelationID:  request.GetRecordCorrelationId(),
		TimeoutSeconds: protoUint32ToInt32Ptr(request.Timeout),
		Block:          request.GetBlock(),
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return &runv0.ActiveStreamReadResult{CorrelationId: correlationID, ErrorMessage: new(err.Error())}, nil
	}
	result := &runv0.ActiveStreamReadResult{
		CorrelationId: correlationID,
		TimedOut:      response.TimedOut,
	}
	if response.Record != nil {
		result.Record = protoStreamRecord(*response.Record)
	}
	return result, nil
}

func protoStreamRecord(record api.StreamRecordResponse) *runv0.StreamRecord {
	return &runv0.StreamRecord{
		Id:            record.ID,
		StreamId:      record.StreamID,
		Sequence:      record.Sequence,
		DataJson:      string(record.Data),
		CorrelationId: optionalString(record.CorrelationID),
		ContentType:   record.ContentType,
		CreatedAt:     record.CreatedAt.Format(time.RFC3339Nano),
	}
}

func protoUint32ToInt32Ptr(value *uint32) *int32 {
	if value == nil {
		return nil
	}
	if *value > math.MaxInt32 {
		max := int32(math.MaxInt32)
		return &max
	}
	converted := int32(*value)
	return &converted
}

func (r GuestRunner) createToken(ctx context.Context, lease api.WorkerRunLease, request *runv0.TokenCreateRequested) *runv0.TokenCreateResult {
	if r.Events == nil {
		return &runv0.TokenCreateResult{ErrorMessage: new("runtime event sink is required")}
	}
	if request == nil {
		return &runv0.TokenCreateResult{ErrorMessage: new("guest token_create_requested is empty")}
	}
	var timeoutAt *time.Time
	if strings.TrimSpace(request.GetTimeoutAt()) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(request.GetTimeoutAt()))
		if err != nil {
			return &runv0.TokenCreateResult{ErrorMessage: new("token timeout_at must be an RFC3339 timestamp")}
		}
		timeoutAt = &parsed
	}
	var timeoutInSeconds *int32
	if request.TimeoutInSeconds != nil {
		if request.GetTimeoutInSeconds() > math.MaxInt32 {
			return &runv0.TokenCreateResult{ErrorMessage: new("token timeout_in_seconds is too large")}
		}
		value := int32(request.GetTimeoutInSeconds())
		timeoutInSeconds = &value
	}
	metadata := json.RawMessage(request.GetMetadataJson())
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	if !json.Valid(metadata) {
		return &runv0.TokenCreateResult{ErrorMessage: new("token metadata_json must be valid JSON")}
	}
	token, err := r.Events.CreateRuntimeToken(ctx, api.WorkerCreateTokenRequest{
		Lease:            lease,
		TimeoutAt:        timeoutAt,
		TimeoutInSeconds: timeoutInSeconds,
		Tags:             request.GetTags(),
		Metadata:         metadata,
	})
	if err != nil {
		return &runv0.TokenCreateResult{ErrorMessage: new(err.Error())}
	}
	result := &runv0.TokenCreateResult{
		Id:          token.ID,
		CallbackUrl: token.CallbackURL,
		TimeoutAt:   timePtrString(token.TimeoutAt),
		Status:      optionalString(token.Status),
		Tags:        token.Tags,
	}
	if strings.TrimSpace(token.PublicAccessToken) != "" {
		result.PublicAccessToken = new(token.PublicAccessToken)
	}
	if len(token.Metadata) > 0 {
		result.MetadataJson = new(string(token.Metadata))
	}
	return result
}

func optionalString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func timePtrString(value *time.Time) *string {
	if value == nil {
		return nil
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	return &formatted
}

func (r GuestRunner) updateRunMetadata(ctx context.Context, claim api.WorkerRunLease, metadata *runv0.MetadataUpdated) error {
	if r.Events == nil {
		return nil
	}
	if metadata == nil {
		return errors.New("guest metadata_updated is empty")
	}
	operation := strings.TrimSpace(metadata.GetOperation())
	if operation == "" {
		return errors.New("guest metadata_updated operation is required")
	}
	request := api.WorkerUpdateRunMetadataRequest{
		Lease:     claim,
		Operation: operation,
		Key:       strings.TrimSpace(metadata.GetKey()),
		Amount:    metadata.GetAmount(),
	}
	if metadata.GetValueJson() != "" {
		value := json.RawMessage(metadata.GetValueJson())
		if !json.Valid(value) {
			return errors.New("guest metadata_updated value_json must be valid JSON")
		}
		request.Value = value
	}
	if metadata.GetPatchJson() != "" {
		patch := json.RawMessage(metadata.GetPatchJson())
		if !json.Valid(patch) {
			return errors.New("guest metadata_updated patch_json must be valid JSON")
		}
		request.Patch = patch
	}
	if _, err := r.Events.UpdateRunMetadata(ctx, request); err != nil {
		return fmt.Errorf("update run metadata: %w", err)
	}
	return nil
}

func (r GuestRunner) handleRunWaitRequested(ctx context.Context, stream io.ReadWriteCloser, session vm.Session, request Request, wait *runv0.RunWaitRequested, activeDuration time.Duration, inputMetadata runtimeInputMetadata, observedSeq *uint64) error {
	if request.WaitHandler == nil {
		return errors.New("guest wait request requires a run wait handler")
	}
	runtimeWait, err := runtimeWaitRequest(request, wait)
	if err != nil {
		return err
	}
	runtimeWait.Leases = request.Leases
	runtimeWait.Lease = currentRunLease(request)
	runtimeWait.ActiveDuration = activeDuration
	resumeSent := false
	runtimeWait.Resume = func(resumeCtx context.Context, decision WaitResumeDecision) error {
		if strings.TrimSpace(decision.Kind) == "" {
			return errors.New("run wait resume kind is required")
		}
		if len(decision.Data) == 0 {
			decision.Data = json.RawMessage(`null`)
		}
		if err := wire.WriteResumeDecision(stream, &runv0.ResumeDecision{
			RunWaitId: wait.CorrelationId,
			Kind:      decision.Kind,
			DataJson:  string(decision.Data),
		}); err != nil {
			return fmt.Errorf("write immediate resume decision: %w", err)
		}
		resumeSent = true
		return resumeCtx.Err()
	}
	if checkpointable, ok := session.(vm.CheckpointableSession); ok {
		runtimeWait.Checkpointer = runtimeCheckpointer{
			session:           checkpointable,
			cas:               r.CAS,
			encryptor:         r.CheckpointEncryptor,
			tempDir:           r.tempDir(),
			stream:            stream,
			workspace:         inputMetadata.workspaceBase,
			substrateSource:   request.Run.Workspace.SubstrateSource,
			runtimeSubstrates: r.RuntimeSubstrates,
			runEvent: func(eventCtx context.Context, event *runv0.RunEvent) error {
				return r.processCheckpointRunEvent(eventCtx, request, observedSeq, event)
			},
		}
	}
	if err := request.WaitHandler.Wait(ctx, runtimeWait); err != nil {
		return err
	}
	if resumeSent {
		return nil
	}
	return errors.New("run wait handler returned without detaching runtime")
}

func currentRunLease(request Request) api.WorkerRunLease {
	return request.Leases.CurrentWorkerRunLease()
}

func runtimeWaitRequest(request Request, wait *runv0.RunWaitRequested) (WaitRequest, error) {
	if wait == nil {
		return WaitRequest{}, errors.New("guest wait request is empty")
	}
	correlationID := strings.TrimSpace(wait.GetCorrelationId())
	if correlationID == "" {
		return WaitRequest{}, errors.New("guest wait request correlation_id is required")
	}
	kind := api.WorkerRunWaitKind(strings.TrimSpace(wait.GetKind()))
	if kind == "" {
		return WaitRequest{}, errors.New("guest wait request kind is required")
	}
	paramsJSON := strings.TrimSpace(wait.GetParamsJson())
	if paramsJSON == "" {
		paramsJSON = "{}"
	}
	if !json.Valid([]byte(paramsJSON)) {
		return WaitRequest{}, errors.New("guest wait params_json must be valid JSON")
	}
	metadataJSON := strings.TrimSpace(wait.GetMetadataJson())
	if metadataJSON == "" {
		metadataJSON = "{}"
	}
	if !json.Valid([]byte(metadataJSON)) {
		return WaitRequest{}, errors.New("guest wait metadata_json must be valid JSON")
	}
	var metadataCompact bytes.Buffer
	if err := json.Compact(&metadataCompact, []byte(metadataJSON)); err != nil {
		return WaitRequest{}, fmt.Errorf("guest wait metadata_json must be valid JSON: %w", err)
	}
	if metadataCompact.Len() > waitMetadataJSONMaxBytes {
		return WaitRequest{}, fmt.Errorf("guest wait metadata_json is %d bytes, exceeds max %d", metadataCompact.Len(), waitMetadataJSONMaxBytes)
	}
	if !waitMetadataJSONObject([]byte(metadataJSON)) {
		return WaitRequest{}, errors.New("guest wait metadata_json must be a JSON object")
	}
	tags, err := normalizeRuntimeWaitTags(wait.GetTags())
	if err != nil {
		return WaitRequest{}, err
	}
	timeout, err := waitTimeoutSeconds(wait.Timeout)
	if err != nil {
		return WaitRequest{}, err
	}
	idleTimeout, err := waitTimeoutSeconds(wait.IdleTimeout)
	if err != nil {
		return WaitRequest{}, err
	}
	return WaitRequest{
		Lease:              currentRunLease(request),
		CorrelationID:      correlationID,
		Kind:               kind,
		Params:             []byte(paramsJSON),
		Metadata:           []byte(metadataJSON),
		Tags:               tags,
		TimeoutSeconds:     timeout,
		IdleTimeoutSeconds: idleTimeout,
	}, nil
}

func normalizeRuntimeWaitTags(tags []string) ([]string, error) {
	if len(tags) > waitTagsMaxCount {
		return nil, fmt.Errorf("guest wait tags has %d entries, exceeds max %d", len(tags), waitTagsMaxCount)
	}
	normalized := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return nil, errors.New("guest wait tags must be non-empty")
		}
		if len([]byte(tag)) > waitTagMaxBytes {
			return nil, fmt.Errorf("guest wait tag is %d bytes, exceeds max %d", len([]byte(tag)), waitTagMaxBytes)
		}
		normalized = append(normalized, tag)
	}
	return normalized, nil
}

func waitMetadataJSONObject(value []byte) bool {
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(value, &decoded); err != nil {
		return false
	}
	return decoded != nil
}

func waitTimeoutSeconds(value *uint32) (*int32, error) {
	if value == nil {
		return nil, nil
	}
	if *value > math.MaxInt32 {
		return nil, fmt.Errorf("wait timeout %d exceeds max %d", *value, int64(math.MaxInt32))
	}
	timeout := int32(*value)
	return &timeout, nil
}

var _ Runner = GuestRunner{}
