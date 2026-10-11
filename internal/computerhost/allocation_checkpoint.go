package computerhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

type AgentCheckpointClient interface {
	BeginAgentComputerCapture(context.Context, workerapi.AgentComputerCaptureRequest) (workerapi.AgentComputerCaptureResponse, error)
	SealAgentComputerCapture(context.Context, workerapi.AgentComputerReceiptRequest) error
	CancelAgentComputerCapture(context.Context, workerapi.AgentComputerUnsealedRequest) error
	AgentComputerSaveAbsence(context.Context, workerapi.AgentComputerSaveAbsenceRequest) error
	PrepareAgentComputerSourceAbort(context.Context, workerapi.AgentComputerSourceAbortRequest) (workerapi.AgentComputerInstallationResponse, error)
	RegisterAgentCheckpoint(context.Context, workerapi.AgentCheckpointPublication) error
	CompleteAgentCheckpoint(context.Context, workerapi.AgentCheckpointPublication) error
}

var errComputerHibernated = errors.New("computer checkpoint is durable")

// Checkpoint admission and all disk publication run under the same serial save
// loop. This retains one operation identity through every uncertain CP reply.
func (o *ComputerAllocationOwner) captureIdle(ctx context.Context, delivery workerapi.ComputerAllocationDelivery, suspendAttachments func() func()) (resultErr error) {
	if time.Now().Before(o.checkpointRetryAt) {
		return nil
	}
	busy := false
	defer func() {
		if resultErr == nil {
			// Busy admission uses a short poll. Exponential backoff is reserved
			// for guest refusal or an aborted capture, not prolonged active work.
			if busy {
				o.checkpointRetryDelay = 0
				o.checkpointRetryAt = time.Now().Add(time.Second)
			} else {
				o.checkpointRetryDelay = checkpointRetryDelay(o.checkpointRetryDelay)
				o.checkpointRetryAt = time.Now().Add(o.checkpointRetryDelay)
			}
		}
	}()
	id := uuid.NewV7().String()
	request := workerapi.AgentComputerCaptureRequest{EnvironmentID: o.identity.EnvironmentID, ComputerID: o.identity.OwnerID, CheckpointID: id, LeaseEpoch: o.identity.Epoch, ChannelCredential: delivery.ChannelCredential}
	var response workerapi.AgentComputerCaptureResponse
	err := retryCheckpoint(ctx, func(ctx context.Context) error {
		var err error
		response, err = o.client.BeginAgentComputerCapture(ctx, request)
		return err
	})
	if err != nil {
		var rejected *httpclient.Error
		if errors.As(err, &rejected) && rejected.Code == workerapi.AgentComputerNotReady {
			busy = true
			return nil
		}
		return err
	}
	capture := new(agentv1.ComputerSessionCapture)
	if err := proto.Unmarshal(response.Capture, capture); err != nil {
		return err
	}
	envelope := capture.GetEnvelope()
	if capture.GetCheckpointId() != id || envelope.GetComputerId() != o.identity.OwnerID || envelope.GetComputerInstanceId() != o.identity.InstanceID || envelope.GetWriterGeneration() != uint64(o.identity.Epoch) || envelope.GetChannelCredential() != delivery.ChannelCredential {
		return errors.New("capture differs from physical allocation")
	}
	if _, err := uuid.Parse(response.SaveID); err != nil {
		return err
	}
	resumeAttachments := suspendAttachments()
	defer func() {
		if resultErr == nil {
			resumeAttachments()
		}
	}()
	hold, err := o.machine.BeginCheckpoint(ctx, vm.SnapshotRequest{ID: id})
	if err != nil {
		return err
	}
	receipt, err := CaptureAgentComputer(ctx, hold, capture)
	if err != nil {
		var rejected *agentComputerUnsealedError
		if errors.As(err, &rejected) {
			slog.Info("Computer capture qualification rejected", "computer_id", o.identity.OwnerID, "reason", rejected.Error())
			if err = retryCheckpoint(ctx, func(ctx context.Context) error {
				return o.client.CancelAgentComputerCapture(ctx, workerapi.AgentComputerUnsealedRequest{EnvironmentID: o.identity.EnvironmentID, CheckpointID: id, Rejected: true})
			}); err != nil {
				return err
			}
			if err = hold.ResumeGuestControl(ctx); err != nil {
				return err
			}
			return hold.CompleteAbort(ctx)
		}
		slog.Info("Computer capture requires source reconciliation", "computer_id", o.identity.OwnerID, "checkpoint_id", id, "error", err)
		// No serialization was attempted. Inspect the retained guest record before
		// reserving source continuation; neither a timeout nor a lost reply proves absence.
		if err = hold.ResumeGuestControl(ctx); err != nil {
			return err
		}
		return o.abortUncutCheckpoint(ctx, delivery, capture, hold)
	}
	raw, err := proto.Marshal(receipt)
	if err != nil {
		return err
	}
	if err = retryCheckpoint(ctx, func(ctx context.Context) error {
		return o.client.SealAgentComputerCapture(ctx, workerapi.AgentComputerReceiptRequest{EnvironmentID: o.identity.EnvironmentID, CheckpointID: id, Receipt: raw})
	}); err != nil {
		return err
	}
	// This physical cut is attempted exactly once. A failed or ambiguous cut is
	// terminal for this allocation; it never becomes a fabricated no-cut receipt.
	artifact, err := hold.CreateSnapshot(ctx)
	if artifact.Computer != nil {
		o.checkpointCut = artifact.Computer.Capture
	}
	if err != nil {
		return fmt.Errorf("create Computer checkpoint snapshot: %w", err)
	}
	if artifact.Computer == nil || artifact.Computer.ComputerID != o.identity.OwnerID || artifact.Computer.Capture == nil || len(artifact.Memory) != 1 {
		return errors.New("incomplete coherent Computer checkpoint")
	}
	publication := workerapi.AgentSavePublication{Save: workerapi.AgentSave{EnvironmentID: o.identity.EnvironmentID, SaveID: response.SaveID, LeaseEpoch: o.identity.Epoch}, Root: artifact.Computer.Capture.Root(), Evidence: id}
	if err = o.retryCheckpointDurability(ctx, func(ctx context.Context) error { return o.client.CaptureAgentSave(ctx, publication) }); err != nil {
		return fmt.Errorf("register checkpoint disk capture: %w", err)
	}
	if err = o.retryCheckpointDurability(ctx, func(ctx context.Context) error {
		return artifact.Computer.Capture.Publish(ctx, agentSavePublisher{client: o.client, objects: o.machines.ComputerObjects, save: publication.Save})
	}); err != nil {
		return fmt.Errorf("publish checkpoint disk objects: %w", err)
	}
	if err = o.retryCheckpointDurability(ctx, func(ctx context.Context) error { return o.client.PublishAgentSave(ctx, publication) }); err != nil {
		return fmt.Errorf("publish checkpoint disk save: %w", err)
	}
	var manifest computercheckpoint.Manifest
	var files []string
	if err = o.retryCheckpointDurability(ctx, func(ctx context.Context) error {
		var err error
		manifest, files, err = o.encryptCheckpoint(ctx, capture, response.Capture, artifact)
		return err
	}); err != nil {
		return fmt.Errorf("encrypt checkpoint runtime objects: %w", err)
	}
	checkpoint := workerapi.AgentCheckpointPublication{EnvironmentID: o.identity.EnvironmentID, Manifest: manifest}
	if err = o.retryCheckpointDurability(ctx, func(ctx context.Context) error { return o.client.RegisterAgentCheckpoint(ctx, checkpoint) }); err != nil {
		return fmt.Errorf("register checkpoint runtime objects: %w", err)
	}
	for i, object := range manifest.Objects() {
		descriptor := cas.Descriptor{Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}
		err = o.retryCheckpointDurability(ctx, func(ctx context.Context) error {
			file, err := os.Open(files[i])
			if err != nil {
				return err
			}
			defer file.Close()
			got, err := o.machines.ComputerObjects.Publish(ctx, descriptor, file)
			if err != nil {
				return err
			}
			return cas.RequireExact(got, descriptor)
		})
		if err != nil {
			return fmt.Errorf("publish checkpoint %s: %w", object.Role, err)
		}
	}
	if err = o.retryCheckpointDurability(ctx, func(ctx context.Context) error { return o.client.CompleteAgentCheckpoint(ctx, checkpoint) }); err != nil {
		return fmt.Errorf("complete Computer checkpoint: %w", err)
	}
	return errComputerHibernated
}

