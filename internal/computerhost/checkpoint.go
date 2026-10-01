package computerhost

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"os"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func materializeCheckpointObject(ctx context.Context, store cas.Reader, encryptor *CheckpointEncryptor, artifact workerapi.CheckpointArtifact, suffix, directory string) (string, error) {
	if store == nil || encryptor == nil {
		return "", errors.New("checkpoint storage and encryption are required")
	}
	descriptor := cas.Descriptor{Digest: artifact.Digest, SizeBytes: artifact.SizeBytes, MediaType: artifact.MediaType}
	if err := cas.ValidateDescriptor(descriptor); err != nil {
		return "", err
	}
	if artifact.SizeBytes == math.MaxInt64 {
		return "", errors.New("checkpoint object size overflow")
	}
	body, err := store.Get(ctx, artifact.Digest)
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
	decryptErr := encryptor.Decrypt(ctx, io.TeeReader(limited, hash), bounded, checkpointPurpose(suffix))
	closeErr := errors.Join(body.Close(), file.Close())
	if decryptErr == nil && (limited.N != 1 || sha256sum.DigestHash(hash) != artifact.Digest) {
		decryptErr = errors.New("checkpoint object descriptor mismatch")
	}
	if err := errors.Join(decryptErr, closeErr); err != nil {
		return "", err
	}
	return file.Name(), nil
}

// SourceReleaseError keeps physical cleanup uncertainty distinct from a
// checkpoint failure that the Control Plane has already acknowledged. Callers
// construct and match it by pointer.
type SourceReleaseError struct {
	Err error
}

func (e *SourceReleaseError) Error() string {
	return "release checkpoint source: " + e.Err.Error()
}

func (e *SourceReleaseError) Unwrap() error { return e.Err }

type computerCheckpointer struct {
	pendingCleanup func() error
	publication    func(computerCheckpointRequest) disk.ContinuationPublication
	reservations   *reservation.Ledger
	objects        cas.ImmutableStore
	machine        vm.CheckpointableMachine
	capture        vm.CheckpointCapture
	// mount is set when machine is served; see ReleaseCheckpointSource.
	mount     *instanceMount
	encryptor *CheckpointEncryptor
	tempDir   string
	computer  workerapi.CheckpointComputerBase
}

// ReleaseCheckpointSource stops the source machine. A served machine is
// released through its mount, which joins the mount's Computer saves before
// closing the machine; an unmounted prepared machine has no save owner and is
// closed directly.
func (c *computerCheckpointer) ReleaseCheckpointSource(ctx context.Context) error {
	var err error
	if c.mount != nil {
		err = c.mount.ReleaseCheckpointSource(ctx)
	} else {
		err = c.machine.Close(ctx)
	}
	if err != nil {
		return err
	}
	return c.cleanupCheckpointStaging()
}

// Serialized by the physical capture owner after snapshot I/O has joined,
// either through guest-control resume or physical exclusion.
func (c *computerCheckpointer) cleanupCheckpointStaging() error {
	if c.pendingCleanup == nil {
		return nil
	}
	if err := c.pendingCleanup(); err != nil {
		return err
	}
	c.pendingCleanup = nil
	return nil
}

func checkpointPurpose(suffix string) string {
	return "helmr.checkpoint." + suffix
}

func workerCheckpointPhases(phases []vm.Phase) []workerapi.CheckpointPhase {
	if len(phases) == 0 {
		return nil
	}
	result := make([]workerapi.CheckpointPhase, 0, len(phases))
	for _, phase := range phases {
		result = append(result, workerCheckpointPhase(phase))
	}
	return result
}

func workerCheckpointPhase(phase vm.Phase) workerapi.CheckpointPhase {
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
