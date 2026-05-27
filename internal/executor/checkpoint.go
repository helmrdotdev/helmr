package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/transport"
	"github.com/helmrdotdev/helmr/internal/vm"
	"google.golang.org/protobuf/proto"
)

func (r GuestRunner) materializeCheckpointObject(ctx context.Context, digest string, suffix string) (string, error) {
	if r.CheckpointEncryptor == nil {
		return "", errors.New("checkpoint encryption is required")
	}
	body, err := r.CAS.Get(ctx, digest)
	if err != nil {
		return "", fmt.Errorf("get checkpoint object %s: %w", digest, err)
	}
	defer body.Close()
	if err := os.MkdirAll(r.tempDir(), 0o755); err != nil {
		return "", fmt.Errorf("create checkpoint temp dir: %w", err)
	}
	file, err := os.CreateTemp(r.tempDir(), "checkpoint-*."+suffix)
	if err != nil {
		return "", fmt.Errorf("create checkpoint temp file: %w", err)
	}
	path := file.Name()
	hash := sha256.New()
	copyErr := r.CheckpointEncryptor.Decrypt(ctx, io.TeeReader(body, hash), file, checkpointPurpose(suffix))
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("decrypt checkpoint object %s: %w", digest, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close checkpoint object %s: %w", digest, closeErr)
	}
	actual := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	if actual != digest {
		_ = os.Remove(path)
		return "", fmt.Errorf("checkpoint object digest mismatch: expected %s, got %s", digest, actual)
	}
	return path, nil
}

func validateRestoreIdentity(checkpoint api.WorkerCheckpointManifest) error {
	runtimeInfo := checkpoint.RecoveryPoint.Runtime
	if runtimeInfo.Backend != "firecracker" {
		return fmt.Errorf("restore checkpoint recovery_point.runtime.backend %q is not supported", runtimeInfo.Backend)
	}
	if runtimeInfo.Arch != runtime.GOARCH {
		return fmt.Errorf("restore checkpoint recovery_point.runtime.arch %q does not match worker arch %q", runtimeInfo.Arch, runtime.GOARCH)
	}
	if strings.TrimSpace(runtimeInfo.ABI) == "" {
		return errors.New("restore checkpoint recovery_point.runtime.abi is required")
	}
	if err := requireCheckpointDigest("recovery_point.runtime.kernel_digest", runtimeInfo.KernelDigest); err != nil {
		return err
	}
	if err := requireCheckpointDigest("recovery_point.runtime.rootfs_digest", runtimeInfo.RootfsDigest); err != nil {
		return err
	}
	if err := requireCheckpointDigest("recovery_point.runtime.config_digest", runtimeInfo.ConfigDigest); err != nil {
		return err
	}
	return requireAvailableCheckpointArtifact(checkpoint, checkpoint.RuntimeState.ConfigArtifactID, "runtime_state.config_artifact_id")
}

func requireCheckpointDigest(field string, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("restore checkpoint %s is required", field)
	}
	return nil
}

func checkpointArtifactByID(checkpoint api.WorkerCheckpointManifest, artifactID string) (api.WorkerCheckpointArtifact, bool) {
	for _, node := range checkpoint.ArtifactGraph.Artifacts {
		if node.ID == artifactID {
			return node.Artifact, true
		}
	}
	return api.WorkerCheckpointArtifact{}, false
}

func checkpointArtifactAvailable(checkpoint api.WorkerCheckpointManifest, artifactID string) bool {
	for _, available := range checkpoint.Availability.Artifacts {
		if available.ArtifactID == artifactID {
			return available.Status == api.WorkerCheckpointArtifactAvailable
		}
	}
	return false
}

func requireAvailableCheckpointArtifact(checkpoint api.WorkerCheckpointManifest, artifactID string, field string) error {
	if strings.TrimSpace(artifactID) == "" {
		return fmt.Errorf("restore checkpoint %s is required", field)
	}
	artifact, ok := checkpointArtifactByID(checkpoint, artifactID)
	if !ok {
		return fmt.Errorf("restore checkpoint %s references missing artifact %q", field, artifactID)
	}
	if !checkpointArtifactAvailable(checkpoint, artifactID) {
		return fmt.Errorf("restore checkpoint %s artifact %q is not available", field, artifactID)
	}
	if strings.TrimSpace(artifact.Digest) == "" {
		return fmt.Errorf("restore checkpoint artifact %q digest is required", artifactID)
	}
	return nil
}

