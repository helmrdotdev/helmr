package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/frameio"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
)

func (r ProgramRunner) materializeCheckpointObject(ctx context.Context, digest string, suffix string) (string, error) {
	return r.materializeEncryptedObject(ctx, digest, suffix, checkpointPurpose(suffix))
}

func (r ProgramRunner) materializeEncryptedObject(ctx context.Context, digest string, suffix string, purpose string) (string, error) {
	if r.CheckpointEncryptor == nil {
		return "", errors.New("checkpoint encryption is required")
	}
	body, err := r.CAS.Get(ctx, digest)
	if err != nil {
		return "", fmt.Errorf("get checkpoint object %s: %w", digest, err)
	}
	if err := os.MkdirAll(r.tempDir(), 0o755); err != nil {
		_ = body.Close()
		return "", fmt.Errorf("create checkpoint temp dir: %w", err)
	}
	file, err := os.CreateTemp(r.tempDir(), "checkpoint-*."+suffix)
	if err != nil {
		_ = body.Close()
		return "", fmt.Errorf("create checkpoint temp file: %w", err)
	}
	path := file.Name()
	hash := sha256.New()
	copyErr := r.CheckpointEncryptor.Decrypt(ctx, io.TeeReader(body, hash), file, purpose)
	bodyCloseErr := body.Close()
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("decrypt checkpoint object %s: %w", digest, copyErr)
	}
	if bodyCloseErr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close checkpoint object %s: %w", digest, bodyCloseErr)
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close checkpoint object %s: %w", digest, closeErr)
	}
	actual := sha256sum.DigestHash(hash)
	if actual != digest {
		_ = os.Remove(path)
		return "", fmt.Errorf("checkpoint object digest mismatch: expected %s, got %s", digest, actual)
	}
	return path, nil
}

func validateRestoreIdentity(
	checkpoint workerapi.CheckpointManifest,
	workerArchitecture deployment.RuntimeArchitecture,
) error {
	runtimeInfo := checkpoint.RecoveryPoint.Runtime
	if runtimeInfo.Backend != "firecracker" {
		return fmt.Errorf("restore checkpoint recovery_point.runtime.backend %q is not supported", runtimeInfo.Backend)
	}
	if err := deployment.ValidateRuntimeArchitecture(workerArchitecture); err != nil {
		return fmt.Errorf("validate worker runtime architecture: %w", err)
	}
	if runtimeInfo.Arch != string(workerArchitecture) {
		return fmt.Errorf("restore checkpoint recovery_point.runtime.arch %q does not match worker arch %q", runtimeInfo.Arch, workerArchitecture)
	}
	if strings.TrimSpace(runtimeInfo.Contract) == "" {
		return errors.New("restore checkpoint recovery_point.runtime.contract is required")
	}
	if err := requireCheckpointDigest("recovery_point.runtime.id", runtimeInfo.ID); err != nil {
		return err
	}
	if err := requireCheckpointDigest("recovery_point.runtime.kernel_digest", runtimeInfo.KernelDigest); err != nil {
		return err
	}
	if err := requireCheckpointDigest("recovery_point.runtime.initramfs_digest", runtimeInfo.InitramfsDigest); err != nil {
		return err
	}
	if err := requireCheckpointDigest("recovery_point.runtime.rootfs_digest", runtimeInfo.RootfsDigest); err != nil {
		return err
	}
	if err := requireCheckpointDigest("recovery_point.runtime.config_digest", runtimeInfo.ConfigDigest); err != nil {
		return err
	}
	if runtimeInfo.VMVCPUCount <= 0 {
		return errors.New("restore checkpoint recovery_point.runtime.vm_vcpu_count must be positive")
	}
	if !sha256sum.ValidDigest(runtimeInfo.CPUConfigDigest) {
		return errors.New("restore checkpoint recovery_point.runtime.cpu_config_digest must be canonical")
	}
	if runtimeInfo.Substrate != nil {
		if err := requireCheckpointDigest("recovery_point.runtime.substrate.digest", runtimeInfo.Substrate.Digest); err != nil {
			return err
		}
		if strings.TrimSpace(runtimeInfo.Substrate.Format) == "" {
			return errors.New("restore checkpoint recovery_point.runtime.substrate.format is required")
		}
		if strings.TrimSpace(runtimeInfo.Substrate.Contract) == "" {
			return errors.New("restore checkpoint recovery_point.runtime.substrate.contract is required")
		}
		if runtimeInfo.Substrate.SizeBytes <= 0 {
			return errors.New("restore checkpoint recovery_point.runtime.substrate.size_bytes must be positive")
		}
	}
	return requireCheckpointArtifact(checkpoint.RuntimeState.ConfigArtifact, "runtime_state.config_artifact")
}

