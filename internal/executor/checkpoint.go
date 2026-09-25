package executor

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/frameio"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

func (r ProgramRunner) materializeCheckpointObject(ctx context.Context, artifact workerapi.CheckpointArtifact, suffix, directory string) (string, error) {
	if r.CAS == nil || r.CheckpointEncryptor == nil {
		return "", errors.New("checkpoint storage and encryption are required")
	}
	descriptor := cas.Descriptor{Digest: artifact.Digest, SizeBytes: artifact.SizeBytes, MediaType: artifact.MediaType}
	if err := cas.ValidateDescriptor(descriptor); err != nil {
		return "", err
	}
	if artifact.SizeBytes == math.MaxInt64 {
		return "", errors.New("checkpoint object size overflow")
	}
	body, err := r.CAS.Get(ctx, artifact.Digest)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp(directory, "checkpoint-*."+suffix)
	if err != nil {
		return "", errors.Join(err, body.Close())
	}
	hash := sha256.New()
	limited := &io.LimitedReader{R: body, N: artifact.SizeBytes + 1}
	// Ciphertext framing is larger than plaintext. Bound both the storage stream
	// and filesystem writes before trusting the source's advertised length.
	bounded := &checkpointBoundedWriter{writer: file, remaining: artifact.SizeBytes}
	decryptErr := r.CheckpointEncryptor.Decrypt(ctx, io.TeeReader(limited, hash), bounded, checkpointPurpose(suffix))
	closeErr := errors.Join(body.Close(), file.Close())
	if decryptErr == nil && (limited.N != 1 || sha256sum.DigestHash(hash) != artifact.Digest) {
		decryptErr = errors.New("checkpoint object descriptor mismatch")
	}
	if err := errors.Join(decryptErr, closeErr); err != nil {
		return "", err
	}
	return file.Name(), nil
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

// checkpointSourceReleaseError keeps physical cleanup uncertainty distinct from
// a checkpoint failure that the Control Plane has already acknowledged.
type checkpointSourceReleaseError struct {
	err error
}

func (e *checkpointSourceReleaseError) Error() string {
	return "release checkpoint source: " + e.err.Error()
}

func (e *checkpointSourceReleaseError) Unwrap() error { return e.err }

type runtimeCheckpointer struct {
	publication func(CheckpointRequest) computer.ContinuationPublication
	capacity    *capacity.Ledger
	objects     cas.ImmutableStore
	protocol    *programProtocol
	session     vm.CheckpointableSession
	encryptor   *checkpoint.Encryptor
	tempDir     string
	stream      io.ReadWriteCloser
	workspace   workerapi.CheckpointWorkspaceBase
	runEvent    func(context.Context, *programv0.RunEvent) error
	freezeGate  *sync.Mutex
	onFrozen    func()
}

func (c runtimeCheckpointer) ReleaseCheckpointSource(ctx context.Context) error {
	if releaser, ok := c.session.(CheckpointSourceReleaser); ok {
		return releaser.ReleaseCheckpointSource(ctx)
	}
	return c.session.Close(ctx)
}

func (c runtimeCheckpointer) suspendGuestForCheckpoint(ctx context.Context, request CheckpointRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := wire.WriteCheckpointPauseRequest(c.stream, &programv0.CheckpointPauseRequest{
		Execution: request.Execution, TurnId: request.TurnID, RunId: request.RunID, AttemptNumber: uint32(request.AttemptNumber), RunLeaseId: request.RunLeaseID, RunWaitId: request.RunWaitID, CorrelationId: request.CorrelationID, CheckpointId: request.CheckpointID, ResumeAttachId: request.ResumeAttachID, CheckpointRequestVersion: request.CheckpointRequestVersion,
	}); err != nil {
		return fmt.Errorf("write checkpoint suspend: %w", err)
	}
	if c.protocol != nil {
		if err := c.protocol.takePhysical(ctx, c.runEvent); err != nil {
			return err
		}
	}
	reader := bufio.NewReader(c.stream)
	if c.protocol != nil {
		reader = c.protocol.reader
	}
	pauseCtx, cancel := context.WithTimeout(ctx, checkpointSuspendTimeout)
	err := c.readPauseReadyContext(pauseCtx, reader, request)
	cancel()
	if err != nil {
		return fmt.Errorf("read checkpoint pause ready: %w", err)
	}
	if c.freezeGate != nil {
		c.freezeGate.Lock()
		defer c.freezeGate.Unlock()
	}
	if c.onFrozen != nil {
		c.onFrozen()
	}
	return nil
}

func (c runtimeCheckpointer) readPauseReadyContext(ctx context.Context, reader *bufio.Reader, request CheckpointRequest) error {
	result := make(chan error, 1)
	go func() { result <- c.readPauseReady(ctx, reader, request) }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = c.stream.Close()
		<-result
		return ctx.Err()
	}
}
func (c runtimeCheckpointer) readPauseReady(ctx context.Context, reader *bufio.Reader, request CheckpointRequest) error {
	for {
		prefix, err := reader.Peek(4)
		if err != nil {
			return err
		}
		if frameio.IsStreamFramePrefix(prefix) {
			header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
			if err != nil {
				return err
			}
			if header.Type != wire.StreamTypeCheckpointPauseReady {
				return fmt.Errorf("unsupported checkpoint stream type %q", header.Type)
			}
			if header.RunWaitID != request.RunWaitID || header.CheckpointID != request.CheckpointID {
				return fmt.Errorf("checkpoint pause ready mismatch: run_wait_id=%q checkpoint_id=%q", header.RunWaitID, header.CheckpointID)
			}
			if bodyLen != 0 {
				return fmt.Errorf("checkpoint pause ready body length must be zero, got %d", bodyLen)
			}
			return nil
		}
		body, err := frameio.ReadMessageFrame(reader)
		if err != nil {
			return err
		}
		var event programv0.RunEvent
		if err := proto.Unmarshal(body, &event); err != nil {
			return err
		}
		if c.runEvent == nil {
			return errors.New("received run event while checkpoint pause ready is pending")
		}
		if err := c.runEvent(ctx, &event); err != nil {
			return err
		}
	}
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