type runtimeCheckpointer struct {
	session   vm.CheckpointableSession
	cas       cas.Store
	encryptor *checkpoint.Encryptor
	tempDir   string
	stream    io.ReadWriteCloser
	workspace api.WorkerCheckpointWorkspaceBase
	runEvent  func(context.Context, *runv0.RunEvent) error
}

func (c runtimeCheckpointer) CreateCheckpoint(ctx context.Context, request CheckpointRequest) (api.WorkerCheckpointManifest, error) {
	if c.cas == nil {
		return api.WorkerCheckpointManifest{}, errors.New("checkpoint CAS is required")
	}
	if c.encryptor == nil {
		return api.WorkerCheckpointManifest{}, errors.New("checkpoint encryption is required")
	}
	if c.stream == nil {
		return api.WorkerCheckpointManifest{}, errors.New("checkpoint control stream is required")
	}
	phases := []api.WorkerCheckpointPhase{}
	recordPhase := func(name string, started time.Time) {
		phases = append(phases, api.WorkerCheckpointPhase{Name: name, DurationMs: durationMilliseconds(time.Since(started))})
	}
	started := time.Now()
	if err := c.suspendGuestForCheckpoint(ctx, request); err != nil {
		return api.WorkerCheckpointManifest{}, err
	}
	recordPhase("suspend_guest", started)
	started = time.Now()
	if err := c.stream.Close(); err != nil {
		_ = c.session.Resume(ctx)
		return api.WorkerCheckpointManifest{}, fmt.Errorf("close checkpoint control stream: %w", err)
	}
	recordPhase("close_control_stream", started)
	started = time.Now()
	artifact, err := c.session.CreateSnapshot(ctx, vm.SnapshotRequest{ID: request.CheckpointID})
	if err != nil {
		_ = c.session.Resume(ctx)
		return api.WorkerCheckpointManifest{}, err
	}
	recordPhase("create_runtime_snapshot", started)
	defer func() {
		cleanupSnapshotArtifact(artifact)
	}()
	started = time.Now()
	manifest, err := c.storeSnapshotArtifact(ctx, request, artifact)
	if err != nil {
		_ = c.session.Resume(ctx)
		return api.WorkerCheckpointManifest{}, err
	}
	recordPhase("store_checkpoint_artifacts", started)
	manifest.Phases = phases
	return manifest, nil
}

func (c runtimeCheckpointer) suspendGuestForCheckpoint(ctx context.Context, request CheckpointRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := transport.WriteProtoFrame(c.stream, &runv0.SuspendForCheckpoint{
		WaitpointId:  request.WaitpointID,
		CheckpointId: request.CheckpointID,
	}); err != nil {
		return fmt.Errorf("write checkpoint suspend: %w", err)
	}
	var ready runv0.PauseReady
	reader := bufio.NewReader(c.stream)
	pauseCtx, cancelPause := context.WithTimeout(ctx, checkpointSuspendTimeout)
	err := c.readPauseReadyContext(pauseCtx, reader, request, &ready)
	cancelPause()
	if err != nil {
		return fmt.Errorf("read checkpoint pause ready: %w", err)
	}
	if ready.WaitpointId != request.WaitpointID || ready.CheckpointId != request.CheckpointID {
		return fmt.Errorf("checkpoint pause ready mismatch: waitpoint_id=%q checkpoint_id=%q", ready.WaitpointId, ready.CheckpointId)
	}
	return nil
}

func (c runtimeCheckpointer) readPauseReadyContext(ctx context.Context, reader *bufio.Reader, request CheckpointRequest, ready *runv0.PauseReady) error {
	result := make(chan error, 1)
	go func() {
		result <- c.readPauseReady(ctx, reader, request, ready)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = c.session.Close()
		return ctx.Err()
	}
}

