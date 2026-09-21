package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/filepack"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type checkpointStagingLimits struct {
	total   int64
	memory  int64
	scratch int64
	state   int64
	config  int64
}

func checkpointStagingSize(shape vm.SnapshotLimits, cipher *checkpoint.Encryptor) (checkpointStagingLimits, error) {
	if shape.ComputerBytes <= 0 || shape.MemoryBytes <= 0 || shape.ScratchBytes <= 0 || shape.StateBytes <= 0 || shape.ConfigBytes <= 0 {
		return checkpointStagingLimits{}, errors.New("incomplete checkpoint capture limits")
	}
	memory, err := filepack.PackedSizeLimit(shape.MemoryBytes, filepack.MemoryRole)
	if err != nil {
		return checkpointStagingLimits{}, err
	}
	scratch, err := filepack.PackedSizeLimit(shape.ScratchBytes, filepack.ScratchRole)
	if err != nil {
		return checkpointStagingLimits{}, err
	}
	disk, err := (computer.DiskStore{Cipher: cipher}).CaptureSizeLimit(shape.ComputerBytes)
	if err != nil {
		return checkpointStagingLimits{}, err
	}
	limits := checkpointStagingLimits{memory: memory, scratch: scratch, state: shape.StateBytes, config: shape.ConfigBytes}
	// Working Computer and scratch are already charged to the runtime. Raw RAM,
	// raw state, packed intermediates and all ciphertexts may coexist here.
	sizes := []int64{shape.MemoryBytes, shape.StateBytes, memory, scratch, disk}
	for _, n := range []int64{memory, scratch, shape.StateBytes, shape.ConfigBytes} {
		encoded, err := cipher.EncryptedSize(n)
		if err != nil {
			return checkpointStagingLimits{}, err
		}
		sizes = append(sizes, encoded)
	}
	for _, n := range sizes {
		if n > math.MaxInt64-limits.total {
			return checkpointStagingLimits{}, capacity.ErrOverflow
		}
		limits.total += n
	}
	return limits, nil
}