// After a known successful cut, a local I/O or remote publication failure is
// not permission to destroy a healthy source. The lease-renewal owner cancels
// this context if physical authority expires or is revoked. Retain the exact cut
// and immutable publication through recoverable I/O and authority-reconciliation
// errors. A rejected/invalid cut remains an explicit continuation failure.
func (o *ComputerAllocationOwner) retryCheckpointDurability(ctx context.Context, operation func(context.Context) error) error {
	var delay time.Duration
	for {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		err := operation(ctx)
		if err == nil {
			return nil
		}
		if !checkpointDurabilityRetryable(err) {
			return err
		}
		delay = checkpointRetryDelay(delay)
		slog.Warn("Computer retains checkpoint source while publication retries", "computer_id", o.identity.OwnerID, "retry_after", delay, "error", err)
		if err = sleepWithContext(ctx, delay); err != nil {
			return err
		}
	}
}
func checkpointDurabilityRetryable(err error) bool {
	if checkpointContinuationRetryable(err) || errors.Is(err, cas.ErrUnavailable) {
		return true
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case syscall.ENOSPC, syscall.EDQUOT, syscall.EIO, syscall.EMFILE, syscall.ENFILE, syscall.EBUSY, syscall.EAGAIN, syscall.EINTR, syscall.ESTALE:
		return true
	}
	return false
}