func (c runtimeCheckpointer) readPauseReady(ctx context.Context, reader *bufio.Reader, request CheckpointRequest, ready *runv0.PauseReady) error {
	for {
		prefix, err := reader.Peek(4)
		if err != nil {
			return err
		}
		if transport.IsStreamFramePrefix(prefix) {
			header, bodyLen, err := transport.ReadStreamFrameHeader(reader)
			if err != nil {
				return err
			}
			if header.Type != transport.StreamTypeCheckpointPauseReady {
				return fmt.Errorf("unsupported checkpoint stream type %q", header.Type)
			}
			if bodyLen != 0 {
				return fmt.Errorf("checkpoint pause ready body length %d must be zero", bodyLen)
			}
			if header.WaitpointID != request.WaitpointID || header.CheckpointID != request.CheckpointID {
				return fmt.Errorf("checkpoint pause ready mismatch: waitpoint_id=%q checkpoint_id=%q", header.WaitpointID, header.CheckpointID)
			}
			ready.WaitpointId = header.WaitpointID
			ready.CheckpointId = header.CheckpointID
			return nil
		}
		body, err := transport.ReadMessageFrame(reader)
		if err != nil {
			return err
		}
		var event runv0.RunEvent
		if err := proto.Unmarshal(body, &event); err != nil {
			return fmt.Errorf("unmarshal checkpoint interleaved run event: %w", err)
		}
		if c.runEvent == nil {
			return errors.New("received run event while checkpoint pause ready is pending")
		}
		if err := c.runEvent(ctx, &event); err != nil {
			return err
		}
	}
}

const (
	runtimeConfigArtifactID  = "runtime.config"
	runtimeVMStateArtifactID = "runtime.vm_state"
	runtimeScratchArtifactID = "runtime.scratch_disk"
)

func runtimeMemoryArtifactID(ordinal int) string {
	return fmt.Sprintf("runtime.memory.%d", ordinal)
}

func (c runtimeCheckpointer) storeSnapshotArtifact(ctx context.Context, request CheckpointRequest, artifact vm.SnapshotArtifact) (api.WorkerCheckpointManifest, error) {
	manifest, err := c.storeSnapshotReader(ctx, bytes.NewReader(artifact.Manifest), cas.CheckpointRuntimeConfigMediaType, "manifest")
	if err != nil {
		return api.WorkerCheckpointManifest{}, fmt.Errorf("store checkpoint manifest: %w", err)
	}
	state, err := c.storeSnapshotFile(ctx, artifact.VMState, "vmstate")
	if err != nil {
		return api.WorkerCheckpointManifest{}, fmt.Errorf("store checkpoint vm state: %w", err)
	}
	scratchDisk, err := c.storeSnapshotFile(ctx, artifact.ScratchDisk, "scratch-disk")
	if err != nil {
		return api.WorkerCheckpointManifest{}, fmt.Errorf("store checkpoint scratch disk: %w", err)
	}
	memory := make([]api.WorkerCheckpointArtifact, 0, len(artifact.Memory))
	for _, file := range artifact.Memory {
		stored, err := c.storeSnapshotFile(ctx, file, "memory")
		if err != nil {
			return api.WorkerCheckpointManifest{}, fmt.Errorf("store checkpoint memory: %w", err)
		}
		memory = append(memory, stored.artifact)
	}
	memoryArtifactIDs := make([]string, 0, len(memory))
	artifactGraph := []api.WorkerCheckpointArtifactNode{
		checkpointArtifactNode(runtimeConfigArtifactID, api.WorkerCheckpointArtifactRoleRuntimeConfig, manifest.artifact),
		checkpointArtifactNode(runtimeVMStateArtifactID, api.WorkerCheckpointArtifactRoleRuntimeVMState, state.artifact),
		checkpointArtifactNode(runtimeScratchArtifactID, api.WorkerCheckpointArtifactRoleRuntimeScratch, scratchDisk.artifact),
	}
	for i, artifact := range memory {
		artifactID := runtimeMemoryArtifactID(i)
		memoryArtifactIDs = append(memoryArtifactIDs, artifactID)
		artifactGraph = append(artifactGraph, checkpointArtifactNode(artifactID, api.WorkerCheckpointArtifactRoleRuntimeMemory, artifact))
	}
	return api.WorkerCheckpointManifest{
		RecoveryPoint: api.WorkerCheckpointRecoveryPoint{
			ID:          request.CheckpointID,
			RunID:       request.RunID,
			WaitpointID: request.WaitpointID,
			Runtime: api.WorkerCheckpointRuntime{
				Backend:      artifact.RuntimeBackend,
				Arch:         artifact.RuntimeArch,
				ABI:          artifact.RuntimeABI,
				KernelDigest: artifact.KernelDigest,
				RootfsDigest: artifact.RootfsDigest,
				ConfigDigest: artifact.RuntimeConfigDigest,
			},
		},
		RuntimeState: api.WorkerCheckpointRuntimeState{
			ConfigArtifactID:      runtimeConfigArtifactID,
			VMStateArtifactID:     runtimeVMStateArtifactID,
			ScratchDiskArtifactID: runtimeScratchArtifactID,
			MemoryArtifactIDs:     memoryArtifactIDs,
			Config:                artifact.Manifest,
		},
		WorkspaceState: api.WorkerCheckpointWorkspaceState{
			Base: c.workspace,
		},
		ArtifactGraph: api.WorkerCheckpointArtifactGraph{Artifacts: artifactGraph},
		Availability:  checkpointAvailability(artifactGraph),
	}, nil
}