func (c runtimeCheckpointer) CreateCheckpoint(ctx context.Context, request CheckpointRequest) (result CheckpointResult, retErr error) {
	if c.session == nil {
		return result, errors.New("checkpoint source session is required")
	}
	var key capacity.Key
	var reserved bool
	var otherOwner bool
	var directory string
	var artifact vm.SnapshotArtifact
	var disk *computer.DiskCandidate
	var candidates []*checkpointCandidate
	defer func() {
		if otherOwner {
			return
		}
		stopSource := func() error {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			return c.ReleaseCheckpointSource(cleanupCtx)
		}
		var stopErr error
		if retErr != nil {
			stopErr = stopSource()
		}
		// All encoders and uploads have joined. These ciphertext files have no VMM
		// writer, so close and reclaim them even when source shutdown is uncertain.
		var cleanupErr error
		if disk != nil {
			cleanupErr = errors.Join(cleanupErr, disk.Close())
		}
		for _, candidate := range candidates {
			cleanupErr = errors.Join(cleanupErr, candidate.close())
		}
		if directory != "" {
			cleanupErr = errors.Join(cleanupErr, os.RemoveAll(directory))
		}
		// Raw VM output is different: a failed capture response can leave a writer.
		// Keep its runtime-owned paths and full charge until exit is confirmed.
		if stopErr == nil {
			cleanupErr = errors.Join(cleanupErr, removeCheckpointSnapshot(artifact))
		}
		if cleanupErr != nil && retErr == nil {
			stopErr = stopSource()
		}
		if cleanupErr == nil && stopErr == nil && reserved {
			cleanupErr = c.capacity.Release(key)
		}
		if err := errors.Join(cleanupErr, stopErr); err != nil {
			retErr = errors.Join(retErr, &checkpointSourceReleaseError{err: err})
		}
	}()

	if c.capacity == nil || c.objects == nil || c.encryptor == nil || c.stream == nil || request.Register == nil {
		return result, errors.New("checkpoint capacity, immutable storage, encryption, stream and registration are required")
	}
	shape, err := c.session.SnapshotLimits()
	if err != nil {
		return result, err
	}
	limits, err := checkpointStagingSize(shape, c.encryptor)
	if err != nil {
		return result, err
	}
	key = capacity.Key{Kind: "checkpoint-staging", ID: request.CheckpointID, Epoch: request.CheckpointRequestVersion}
	reserved, err = c.capacity.Reserve(key, capacity.Vector{GuestEphemeralDiskBytes: limits.total})
	if err != nil {
		if errors.Is(err, capacity.ErrDuplicateReservation) {
			otherOwner = true
		}
		return result, err
	}
	if !reserved {
		otherOwner = true
		return result, errors.New("checkpoint staging is already owned")
	}
	if err := os.MkdirAll(c.tempDir, 0700); err != nil {
		return result, err
	}
	directory, err = os.MkdirTemp(c.tempDir, "checkpoint-")
	if err != nil {
		return result, err
	}
	started := time.Now()
	if err := c.suspendGuestForCheckpoint(ctx, request); err != nil {
		return result, err
	}
	if err := c.stream.Close(); err != nil {
		return result, fmt.Errorf("close checkpoint control stream: %w", err)
	}
	artifact, err = c.session.CreateSnapshot(ctx, vm.SnapshotRequest{ID: request.CheckpointID})
	if err != nil {
		return result, err
	}
	if artifact.Computer == nil || artifact.Computer.ComputerID == "" || artifact.Computer.SizeBytes != shape.ComputerBytes || len(artifact.Memory) != 1 || artifact.VMVCPUCount <= 0 || !sha256sum.ValidDigest(artifact.CPUConfigDigest) {
		return result, errors.New("incomplete or changed paired checkpoint snapshot")
	}
	source, err := os.Stat(artifact.Computer.Path)
	if err != nil {
		return result, err
	}
	if !source.Mode().IsRegular() || source.Size() != shape.ComputerBytes {
		return result, errors.New("checkpoint Computer source size changed")
	}
	disk, err = (computer.DiskStore{Cipher: c.encryptor}).Capture(ctx, artifact.Computer.ComputerID, artifact.Computer.Path, directory)
	if err != nil {
		return result, err
	}
	if err := disk.Artifact().Validate(shape.ComputerBytes); err != nil {
		return result, err
	}
	inputs := []struct {
		file   vm.SnapshotFile
		body   []byte
		suffix string
		limit  int64
	}{
		{body: artifact.Manifest, file: vm.SnapshotFile{MediaType: cas.CheckpointRuntimeConfigMediaType}, suffix: "manifest", limit: limits.config},
		{file: artifact.VMState, suffix: "vmstate", limit: limits.state},
		{file: artifact.ScratchDisk, suffix: "scratch-disk", limit: limits.scratch},
		{file: artifact.Memory[0], suffix: "memory", limit: limits.memory},
	}
	for _, input := range inputs {
		var body io.Reader = bytes.NewReader(input.body)
		var opened *os.File
		if input.file.Path != "" {
			opened, err = os.Open(input.file.Path)
			if err != nil {
				return result, err
			}
			body = opened
		}
		candidate, stageErr := stageCheckpoint(ctx, directory, c.encryptor, body, input.file.MediaType, input.suffix, input.limit)
		if candidate != nil {
			candidates = append(candidates, candidate)
		}
		if opened != nil {
			stageErr = errors.Join(stageErr, opened.Close())
		}
		if stageErr != nil {
			return result, stageErr
		}
	}
	result.Manifest = c.checkpointManifest(request, artifact, disk.Artifact(), candidates)
	result.Manifest.Phases = append(workerCheckpointPhases(artifact.Phases), workerapi.CheckpointPhase{Name: "capture_checkpoint", DurationMs: durationMilliseconds(time.Since(started))})
	// The registered descriptors stay fixed through uncertain replies and retries.
	if err := request.Register(ctx, result.Manifest); err != nil {
		return result, err
	}
	if err := retryCheckpointUpload(ctx, func() error { return disk.Upload(ctx, c.objects) }); err != nil {
		return result, err
	}
	for _, candidate := range candidates {
		if err := retryCheckpointUpload(ctx, func() error { return candidate.upload(ctx, c.objects) }); err != nil {
			return result, err
		}
	}
	return result, nil
}

