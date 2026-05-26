package executor

import (
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
	if checkpoint.Runtime.Backend != "firecracker" {
		return fmt.Errorf("restore checkpoint runtime.backend %q is not supported", checkpoint.Runtime.Backend)
	}
	if checkpoint.Runtime.Arch != runtime.GOARCH {
		return fmt.Errorf("restore checkpoint runtime.arch %q does not match worker arch %q", checkpoint.Runtime.Arch, runtime.GOARCH)
	}
	if strings.TrimSpace(checkpoint.Runtime.ABI) == "" {
		return errors.New("restore checkpoint runtime.abi is required")
	}
	if err := requireCheckpointDigest("runtime.kernel_digest", checkpoint.Runtime.KernelDigest); err != nil {
		return err
	}
	if err := requireCheckpointDigest("runtime.rootfs_digest", checkpoint.Runtime.RootfsDigest); err != nil {
		return err
	}
	if err := requireCheckpointDigest("runtime.config_digest", checkpoint.Runtime.ConfigDigest); err != nil {
		return err
	}
	if err := requireCheckpointDigest("runtime_state.manifest.digest", checkpoint.RuntimeState.Manifest.Digest); err != nil {
		return err
	}
	return nil
}

func requireCheckpointDigest(field string, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("restore checkpoint %s is required", field)
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
	if err := c.suspendGuestForCheckpoint(ctx, request); err != nil {
		return api.WorkerCheckpointManifest{}, err
	}
	if err := c.stream.Close(); err != nil {
		_ = c.session.Resume(ctx)
		return api.WorkerCheckpointManifest{}, fmt.Errorf("close checkpoint control stream: %w", err)
	}
	artifact, err := c.session.CreateSnapshot(ctx, vm.SnapshotRequest{ID: request.CheckpointID})
	if err != nil {
		_ = c.session.Resume(ctx)
		return api.WorkerCheckpointManifest{}, err
	}
	cleanupScratchDisk := true
	defer func() {
		cleanupSnapshotArtifact(artifact, cleanupScratchDisk)
	}()
	manifest, err := c.storeSnapshotArtifact(ctx, artifact)
	if err != nil {
		cleanupScratchDisk = false
		_ = c.session.Resume(ctx)
		return api.WorkerCheckpointManifest{}, err
	}
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
	pauseCtx, cancelPause := context.WithTimeout(ctx, checkpointSuspendTimeout)
	err := readProtoFrameContext(pauseCtx, c.session, &ready)
	cancelPause()
	if err != nil {
		return fmt.Errorf("read checkpoint pause ready: %w", err)
	}
	if ready.WaitpointId != request.WaitpointID || ready.CheckpointId != request.CheckpointID {
		return fmt.Errorf("checkpoint pause ready mismatch: waitpoint_id=%q checkpoint_id=%q", ready.WaitpointId, ready.CheckpointId)
	}
	return nil
}

func (c runtimeCheckpointer) storeSnapshotArtifact(ctx context.Context, artifact vm.SnapshotArtifact) (api.WorkerCheckpointManifest, error) {
	manifest, err := c.storeSnapshotBytes(ctx, artifact.Manifest, "manifest", cas.CheckpointManifestMediaType)
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
	return api.WorkerCheckpointManifest{
		Runtime: api.WorkerCheckpointRuntime{
			Backend:      artifact.RuntimeBackend,
			Arch:         artifact.RuntimeArch,
			ABI:          artifact.RuntimeABI,
			KernelDigest: artifact.KernelDigest,
			RootfsDigest: artifact.RootfsDigest,
			ConfigDigest: artifact.RuntimeConfigDigest,
		},
		RuntimeState: api.WorkerCheckpointRuntimeState{
			Manifest: manifest.artifact,
			VMState:  state.artifact,
			Memory:   memory,
		},
		Workspace: api.WorkerCheckpointWorkspace{
			Base:    c.workspace,
			Scratch: &scratchDisk.artifact,
		},
		RuntimeManifest: artifact.Manifest,
	}, nil
}

type storedCheckpointArtifact struct {
	artifact api.WorkerCheckpointArtifact
}

func (c runtimeCheckpointer) storeSnapshotBytes(ctx context.Context, bytes []byte, suffix string, mediaType string) (storedCheckpointArtifact, error) {
	tempDir := c.tempDir
	if strings.TrimSpace(tempDir) == "" {
		tempDir = os.TempDir()
	}
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return storedCheckpointArtifact{}, err
	}
	file, err := os.CreateTemp(tempDir, "helmr-checkpoint-*."+suffix)
	if err != nil {
		return storedCheckpointArtifact{}, err
	}
	path := file.Name()
	defer os.Remove(path)
	if _, err := file.Write(bytes); err != nil {
		_ = file.Close()
		return storedCheckpointArtifact{}, err
	}
	if err := file.Close(); err != nil {
		return storedCheckpointArtifact{}, err
	}
	return c.storeSnapshotFile(ctx, vm.SnapshotFile{Path: path, MediaType: mediaType}, suffix)
}

func (c runtimeCheckpointer) storeSnapshotFile(ctx context.Context, file vm.SnapshotFile, suffix string) (storedCheckpointArtifact, error) {
	encryptStarted := time.Now()
	encrypted, cleanup, err := c.encryptSnapshotFile(ctx, file, suffix)
	if err != nil {
		return storedCheckpointArtifact{}, err
	}
	defer cleanup()
	encryptDuration := time.Since(encryptStarted)
	body, err := os.Open(encrypted)
	if err != nil {
		return storedCheckpointArtifact{}, err
	}
	defer body.Close()
	storeStarted := time.Now()
	object, err := c.cas.Put(ctx, file.MediaType, body)
	if err != nil {
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

func (c runtimeCheckpointer) encryptSnapshotFile(ctx context.Context, file vm.SnapshotFile, suffix string) (string, func(), error) {
	body, err := os.Open(file.Path)
	if err != nil {
		return "", nil, err
	}
	defer body.Close()
	tempDir := c.tempDir
	if strings.TrimSpace(tempDir) == "" {
		tempDir = os.TempDir()
	}
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return "", nil, err
	}
	encrypted, err := os.CreateTemp(tempDir, "helmr-checkpoint-*."+suffix+".enc")
	if err != nil {
		return "", nil, err
	}
	path := encrypted.Name()
	cleanup := func() { _ = os.Remove(path) }
	encryptErr := c.encryptor.Encrypt(ctx, body, encrypted, checkpointPurpose(suffix))
	closeErr := encrypted.Close()
	if encryptErr != nil {
		cleanup()
		return "", nil, encryptErr
	}
	if closeErr != nil {
		cleanup()
		return "", nil, closeErr
	}
	return path, cleanup, nil
}

func checkpointPurpose(suffix string) string {
	return "helmr.checkpoint." + suffix
}

func cleanupSnapshotArtifact(artifact vm.SnapshotArtifact, cleanupScratchDisk bool) {
	_ = os.Remove(artifact.VMState.Path)
	if cleanupScratchDisk {
		_ = os.Remove(artifact.ScratchDisk.Path)
	}
	for _, file := range artifact.Memory {
		_ = os.Remove(file.Path)
	}
}
