package guestd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"google.golang.org/protobuf/proto"
)

var resumeAttachTimeout = 30 * time.Second

type waitingRunRegistry struct {
	mu      sync.Mutex
	slots   map[string]*waitingRunSlot
	changed chan struct{}
}

type waitingRunSlot struct {
	replayMu                 sync.Mutex
	execution                *programv0.SessionExecution
	turnID                   *string
	runID                    string
	attemptNumber            uint32
	runLeaseID               string
	frozen                   bool
	checkpointID             string
	resumeAttachID           string
	checkpointRequestVersion int64
	correlationID            string
	retired                  chan struct{}
	abortResume              chan struct{}
	abortDone                chan struct{}
	abortErr                 error
	abortStream              programConnection
	abortSequence            uint64
	attached                 chan waitingRunAttachment
	accepted                 *programv0.ResumeAttach
	appliedDecision          *programv0.ResumeDecision
	appliedAck               *programv0.ResumeAck
	granted                  *programResumeGrant
}

type programResumeGrant struct {
	attach *programv0.ResumeAttach
	lock   func()
	unlock func()
	valid  func(time.Time) bool
}

type waitingRunAttachment struct {
	stream io.ReadWriter
	attach *programv0.ResumeAttach
}

func (r *waitingRunRegistry) registerProgram(request *programv0.CheckpointPauseRequest) (waitingRunRegistration, error) {
	if request == nil || request.GetRunWaitId() == "" || request.GetCheckpointId() == "" ||
		request.GetResumeAttachId() == "" || request.GetCheckpointRequestVersion() <= 0 ||
		request.GetCorrelationId() == "" {
		return waitingRunRegistration{}, fmt.Errorf("exact program checkpoint registration is required")
	}
	frozen := proto.Clone(request).(*programv0.CheckpointPauseRequest)
	slot := &waitingRunSlot{
		execution:                frozen.Execution,
		turnID:                   frozen.TurnId,
		runID:                    request.GetRunId(),
		attemptNumber:            request.GetAttemptNumber(),
		runLeaseID:               request.GetRunLeaseId(),
		checkpointID:             request.GetCheckpointId(),
		resumeAttachID:           request.GetResumeAttachId(),
		checkpointRequestVersion: request.GetCheckpointRequestVersion(),
		correlationID:            request.GetCorrelationId(),
		attached:                 make(chan waitingRunAttachment, 1),
		retired:                  make(chan struct{}),
		abortResume:              make(chan struct{}),
		abortDone:                make(chan struct{}),
	}
	r.retireAppliedWait(request)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.slots[request.GetRunWaitId()]; exists {
		return waitingRunRegistration{}, fmt.Errorf("run wait %s already has a registration", request.GetRunWaitId())
	}
	r.slots[request.GetRunWaitId()] = slot
	r.notifyChangedLocked()
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

func (r waitingRunRegistration) markFrozen() {
	r.registry.mu.Lock()
	defer r.registry.mu.Unlock()
	r.slot.frozen = true
	r.registry.notifyChangedLocked()
}

func (r *waitingRunRegistry) attachResume(attach *programv0.ResumeAttach, stream io.ReadWriter) error {
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
			return errors.New("program resume grant authority is no longer current")
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
		return fmt.Errorf("resume attach did not match exact program wait authority")
	}
	if slot.resumeAttachID != "" && (slot.granted == nil || slot.granted != grant ||
		!proto.Equal(slot.granted.attach, attach)) {
		return fmt.Errorf("resume attach was not granted by current program authority")
	}
	if slot.accepted != nil && !proto.Equal(slot.accepted, attach) {
		return fmt.Errorf("resume attach changed an already accepted program wait tuple")
	}
	select {
	case slot.attached <- waitingRunAttachment{
		stream: stream,
		attach: proto.Clone(attach).(*programv0.ResumeAttach),
	}:
		if slot.accepted == nil {
			slot.accepted = proto.Clone(attach).(*programv0.ResumeAttach)
		}
		return nil
	default:
		return fmt.Errorf("run wait %s already has an attached resume stream", attach.GetRunWaitId())
	}
}

func (r *waitingRunRegistry) grantProgramResume(grant *programResumeGrant) error {
	if grant == nil || grant.attach == nil || grant.lock == nil || grant.unlock == nil || grant.valid == nil {
		return errors.New("program resume grant is required")
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
		return errors.New("program resume grant did not match the frozen wait")
	}
	// Actor scope comes from the frozen Program, while the mounted authority
	// grants its new lease. Keep the combined tuple exact for every attachment.
	attach = proto.Clone(attach).(*programv0.ResumeAttach)
	attach.Execution, attach.TurnId = nil, nil
	if slot.execution != nil {
		attach.Execution = proto.Clone(slot.execution).(*programv0.SessionExecution)
	}
	if slot.turnID != nil {
		turnID := *slot.turnID
		attach.TurnId = &turnID
	}
	if slot.granted != nil && !proto.Equal(slot.granted.attach, attach) {
		return errors.New("program resume grant changed an installed authority")
	}
	if slot.accepted != nil && !proto.Equal(slot.accepted, attach) {
		return errors.New("program resume grant changed an accepted authority")
	}
	grant.attach = attach
	slot.granted = grant
	return nil
}

