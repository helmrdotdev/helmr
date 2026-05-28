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
	"golang.org/x/sync/errgroup"
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
	copyErr := r.CheckpointEncryptor.Decrypt(ctx, io.TeeReader(body, hash), file, checkpointPurpose(suffix))
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
	return requireCheckpointArtifact(checkpoint.RuntimeState.ConfigArtifact, "runtime_state.config_artifact")
}

func requireCheckpointDigest(field string, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("restore checkpoint %s is required", field)
	}
	return nil
}

func requireCheckpointArtifact(artifact api.WorkerCheckpointArtifact, field string) error {
	if strings.TrimSpace(artifact.Digest) == "" {
		return fmt.Errorf("restore checkpoint %s.digest is required", field)
	}
	if strings.TrimSpace(artifact.MediaType) == "" {
		return fmt.Errorf("restore checkpoint %s.media_type is required", field)
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
	type pauseReadyResult struct {
		ready runv0.PauseReady
		err   error
	}
	result := make(chan pauseReadyResult, 1)
	go func() {
		var parsed runv0.PauseReady
		err := c.readPauseReady(ctx, reader, request, &parsed)
		result <- pauseReadyResult{
			ready: parsed,
			err:   err,
		}
	}()
	select {
	case result := <-result:
		if result.err != nil {
			return result.err
		}
		*ready = result.ready
		return nil
	case <-ctx.Done():
		_ = c.stream.Close()
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

func (c runtimeCheckpointer) storeSnapshotArtifact(ctx context.Context, request CheckpointRequest, artifact vm.SnapshotArtifact) (api.WorkerCheckpointManifest, error) {
	var manifest storedCheckpointArtifact
	var state storedCheckpointArtifact
	var scratchDisk storedCheckpointArtifact
	memory := make([]api.WorkerCheckpointArtifact, len(artifact.Memory))
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
		i := i
		file := file
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
		return api.WorkerCheckpointManifest{}, err
	}
	for _, artifact := range memory {
		if strings.TrimSpace(artifact.Digest) == "" {
			return api.WorkerCheckpointManifest{}, errors.New("stored checkpoint memory artifact is missing digest")
		}
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
			ConfigArtifact:      manifest.artifact,
			VMStateArtifact:     state.artifact,
			ScratchDiskArtifact: scratchDisk.artifact,
			MemoryArtifacts:     memory,
			Config:              artifact.Manifest,
		},
		WorkspaceState: api.WorkerCheckpointWorkspaceState{
			Base: c.workspace,
		},
	}, nil
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
