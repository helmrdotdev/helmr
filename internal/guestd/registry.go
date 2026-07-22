package guestd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"google.golang.org/protobuf/proto"
)

var resumeAttachTimeout = 30 * time.Second

type waitingRunRegistry struct {
	mu    sync.Mutex
	slots map[string]*waitingRunSlot
}

type waitingRunSlot struct {
	runID                    string
	attemptNumber            uint32
	checkpointID             string
	resumeAttachID           string
	checkpointRequestVersion int64
	correlationID            string
	attached                 chan waitingRunAttachment
	accepted                 *runv0.ResumeAttach
	appliedDecision          *runv0.ResumeDecision
	appliedAck               *runv0.ResumeAck
	granted                  *programResumeGrant
}

type programResumeGrant struct {
	attach *runv0.ResumeAttach
	lock   func()
	unlock func()
	valid  func(time.Time) bool
}

type waitingRunAttachment struct {
	stream io.ReadWriter
	attach *runv0.ResumeAttach
}

func (r *waitingRunRegistry) registerProgram(request *runv0.CheckpointPauseRequest) (waitingRunRegistration, error) {
	if request == nil || request.GetRunWaitId() == "" || request.GetCheckpointId() == "" ||
		request.GetResumeAttachId() == "" || request.GetCheckpointRequestVersion() <= 0 ||
		request.GetCorrelationId() == "" {
		return waitingRunRegistration{}, fmt.Errorf("exact Program checkpoint registration is required")
	}
	slot := &waitingRunSlot{
		runID:                    request.GetRunId(),
		attemptNumber:            request.GetAttemptNumber(),
		checkpointID:             request.GetCheckpointId(),
		resumeAttachID:           request.GetResumeAttachId(),
		checkpointRequestVersion: request.GetCheckpointRequestVersion(),
		correlationID:            request.GetCorrelationId(),
		attached:                 make(chan waitingRunAttachment, 1),
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.slots[request.GetRunWaitId()]; exists {
		return waitingRunRegistration{}, fmt.Errorf("run wait %s already has a registration", request.GetRunWaitId())
	}
	r.slots[request.GetRunWaitId()] = slot
	return waitingRunRegistration{registry: r, runWaitID: request.GetRunWaitId(), slot: slot}, nil
}

type waitingRunRegistration struct {
	registry  *waitingRunRegistry
	runWaitID string
	slot      *waitingRunSlot
}

func newWaitingRunRegistry() *waitingRunRegistry {
	return &waitingRunRegistry{slots: map[string]*waitingRunSlot{}}
}

func (r *waitingRunRegistry) register(runWaitID string, checkpointID string) waitingRunRegistration {
	slot := &waitingRunSlot{
		checkpointID: checkpointID,
		attached:     make(chan waitingRunAttachment, 1),
	}
	r.mu.Lock()
	r.slots[runWaitID] = slot
	r.mu.Unlock()
	return waitingRunRegistration{registry: r, runWaitID: runWaitID, slot: slot}
}

func (r *waitingRunRegistry) hasFrozenProgramCheckpoint(checkpointID string) bool {
	checkpointID = strings.TrimSpace(checkpointID)
	if checkpointID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, slot := range r.slots {
		if slot != nil && slot.resumeAttachID != "" && slot.checkpointID == checkpointID &&
			slot.accepted == nil && slot.appliedDecision == nil && slot.appliedAck == nil {
			return true
		}
	}
	return false
}

func (r *waitingRunRegistry) verifyFrozenProgram(request *workspacev0.VerifyProgramRestoreRequest) bool {
	if request == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	slot := r.slots[request.GetRunWaitId()]
	return slot != nil && slot.resumeAttachID != "" && slot.runID == request.GetRunId() &&
		slot.attemptNumber == request.GetAttemptNumber() && slot.checkpointID == request.GetCheckpointId() &&
		slot.correlationID == request.GetCorrelationId() && slot.accepted == nil &&
		slot.appliedDecision == nil && slot.appliedAck == nil
}

func (r *waitingRunRegistry) attach(runWaitID string, checkpointID string, stream io.ReadWriter) error {
	return r.attachResume(&runv0.ResumeAttach{
		RunWaitId: runWaitID, CheckpointId: checkpointID,
	}, stream)
}

func (r *waitingRunRegistry) attachResume(attach *runv0.ResumeAttach, stream io.ReadWriter) error {
	if attach == nil {
		return fmt.Errorf("resume attach is required")
	}
	r.mu.Lock()
	slot := r.slots[attach.GetRunWaitId()]
	var grant *programResumeGrant
	if slot != nil {
		grant = slot.granted
	}
	r.mu.Unlock()
	if grant != nil {
		grant.lock()
		defer grant.unlock()
		if !grant.valid(time.Now()) {
			return errors.New("Program resume grant authority is no longer current")
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	slot = r.slots[attach.GetRunWaitId()]
	if slot == nil {
		return fmt.Errorf("no waiting run slot matched run wait %s checkpoint %s", attach.GetRunWaitId(), attach.GetCheckpointId())
	}
	if slot.checkpointID != attach.GetCheckpointId() {
		return fmt.Errorf("resume attach checkpoint %s did not match expected %s", attach.GetCheckpointId(), slot.checkpointID)
	}
	if slot.resumeAttachID != "" && (attach.GetRunId() != slot.runID ||
		attach.GetAttemptNumber() != slot.attemptNumber ||
		strings.TrimSpace(attach.GetRunLeaseId()) == "" ||
		attach.GetResumeAttachId() != slot.resumeAttachID ||
		attach.GetCorrelationId() != slot.correlationID ||
		attach.GetResumeRequestVersion() <= 0) {
		return fmt.Errorf("resume attach did not match exact Program Wait authority")
	}
	if slot.resumeAttachID != "" && (slot.granted == nil || slot.granted != grant ||
		!proto.Equal(slot.granted.attach, attach)) {
		return fmt.Errorf("resume attach was not granted by current Program authority")
	}
	if slot.accepted != nil && !proto.Equal(slot.accepted, attach) {
		return fmt.Errorf("resume attach changed an already accepted Program Wait tuple")
	}
	select {
	case slot.attached <- waitingRunAttachment{
		stream: stream,
		attach: proto.Clone(attach).(*runv0.ResumeAttach),
	}:
		if slot.accepted == nil {
			slot.accepted = proto.Clone(attach).(*runv0.ResumeAttach)
		}
		return nil
	default:
		return fmt.Errorf("run wait %s already has an attached resume stream", attach.GetRunWaitId())
	}
}

func (r *waitingRunRegistry) grantProgramResume(grant *programResumeGrant) error {
	if grant == nil || grant.attach == nil || grant.lock == nil || grant.unlock == nil || grant.valid == nil {
		return errors.New("Program resume grant is required")
	}
	attach := grant.attach
	r.mu.Lock()
	defer r.mu.Unlock()
	slot := r.slots[attach.GetRunWaitId()]
	if slot == nil || slot.resumeAttachID == "" ||
		attach.GetRunId() != slot.runID || attach.GetAttemptNumber() != slot.attemptNumber ||
		attach.GetCheckpointId() != slot.checkpointID ||
		attach.GetResumeAttachId() != slot.resumeAttachID ||
		attach.GetCorrelationId() != slot.correlationID ||
		strings.TrimSpace(attach.GetRunLeaseId()) == "" || attach.GetResumeRequestVersion() <= 0 {
		return errors.New("Program resume grant did not match the frozen Wait")
	}
	if slot.granted != nil && !proto.Equal(slot.granted.attach, attach) {
		return errors.New("Program resume grant changed an installed authority")
	}
	if slot.accepted != nil && !proto.Equal(slot.accepted, attach) {
		return errors.New("Program resume grant changed an accepted authority")
	}
	grant.attach = proto.Clone(attach).(*runv0.ResumeAttach)
	slot.granted = grant
	return nil
}

func (r waitingRunRegistration) markApplied(decision *runv0.ResumeDecision, ack *runv0.ResumeAck) {
	r.registry.mu.Lock()
	defer r.registry.mu.Unlock()
	r.slot.appliedDecision = proto.Clone(decision).(*runv0.ResumeDecision)
	r.slot.appliedAck = proto.Clone(ack).(*runv0.ResumeAck)
}

func (r waitingRunRegistration) wait(ctx context.Context) (io.ReadWriter, *runv0.ResumeAttach, error) {
	return r.waitStream(ctx, nil)
}

func (r waitingRunRegistration) waitStream(ctx context.Context, stopped <-chan struct{}) (io.ReadWriter, *runv0.ResumeAttach, error) {
	select {
	case attached := <-r.slot.attached:
		return attached.stream, attached.attach, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-stopped:
		return nil, nil, errors.New("Program stream stopped")
	}
}

func (r waitingRunRegistration) unregister() {
	r.registry.mu.Lock()
	if r.registry.slots[r.runWaitID] == r.slot {
		delete(r.registry.slots, r.runWaitID)
	}
	r.registry.mu.Unlock()
}
