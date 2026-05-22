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
	if checkpoint.RuntimeBackend != "firecracker" {
		return fmt.Errorf("restore checkpoint runtime_backend %q is not supported", checkpoint.RuntimeBackend)
	}
	if checkpoint.RuntimeArch != runtime.GOARCH {
		return fmt.Errorf("restore checkpoint runtime_arch %q does not match worker arch %q", checkpoint.RuntimeArch, runtime.GOARCH)
	}
	if strings.TrimSpace(checkpoint.RuntimeABI) == "" {
		return errors.New("restore checkpoint runtime_abi is required")
	}
	if err := requireCheckpointDigest("kernel_digest", checkpoint.KernelDigest); err != nil {
		return err
	}
	if err := requireCheckpointDigest("rootfs_digest", checkpoint.RootfsDigest); err != nil {
		return err
	}
	if err := requireCheckpointDigest("runtime_config_digest", checkpoint.RuntimeConfigDigest); err != nil {
		return err
	}
	return nil
}

func requireCheckpointDigest(field string, value *string) error {
	if value == nil || strings.TrimSpace(*value) == "" {
		return fmt.Errorf("restore checkpoint %s is required", field)
	}
	return nil
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

type runtimeCheckpointer struct {
	session   vm.CheckpointableSession
	cas       cas.Store
	encryptor *checkpoint.Encryptor
	tempDir   string
	stream    io.ReadWriteCloser
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
	defer cleanupSnapshotArtifact(artifact)
	manifest, err := c.storeSnapshotArtifact(ctx, artifact)
	if err != nil {
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
	state, err := c.storeSnapshotFile(ctx, artifact.VMState, "vmstate")
	if err != nil {
		return api.WorkerCheckpointManifest{}, fmt.Errorf("store checkpoint vm state: %w", err)
	}
	objects := []api.CASObject{apiCASObject(state)}
	memoryDigests := make([]string, 0, len(artifact.Memory))
	for _, file := range artifact.Memory {
		stored, err := c.storeSnapshotFile(ctx, file, "memory")
		if err != nil {
			return api.WorkerCheckpointManifest{}, fmt.Errorf("store checkpoint memory: %w", err)
		}
		memoryDigests = append(memoryDigests, stored.Digest)
		objects = append(objects, apiCASObject(stored))
	}
	return api.WorkerCheckpointManifest{
		RuntimeBackend:      artifact.RuntimeBackend,
		RuntimeArch:         artifact.RuntimeArch,
		RuntimeABI:          artifact.RuntimeABI,
		KernelDigest:        optionalString(artifact.KernelDigest),
		RootfsDigest:        optionalString(artifact.RootfsDigest),
		RuntimeConfigDigest: optionalString(artifact.RuntimeConfigDigest),
		VMStateDigest:       optionalString(state.Digest),
		MemoryDigests:       memoryDigests,
		CASObjects:          objects,
		Manifest:            artifact.Manifest,
	}, nil
}

func apiCASObject(object cas.Object) api.CASObject {
	return api.CASObject{
		Digest:    object.Digest,
		SizeBytes: object.SizeBytes,
		MediaType: object.MediaType,
	}
}

func (c runtimeCheckpointer) storeSnapshotFile(ctx context.Context, file vm.SnapshotFile, suffix string) (cas.Object, error) {
	encrypted, cleanup, err := c.encryptSnapshotFile(ctx, file, suffix)
	if err != nil {
		return cas.Object{}, err
	}
	defer cleanup()
	body, err := os.Open(encrypted)
	if err != nil {
		return cas.Object{}, err
	}
	defer body.Close()
	return c.cas.Put(ctx, file.MediaType, body)
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

func cleanupSnapshotArtifact(artifact vm.SnapshotArtifact) {
	_ = os.Remove(artifact.VMState.Path)
	for _, file := range artifact.Memory {
		_ = os.Remove(file.Path)
	}
}

func optionalString(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}