func checkpointArtifactNode(id string, role api.WorkerCheckpointArtifactRole, artifact api.WorkerCheckpointArtifact) api.WorkerCheckpointArtifactNode {
	return api.WorkerCheckpointArtifactNode{
		ID:       id,
		Role:     role,
		Artifact: artifact,
	}
}

func checkpointAvailability(nodes []api.WorkerCheckpointArtifactNode) api.WorkerCheckpointAvailability {
	availability := api.WorkerCheckpointAvailability{Artifacts: make([]api.WorkerCheckpointArtifactAvailability, 0, len(nodes))}
	for _, node := range nodes {
		availability.Artifacts = append(availability.Artifacts, api.WorkerCheckpointArtifactAvailability{
			ArtifactID: node.ID,
			Status:     api.WorkerCheckpointArtifactAvailable,
		})
	}
	return availability
}

type storedCheckpointArtifact struct {
	artifact api.WorkerCheckpointArtifact
}

func (c runtimeCheckpointer) storeSnapshotFile(ctx context.Context, file vm.SnapshotFile, suffix string) (storedCheckpointArtifact, error) {
	body, err := os.Open(file.Path)
	if err != nil {
		return storedCheckpointArtifact{}, err
	}
	defer body.Close()
	return c.storeSnapshotReader(ctx, body, file.MediaType, suffix)
}

func (c runtimeCheckpointer) storeSnapshotReader(ctx context.Context, body io.Reader, mediaType string, suffix string) (storedCheckpointArtifact, error) {
	encryptStarted := time.Now()
	stage, err := c.cas.Stage(ctx, mediaType)
	if err != nil {
		return storedCheckpointArtifact{}, err
	}
	if err := c.encryptor.Encrypt(ctx, body, stage, checkpointPurpose(suffix)); err != nil {
		_ = stage.Abort(context.Background())
		return storedCheckpointArtifact{}, err
	}
	encryptDuration := time.Since(encryptStarted)
	storeStarted := time.Now()
	object, err := stage.Commit(ctx)
	if err != nil {
		_ = stage.Abort(context.Background())
		return storedCheckpointArtifact{}, err
	}
	return storedCheckpointArtifact{artifact: api.WorkerCheckpointArtifact{
		Digest:            object.Digest,
		SizeBytes:         object.SizeBytes,
		MediaType:         object.MediaType,
		EncryptDurationMs: durationMilliseconds(encryptDuration),
		StoreDurationMs:   durationMilliseconds(time.Since(storeStarted)),
	}}, nil
}

func checkpointPurpose(suffix string) string {
	return "helmr.checkpoint." + suffix
}

func cleanupSnapshotArtifact(artifact vm.SnapshotArtifact) {
	_ = os.Remove(artifact.VMState.Path)
	_ = os.Remove(artifact.ScratchDisk.Path)
	for _, file := range artifact.Memory {
		_ = os.Remove(file.Path)
	}
}