func (r waitingRunRegistration) markApplied(decision *programv0.ResumeDecision, ack *programv0.ResumeAck) {
	r.registry.mu.Lock()
	defer r.registry.mu.Unlock()
	r.slot.appliedDecision = proto.Clone(decision).(*programv0.ResumeDecision)
	r.slot.appliedAck = proto.Clone(ack).(*programv0.ResumeAck)
}

func (r waitingRunRegistration) wait(ctx context.Context) (io.ReadWriter, *programv0.ResumeAttach, error) {
	return r.waitStream(ctx, nil)
}

func (r waitingRunRegistration) waitStream(ctx context.Context, stopped <-chan struct{}) (io.ReadWriter, *programv0.ResumeAttach, error) {
	select {
	case attached := <-r.slot.attached:
		return attached.stream, attached.attach, nil
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	case <-r.slot.retired:
		return nil, nil, errors.New("program resume receipt retired")
	case <-stopped:
		return nil, nil, errors.New("program stream stopped")
	case <-r.slot.abortResume:
		return nil, nil, errCaptureAborted
	}
}

func (r waitingRunRegistration) unregister() {
	r.slot.replayMu.Lock()
	defer r.slot.replayMu.Unlock()
	r.registry.mu.Lock()
	if r.registry.slots[r.runWaitID] == r.slot {
		r.registry.retireSlotLocked(r.runWaitID, r.slot)
	}
	r.registry.mu.Unlock()
}

// Attachment correlation belongs to the retained Guest wait, not VM scheduling.
func (r *waitingRunRegistry) resumeAttachment(fence *computerv0.ComputerAuthorityFence, request *computerv0.GrantProgramResumeRequest) (*programv0.ResumeAttach, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	slot := r.slots[request.GetRunWaitId()]
	if slot == nil || fence == nil || slot.checkpointRequestVersion <= 0 || slot.runID != fence.GetRunId() || slot.attemptNumber != fence.GetAttemptNumber() || slot.checkpointID != request.GetCheckpointId() {
		return nil, errors.New("resume request differs from retained wait")
	}
	attach := &programv0.ResumeAttach{RunId: slot.runID, AttemptNumber: slot.attemptNumber, RunLeaseId: fence.GetRunLeaseId(), RunWaitId: request.GetRunWaitId(), CheckpointId: slot.checkpointID, ResumeAttachId: slot.resumeAttachID, CorrelationId: slot.correlationID, ResumeRequestVersion: slot.checkpointRequestVersion, Execution: slot.execution, TurnId: slot.turnID}
	return proto.Clone(attach).(*programv0.ResumeAttach), nil
}

func (r *waitingRunRegistry) retireSlotLocked(id string, slot *waitingRunSlot) {
	if r.slots[id] != slot {
		return
	}
	delete(r.slots, id)
	if slot.abortStream != nil {
		_ = slot.abortStream.Close()
		slot.abortStream = nil
	}
	if slot.retired != nil {
		close(slot.retired)
	}
	select {
	case attached := <-slot.attached:
		if c, ok := attached.stream.(io.Closer); ok {
			_ = c.Close()
		}
	default:
	}
	r.notifyChangedLocked()
}

func (r *waitingRunRegistry) retireAppliedWait(request *programv0.CheckpointPauseRequest) {
	r.mu.Lock()
	old := r.slots[request.GetRunWaitId()]
	r.mu.Unlock()
	if old == nil {
		return
	}
	old.replayMu.Lock()
	defer old.replayMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if old.appliedAck != nil && old.checkpointID != request.GetCheckpointId() && old.runID == request.GetRunId() && old.attemptNumber == request.GetAttemptNumber() && old.appliedAck.GetRunLeaseId() == request.GetRunLeaseId() {
		r.retireSlotLocked(request.GetRunWaitId(), old)
	}
}
func (r *waitingRunRegistry) retireAppliedCaptures(checkpointID string) {
	r.mu.Lock()
	candidates := make(map[string]*waitingRunSlot)
	for id, slot := range r.slots {
		if slot.appliedAck != nil && slot.checkpointID != checkpointID {
			candidates[id] = slot
		}
	}
	r.mu.Unlock()
	for id, slot := range candidates {
		slot.replayMu.Lock()
		r.mu.Lock()
		r.retireSlotLocked(id, slot)
		r.mu.Unlock()
		slot.replayMu.Unlock()
	}
}