func retryCheckpointUpload(ctx context.Context, upload func() error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := upload()
		if err == nil {
			return nil
		}
		// Permanent local/format errors are not recovered by retrying storage.
		var temporary interface{ Temporary() bool }
		var status interface{ HTTPStatusCode() int }
		retry := (errors.As(err, &temporary) && temporary.Temporary()) || (errors.As(err, &status) && status.HTTPStatusCode() >= 500)
		if !retry {
			return err
		}
		if err := sleepWithContext(ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}
}

type checkpointCandidate struct {
	file       *os.File
	descriptor cas.Descriptor
}

func (c *checkpointCandidate) close() error { return c.file.Close() }
func (c *checkpointCandidate) upload(ctx context.Context, store cas.ImmutableStore) error {
	object, err := store.Publish(ctx, c.descriptor, c.file)
	if err != nil {
		return err
	}
	if object.Digest != c.descriptor.Digest || object.SizeBytes != c.descriptor.SizeBytes || object.MediaType != c.descriptor.MediaType {
		return errors.New("checkpoint upload descriptor mismatch")
	}
	return nil
}

type checkpointBoundedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *checkpointBoundedWriter) Write(b []byte) (int, error) {
	if int64(len(b)) > w.remaining {
		return 0, errors.New("checkpoint ciphertext exceeds reserved bound")
	}
	n, err := w.writer.Write(b)
	w.remaining -= int64(n)
	return n, err
}
func stageCheckpoint(ctx context.Context, directory string, cipher *checkpoint.Encryptor, body io.Reader, mediaType, suffix string, limit int64) (*checkpointCandidate, error) {
	encoded, err := cipher.EncryptedSize(limit)
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(directory, suffix+"-")
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	bounded := &checkpointBoundedWriter{writer: io.MultiWriter(file, hash), remaining: encoded}
	// Read the extra byte so a changed source cannot be silently truncated.
	source := &io.LimitedReader{R: body, N: limit + 1}
	err = cipher.Encrypt(ctx, source, bounded, checkpointPurpose(suffix))
	if err == nil && source.N == 0 {
		err = errors.New("checkpoint source exceeds declared limit")
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if err = file.Chmod(0400); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	readonly, err := os.Open(file.Name())
	closeErr := file.Close()
	if err != nil {
		return nil, errors.Join(err, closeErr)
	}
	if closeErr != nil {
		return nil, errors.Join(closeErr, readonly.Close())
	}
	return &checkpointCandidate{file: readonly, descriptor: cas.Descriptor{Digest: sha256sum.FormatDigest(hash.Sum(nil)), SizeBytes: encoded - bounded.remaining, MediaType: mediaType}}, nil
}

func removeCheckpointSnapshot(artifact vm.SnapshotArtifact) error {
	paths := []string{artifact.VMState.Path, artifact.ScratchDisk.Path}
	for _, file := range artifact.Memory {
		paths = append(paths, file.Path)
	}
	var result error
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, fmt.Errorf("remove checkpoint staging: %w", err))
		}
	}
	return result
}

func checkpointDescriptor(d cas.Descriptor) workerapi.CheckpointArtifact {
	return workerapi.CheckpointArtifact{Digest: d.Digest, SizeBytes: d.SizeBytes, MediaType: d.MediaType}
}
func (c runtimeCheckpointer) checkpointManifest(request CheckpointRequest, artifact vm.SnapshotArtifact, disk computer.DiskArtifact, candidates []*checkpointCandidate) workerapi.CheckpointManifest {
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
			Computer:            &workerapi.CheckpointComputer{ComputerID: artifact.Computer.ComputerID, LogicalBytes: disk.LogicalBytes, Artifact: checkpointDescriptor(disk.Object)},
			ConfigArtifact:      checkpointDescriptor(candidates[0].descriptor),
			VMStateArtifact:     checkpointDescriptor(candidates[1].descriptor),
			ScratchDiskArtifact: checkpointDescriptor(candidates[2].descriptor),
			MemoryArtifacts:     []workerapi.CheckpointArtifact{checkpointDescriptor(candidates[3].descriptor)},
			Config:              artifact.Manifest,
		},
		WorkspaceState: workerapi.CheckpointWorkspaceState{
			Base: c.workspace,
		},
	}
}