func requireCheckpointDigest(field string, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("restore checkpoint %s is required", field)
	}
	return nil
}

func requireCheckpointArtifact(artifact workerapi.CheckpointArtifact, field string) error {
	if strings.TrimSpace(artifact.Digest) == "" {
		return fmt.Errorf("restore checkpoint %s.digest is required", field)
	}
	if strings.TrimSpace(artifact.MediaType) == "" {
		return fmt.Errorf("restore checkpoint %s.media_type is required", field)
	}
	return nil
}

type runtimeCheckpointer struct {
	session    vm.CheckpointableSession
	cas        cas.Store
	encryptor  *checkpoint.Encryptor
	tempDir    string
	stream     io.ReadWriteCloser
	workspace  workerapi.CheckpointWorkspaceBase
	runEvent   func(context.Context, *runv0.RunEvent) error
	freezeGate *sync.Mutex
	onFrozen   func()
}

func (c runtimeCheckpointer) CreateCheckpoint(ctx context.Context, request CheckpointRequest) (result CheckpointResult, err error) {
	if c.session == nil {
		return CheckpointResult{}, errors.New("checkpoint source session is required")
	}
	sourceReleaseAttempted := false
	defer func() {
		if err == nil || sourceReleaseAttempted {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = c.releaseCheckpointSource(cleanupCtx)
	}()
	if c.cas == nil {
		return CheckpointResult{}, errors.New("checkpoint CAS is required")
	}
	if c.encryptor == nil {
		return CheckpointResult{}, errors.New("checkpoint encryption is required")
	}
	if c.stream == nil {
		return CheckpointResult{}, errors.New("checkpoint control stream is required")
	}
	phases := []workerapi.CheckpointPhase{}
	recordPhase := func(name string, started time.Time) {
		phases = append(phases, workerapi.CheckpointPhase{Name: name, DurationMs: durationMilliseconds(time.Since(started))})
	}
	started := time.Now()
	workspaceCapture, err := c.suspendGuestForCheckpoint(ctx, request)
	if err != nil {
		return CheckpointResult{}, err
	}
	recordPhase("suspend_guest", started)
	started = time.Now()
	if err := c.stream.Close(); err != nil {
		return CheckpointResult{}, fmt.Errorf("close checkpoint control stream: %w", err)
	}
	recordPhase("close_control_stream", started)
	started = time.Now()
	artifact, err := c.session.CreateSnapshot(ctx, vm.SnapshotRequest{ID: request.CheckpointID})
	if err != nil {
		return CheckpointResult{}, err
	}
	recordPhase("create_runtime_snapshot", started)
	phases = append(phases, workerCheckpointPhases(artifact.Phases)...)
	defer func() {
		cleanupSnapshotArtifact(artifact)
	}()
	started = time.Now()
	manifest, err := c.storeSnapshotArtifact(ctx, request, artifact)
	if err != nil {
		return CheckpointResult{}, err
	}
	recordPhase("store_checkpoint_artifacts", started)
	started = time.Now()
	sourceReleaseAttempted = true
	if err := c.releaseCheckpointSource(ctx); err != nil {
		return CheckpointResult{}, fmt.Errorf("release checkpoint source: %w", err)
	}
	recordPhase("release_checkpoint_source", started)
	sourceCleanup := &workerapi.RuntimeCleanupProof{
		Method: workerapi.RuntimeCleanupSessionClosed, CompletedAt: time.Now().UTC(),
	}
	manifest.Phases = phases
	return CheckpointResult{
		Manifest: manifest, WorkspaceCapture: workspaceCapture, SourceCleanup: sourceCleanup,
	}, nil
}

func (c runtimeCheckpointer) releaseCheckpointSource(ctx context.Context) error {
	if releaser, ok := c.session.(CheckpointSourceReleaser); ok {
		return releaser.ReleaseCheckpointSource(ctx)
	}
	return c.session.Close(ctx)
}

func (c runtimeCheckpointer) suspendGuestForCheckpoint(ctx context.Context, request CheckpointRequest) (*CheckpointWorkspaceCapture, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := wire.WriteCheckpointPauseRequest(c.stream, &runv0.CheckpointPauseRequest{
		RunId:                    request.RunID,
		AttemptNumber:            uint32(request.AttemptNumber),
		RunLeaseId:               request.RunLeaseID,
		RunWaitId:                request.RunWaitID,
		CorrelationId:            request.CorrelationID,
		CheckpointId:             request.CheckpointID,
		ResumeAttachId:           request.ResumeAttachID,
		CheckpointRequestVersion: request.CheckpointRequestVersion,
		CaptureWorkspace:         request.CaptureWorkspace,
	}); err != nil {
		return nil, fmt.Errorf("write checkpoint suspend: %w", err)
	}
	reader := bufio.NewReader(c.stream)
	pauseCtx, cancelPause := context.WithTimeout(ctx, checkpointSuspendTimeout)
	ready, workspaceArtifact, err := c.readPauseReadyContext(pauseCtx, reader, request)
	cancelPause()
	if err != nil {
		return nil, fmt.Errorf("read checkpoint pause ready: %w", err)
	}
	if err := validateCheckpointPauseReady(ready, request); err != nil {
		return nil, err
	}
	if c.freezeGate != nil {
		c.freezeGate.Lock()
	}
	if c.onFrozen != nil {
		c.onFrozen()
	}
	if c.freezeGate != nil {
		c.freezeGate.Unlock()
	}
	if request.CaptureWorkspace && workspaceArtifact == nil {
		return nil, errors.New("checkpoint pause did not return required workspace capture")
	}
	if workspaceArtifact == nil {
		return nil, nil
	}
	body, err := c.cas.Get(ctx, workspaceArtifact.Digest)
	if err != nil {
		return nil, fmt.Errorf("reopen checkpoint workspace artifact: %w", err)
	}
	tree, inspectErr := workspace.InspectArtifact(body, *workspaceArtifact)
	closeErr := body.Close()
	if inspectErr != nil {
		return nil, fmt.Errorf("inspect checkpoint workspace artifact: %w", inspectErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close checkpoint workspace artifact: %w", closeErr)
	}
	return &CheckpointWorkspaceCapture{Tree: tree, Artifact: *workspaceArtifact}, nil
}

func validateCheckpointPauseReady(ready *runv0.CheckpointPauseReady, request CheckpointRequest) error {
	if request.ResumeAttachID == "" {
		if ready == nil || ready.GetRunWaitId() != request.RunWaitID || ready.GetCheckpointId() != request.CheckpointID {
			return errors.New("checkpoint pause ready did not match request")
		}
		return nil
	}
	if ready == nil ||
		ready.GetRunId() != request.RunID ||
		ready.GetAttemptNumber() != uint32(request.AttemptNumber) ||
		ready.GetRunLeaseId() != request.RunLeaseID ||
		ready.GetRunWaitId() != request.RunWaitID ||
		ready.GetCorrelationId() != request.CorrelationID ||
		ready.GetCheckpointId() != request.CheckpointID ||
		ready.GetResumeAttachId() != request.ResumeAttachID ||
		ready.GetCheckpointRequestVersion() != request.CheckpointRequestVersion {
		return errors.New("checkpoint pause ready did not match exact request authority")
	}
	return nil
}

func (c runtimeCheckpointer) readPauseReadyContext(ctx context.Context, reader *bufio.Reader, request CheckpointRequest) (*runv0.CheckpointPauseReady, *workspace.WorkspaceArtifact, error) {
	type pauseReadyResult struct {
		ready            *runv0.CheckpointPauseReady
		workspaceCapture *workspace.WorkspaceArtifact
		err              error
	}
	result := make(chan pauseReadyResult, 1)
	go func() {
		parsed := &runv0.CheckpointPauseReady{}
		workspaceCapture, err := c.readPauseReady(ctx, reader, request, parsed)
		result <- pauseReadyResult{
			ready:            parsed,
			workspaceCapture: workspaceCapture,
			err:              err,
		}
	}()
	select {
	case result := <-result:
		if result.err != nil {
			return nil, nil, result.err
		}
		return result.ready, result.workspaceCapture, nil
	case <-ctx.Done():
		_ = c.stream.Close()
		return nil, nil, ctx.Err()
	}
}

func (c runtimeCheckpointer) readPauseReady(ctx context.Context, reader *bufio.Reader, request CheckpointRequest, ready *runv0.CheckpointPauseReady) (*workspace.WorkspaceArtifact, error) {
	var workspaceCapture *workspace.WorkspaceArtifact
	for {
		prefix, err := reader.Peek(4)
		if err != nil {
			return nil, err
		}
		if frameio.IsStreamFramePrefix(prefix) {
			header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
			if err != nil {
				return nil, err
			}
			switch header.Type {
			case wire.StreamTypeCheckpointPauseReady:
				if header.RunWaitID != request.RunWaitID || header.CheckpointID != request.CheckpointID {
					return nil, fmt.Errorf("checkpoint pause ready mismatch: run_wait_id=%q checkpoint_id=%q", header.RunWaitID, header.CheckpointID)
				}
				if bodyLen == 0 {
					ready.RunWaitId = header.RunWaitID
					ready.CheckpointId = header.CheckpointID
				} else if bodyLen > uint64(frameio.MaxFrameBytes) {
					return nil, fmt.Errorf("checkpoint pause ready body length %d exceeds max %d", bodyLen, frameio.MaxFrameBytes)
				} else if body, err := io.ReadAll(io.LimitReader(reader, int64(bodyLen))); err != nil {
					return nil, fmt.Errorf("read checkpoint pause ready: %w", err)
				} else if uint64(len(body)) != bodyLen {
					return nil, io.ErrUnexpectedEOF
				} else if err := proto.Unmarshal(body, ready); err != nil {
					return nil, fmt.Errorf("unmarshal checkpoint pause ready: %w", err)
				} else if ready.GetRunWaitId() != header.RunWaitID || ready.GetCheckpointId() != header.CheckpointID {
					return nil, errors.New("checkpoint pause ready header did not match body")
				}
				return workspaceCapture, nil
			case wire.StreamTypeWorkspaceArtifact:
				if !request.CaptureWorkspace {
					return nil, errors.New("checkpoint pause returned unexpected workspace capture")
				}
				if workspaceCapture != nil {
					return nil, errors.New("checkpoint pause returned multiple workspace captures")
				}
				artifact, err := storeWorkspaceArtifactFrame(ctx, c.cas, reader, header, bodyLen, request.RunID)
				if err != nil {
					return nil, err
				}
				workspaceCapture = &artifact
			default:
				return nil, fmt.Errorf("unsupported checkpoint stream type %q", header.Type)
			}
			continue
		}
		body, err := frameio.ReadMessageFrame(reader)
		if err != nil {
			return nil, err
		}
		var event runv0.RunEvent
		if err := proto.Unmarshal(body, &event); err != nil {
			return nil, fmt.Errorf("unmarshal checkpoint interleaved run event: %w", err)
		}
		if c.runEvent == nil {
			return nil, errors.New("received run event while checkpoint pause ready is pending")
		}
		if err := c.runEvent(ctx, &event); err != nil {
			return nil, err
		}
	}
}

func (c runtimeCheckpointer) storeSnapshotArtifact(ctx context.Context, request CheckpointRequest, artifact vm.SnapshotArtifact) (workerapi.CheckpointManifest, error) {
	if artifact.VMVCPUCount <= 0 {
		return workerapi.CheckpointManifest{}, errors.New("checkpoint snapshot VM vCPU count must be positive")
	}
	if !sha256sum.ValidDigest(artifact.CPUConfigDigest) {
		return workerapi.CheckpointManifest{}, errors.New("checkpoint snapshot CPU configuration digest must be canonical")
	}
	var manifest storedCheckpointArtifact
	var state storedCheckpointArtifact
	var scratchDisk storedCheckpointArtifact
	memory := make([]workerapi.CheckpointArtifact, len(artifact.Memory))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(4)
	group.Go(func() error {
		stored, err := c.storeSnapshotReader(groupCtx, bytes.NewReader(artifact.Manifest), cas.CheckpointRuntimeConfigMediaType, "manifest")
		if err != nil {
			return fmt.Errorf("store checkpoint manifest: %w", err)
		}
		manifest = stored
		return nil
	})
	group.Go(func() error {
		stored, err := c.storeSnapshotFile(groupCtx, artifact.VMState, "vmstate")
		if err != nil {
			return fmt.Errorf("store checkpoint vm state: %w", err)
		}
		state = stored
		return nil
	})
	group.Go(func() error {
		stored, err := c.storeSnapshotFile(groupCtx, artifact.ScratchDisk, "scratch-disk")
		if err != nil {
			return fmt.Errorf("store checkpoint scratch disk: %w", err)
		}
		scratchDisk = stored
		return nil
	})
	for i, file := range artifact.Memory {
		group.Go(func() error {
			stored, err := c.storeSnapshotFile(groupCtx, file, "memory")
			if err != nil {
				return fmt.Errorf("store checkpoint memory: %w", err)
			}
			memory[i] = stored.artifact
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return workerapi.CheckpointManifest{}, err
	}
	for _, artifact := range memory {
		if strings.TrimSpace(artifact.Digest) == "" {
			return workerapi.CheckpointManifest{}, errors.New("stored checkpoint memory artifact is missing digest")
		}
	}
	return workerapi.CheckpointManifest{
		RecoveryPoint: workerapi.CheckpointRecoveryPoint{
			ID:            request.CheckpointID,
			RunID:         request.RunID,
			AttemptNumber: request.AttemptNumber,
			RunWaitID:     request.RunWaitID,
			CorrelationID: request.CorrelationID,
			Runtime: workerapi.CheckpointRuntime{
				Backend:         artifact.RuntimeBackend,
				ID:              artifact.RuntimeID,
				Arch:            artifact.RuntimeArch,
				Contract:        artifact.VMRuntimeContract,
				KernelDigest:    artifact.KernelDigest,
				InitramfsDigest: artifact.InitramfsDigest,
				RootfsDigest:    artifact.RootfsDigest,
				ConfigDigest:    artifact.RuntimeConfigDigest,
				VMVCPUCount:     artifact.VMVCPUCount,
				CPUConfigDigest: artifact.CPUConfigDigest,
				Substrate:       checkpointRuntimeSubstrate(artifact.Substrate),
			},
		},
		RuntimeState: workerapi.CheckpointRuntimeState{
			ConfigArtifact:      manifest.artifact,
			VMStateArtifact:     state.artifact,
			ScratchDiskArtifact: scratchDisk.artifact,
			MemoryArtifacts:     memory,
			Config:              artifact.Manifest,
		},
		WorkspaceState: workerapi.CheckpointWorkspaceState{
			Base: c.workspace,
		},
	}, nil
}

func checkpointRuntimeSubstrate(substrate *vm.RuntimeSubstrate) *workerapi.CheckpointRuntimeSubstrate {
	if substrate == nil {
		return nil
	}
	return &workerapi.CheckpointRuntimeSubstrate{
		Digest:    strings.TrimSpace(substrate.Digest),
		Format:    strings.TrimSpace(substrate.Format),
		Contract:  strings.TrimSpace(substrate.Contract),
		SizeBytes: substrate.SizeBytes,
	}
}

type storedCheckpointArtifact struct {
	artifact workerapi.CheckpointArtifact
}

func (c runtimeCheckpointer) storeSnapshotFile(ctx context.Context, file vm.SnapshotFile, suffix string) (storedCheckpointArtifact, error) {
	if strings.TrimSpace(file.Path) == "" {
		return storedCheckpointArtifact{}, fmt.Errorf("checkpoint %s path is required", suffix)
	}
	body, err := os.Open(file.Path)
	if err != nil {
		return storedCheckpointArtifact{}, err
	}
	defer body.Close()
	return c.storeSnapshotReader(ctx, body, file.MediaType, suffix)
}

func (c runtimeCheckpointer) storeSnapshotReader(ctx context.Context, body io.Reader, mediaType string, suffix string) (storedCheckpointArtifact, error) {
	stage, err := c.cas.Stage(ctx, mediaType)
	if err != nil {
		return storedCheckpointArtifact{}, err
	}
	if err := c.encryptor.Encrypt(ctx, body, stage, checkpointPurpose(suffix)); err != nil {
		_ = stage.Abort(context.Background())
		return storedCheckpointArtifact{}, err
	}
	object, err := stage.Commit(ctx)
	if err != nil {
		_ = stage.Abort(context.Background())
		return storedCheckpointArtifact{}, err
	}
	return storedCheckpointArtifact{artifact: workerapi.CheckpointArtifact{
		Digest:    object.Digest,
		SizeBytes: object.SizeBytes,
		MediaType: object.MediaType,
	}}, nil
}

func checkpointPurpose(suffix string) string {
	return "helmr.checkpoint." + suffix
}

func workerCheckpointPhases(phases []vm.RuntimePhase) []workerapi.CheckpointPhase {
	if len(phases) == 0 {
		return nil
	}
	result := make([]workerapi.CheckpointPhase, 0, len(phases))
	for _, phase := range phases {
		result = append(result, workerCheckpointPhase(phase))
	}
	return result
}

func workerCheckpointPhase(phase vm.RuntimePhase) workerapi.CheckpointPhase {
	return workerapi.CheckpointPhase{
		Name:       phase.Name,
		DurationMs: phase.DurationMs,
		Role:       phase.Role,
		MediaType:  phase.MediaType,
		ErrorClass: phase.ErrorClass,
		Filepack:   workerCheckpointFilepackStats(phase.Filepack),
	}
}

func workerCheckpointFilepackStats(stats *vm.FilepackStats) *workerapi.CheckpointFilepackStats {
	if stats == nil {
		return nil
	}
	return &workerapi.CheckpointFilepackStats{
		LogicalBytes:       stats.LogicalBytes,
		EncodedChunks:      stats.EncodedChunks,
		UnpackWrittenBytes: stats.UnpackWrittenBytes,
	}
}

func cleanupSnapshotArtifact(artifact vm.SnapshotArtifact) {
	_ = os.Remove(artifact.VMState.Path)
	_ = os.Remove(artifact.ScratchDisk.Path)
	for _, file := range artifact.Memory {
		_ = os.Remove(file.Path)
	}
}