func checkpointRetryDelay(previous time.Duration) time.Duration {
	if previous == 0 {
		return time.Second
	}
	return min(2*previous, 30*time.Second)
}

func retryCheckpoint(ctx context.Context, operation func(context.Context) error) error {
	for {
		err := operation(ctx)
		if err == nil || (!saveAdmissionRetryable(err) && !errors.Is(err, cas.ErrUnavailable)) {
			return err
		}
		if err = sleepWithContext(ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}
}

func (o *ComputerAllocationOwner) abortUncutCheckpoint(ctx context.Context, delivery workerapi.ComputerAllocationDelivery, capture *agentv1.ComputerSessionCapture, hold vm.CheckpointCapture) (resultErr error) {
	var err error
	for {
		observedAt := time.Now()
		_, err = controlAgentComputer(ctx, o.machine, &agentv1.ComputerSessionControl{Operation: &agentv1.ComputerSessionControl_Inspect{Inspect: capture}})
		if err == nil {
			break
		}
		var unsealed *agentComputerUnsealedError
		if errors.As(err, &unsealed) && unsealed.code == agentv1.ComputerSessionErrorCode_COMPUTER_SESSION_ERROR_CODE_CAPTURE_ABSENT_AFTER_EXPIRY && !observedAt.Before(time.Unix(0, capture.GetEnvelope().GetOperationExpiresAtUnixNano())) {
			// Preserve this exact observation while PG catches up with an ahead
			// worker clock. The guest separately proved its admission expired.
			for {
				err = o.client.CancelAgentComputerCapture(ctx, workerapi.AgentComputerUnsealedRequest{EnvironmentID: o.identity.EnvironmentID, CheckpointID: capture.GetCheckpointId(), AbsentObservedAt: observedAt})
				if err == nil {
					return hold.CompleteAbort(ctx)
				}
				if !checkpointContinuationRetryable(err) {
					return err
				}
				if err = sleepWithContext(ctx, 250*time.Millisecond); err != nil {
					return err
				}
			}
		}
		if unsealed != nil {
			if unsealed.code != agentv1.ComputerSessionErrorCode_COMPUTER_SESSION_ERROR_CODE_CAPTURE_ADMISSION_PENDING && unsealed.code != agentv1.ComputerSessionErrorCode_COMPUTER_SESSION_ERROR_CODE_CAPTURE_ABSENT_AFTER_EXPIRY {
				return err
			}
		} else if !saveAdmissionRetryable(err) {
			return err
		}
		if err = sleepWithContext(ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}
	if err = o.continueCheckpoint(ctx, delivery, capture, true); err != nil {
		return err
	}
	return hold.CompleteAbort(ctx)
}

func (o *ComputerAllocationOwner) encryptCheckpoint(ctx context.Context, capture *agentv1.ComputerSessionCapture, encoded []byte, a vm.SnapshotArtifact) (computercheckpoint.Manifest, []string, error) {
	m := computercheckpoint.Manifest{LeaseEpoch: o.identity.Epoch, ControlVersion: capture.GetDesiredVersion(), Config: a.Manifest, Disk: a.Computer.Capture.Root(), Runtime: vm.CheckpointIdentity{RuntimeBackend: a.RuntimeBackend, RuntimeArch: a.RuntimeArch, VMRuntimeContract: a.VMRuntimeContract, RuntimeID: a.RuntimeID, KernelDigest: a.KernelDigest, InitramfsDigest: a.InitramfsDigest, RootfsDigest: a.RootfsDigest, VMConfigDigest: a.VMConfigDigest, VMVCPUCount: a.VMVCPUCount, CPUConfigDigest: a.CPUConfigDigest}}
	var err error
	m.CheckpointID, err = uuid.Parse(capture.GetCheckpointId())
	if err != nil {
		return m, nil, err
	}
	m.ComputerID, err = uuid.Parse(o.identity.OwnerID)
	if err != nil {
		return m, nil, err
	}
	m.InstanceID, err = uuid.Parse(o.identity.InstanceID)
	if err != nil {
		return m, nil, err
	}
	digest := sha256.Sum256(encoded)
	m.CaptureDigest = digest[:]
	for _, member := range capture.GetSessions() {
		id, err := uuid.Parse(member.GetSessionId())
		if err != nil {
			return m, nil, err
		}
		m.Members = append(m.Members, computercheckpoint.Session{SessionID: id, ProcessEpoch: member.GetProcessEpoch()})
	}
	shape, err := o.machine.SnapshotLimits()
	if err != nil {
		return m, nil, err
	}
	limits, err := checkpointStagingSize(shape, o.machines.CheckpointCipher)
	if err != nil {
		return m, nil, err
	}
	parent := filepath.Join(o.machines.computerPreparationDirectory(o.identity.InstanceID, o.identity.Epoch), "checkpoint")
	if err = os.MkdirAll(parent, 0700); err != nil {
		return m, nil, err
	}
	directory := filepath.Join(parent, "encryption")
	// An unsuccessful attempt is replaced only after its readers have joined.
	// If cleanup fails, retry this same directory instead of allocating more.
	if err = os.RemoveAll(directory); err != nil {
		return m, nil, err
	}
	if err = os.Mkdir(directory, 0700); err != nil {
		return m, nil, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(directory)
		}
	}()
	var files []string
	var path string
	m.VMConfig, path, err = encryptCheckpointObject(ctx, o.machines.CheckpointCipher, directory, capture.GetCheckpointId(), "vm_config", cas.CheckpointVMConfigMediaType, bytes.NewReader(a.Manifest), limits.config)
	if err != nil {
		return m, nil, err
	}
	files = append(files, path)
	for _, item := range []struct {
		role, media, path string
		limit             int64
		target            *computercheckpoint.Object
	}{{"vm_state", cas.CheckpointVMStateMediaType, a.VMState.Path, limits.state, &m.VMState}, {"memory", cas.CheckpointMemoryMediaType, a.Memory[0].Path, limits.memory, &m.Memory}, {"scratch_disk", cas.CheckpointScratchDiskMediaType, a.ScratchDisk.Path, limits.scratch, &m.ScratchDisk}} {
		source, err := os.Open(item.path)
		if err != nil {
			return m, nil, err
		}
		object, path, err := encryptCheckpointObject(ctx, o.machines.CheckpointCipher, directory, capture.GetCheckpointId(), item.role, item.media, source, item.limit)
		err = errors.Join(err, source.Close())
		if err != nil {
			return m, nil, err
		}
		*item.target = object
		files = append(files, path)
	}
	_, err = m.Encode()
	complete = err == nil
	return m, files, err
}
