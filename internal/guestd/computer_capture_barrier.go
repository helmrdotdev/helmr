package guestd

import (
	"errors"
	"strings"
	"time"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

// sealComputerCapture closes physical admission before any member is frozen.
// A failed freeze must not reopen it: thaw or physical exclusion needs its own
// proof. Sealing alone is not a freeze acknowledgement.
func (r *computerOperationRegistry) sealComputerCapture(request *computerv0.FreezeComputerRequest, clock func() time.Time) error {
	if request == nil || strings.TrimSpace(request.ComputerId) == "" || strings.TrimSpace(request.ComputerInstanceId) == "" || strings.TrimSpace(request.CheckpointId) == "" || request.WriterGeneration <= 0 || request.DesiredVersion <= 0 || request.MembershipRevision < 0 {
		return errors.New("computer capture identity is incomplete")
	}
	r.mu.Lock()
	var entry *computerMountEntry
	for _, candidate := range r.entries {
		if candidate == nil || (entry != nil && entry != candidate) {
			r.mu.Unlock()
			return errors.New("computer capture has ambiguous mounted identity")
		}
		entry = candidate
	}
	r.mu.Unlock()
	if entry != nil {
		entry.lifecycleMu.Lock()
		defer entry.lifecycleMu.Unlock()
		entry.finalizationMu.Lock()
		defer entry.finalizationMu.Unlock()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.captureRequest != nil {
		if !proto.Equal(r.captureRequest, request) {
			return errors.New("computer capture barrier belongs to another request")
		}
		return nil
	}
	if r.materializations != 0 {
		return errors.New("computer materialization is still in progress")
	}
	var current *computerMountEntry
	for _, candidate := range r.entries {
		if candidate == nil || candidate.retired || (current != nil && current != candidate) {
			return errors.New("computer capture mounted identity is unavailable")
		}
		current = candidate
	}
	if current != entry {
		return errors.New("computer capture mounted identity changed")
	}
	if entry == nil {
		prepared := r.preparedRuntime
		if prepared == nil || len(request.Runs) != 0 || len(r.programClaims) != 0 || prepared.computerID != request.ComputerId || prepared.computerInstanceID != request.ComputerInstanceId || prepared.writerGeneration != request.WriterGeneration {
			return errors.New("computer capture prepared identity differs")
		}
	} else {
		if r.preparedRuntime != nil || entry.stopping || entry.computerID != request.ComputerId || entry.computerInstanceID != request.ComputerInstanceId || entry.writerGeneration != request.WriterGeneration {
			return errors.New("computer capture mounted identity differs")
		}
		entry.processesMu.Lock()
		busy := entry.processAdmissions != 0 || entry.recoveryRequired
		entry.processesMu.Unlock()
		if busy || entry.hasUnreleasedCommands() {
			return errors.New("computer capture has unreconciled work")
		}
	}
	if len(r.programClaims) != len(request.Runs) {
		return errors.New("computer capture membership differs")
	}
	expected := make(map[string]*computerv0.ComputerCaptureRun, len(request.Runs))
	for _, member := range request.Runs {
		if member == nil || strings.TrimSpace(member.RunId) == "" || member.AttemptNumber == 0 || strings.TrimSpace(member.RunWaitId) == "" || strings.TrimSpace(member.RunLeaseId) == "" || expected[member.RunId] != nil {
			return errors.New("computer capture member is incomplete or duplicated")
		}
		expected[member.RunId] = member
	}
	if clock == nil {
		clock = time.Now
	}
	now := clock()
	for _, claim := range r.programClaims {
		if claim == nil || claim.entry != entry || claim.authority == nil {
			return errors.New("computer capture claim is incomplete")
		}
		fence := claim.authority.GetFence()
		member := expected[fence.GetRunId()]
		if member == nil || member.AttemptNumber != fence.GetAttemptNumber() || member.RunLeaseId != fence.GetRunLeaseId() || fence.GetComputerId() != request.ComputerId || fence.GetComputerInstanceId() != request.ComputerInstanceId || fence.GetWriterGeneration() != request.WriterGeneration || fence.GetExpiresAtUnixNano() <= now.UnixNano() {
			return errors.New("computer capture claim authority differs or expired")
		}
		delete(expected, member.RunId)
	}
	r.captureRequest = proto.Clone(request).(*computerv0.FreezeComputerRequest)
	r.restoredMaterialization = nil
	r.restoreInstallation = nil
	r.restoreActivated = false
	return nil
}

func (r *computerOperationRegistry) captureSealed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.captureRequest != nil
}

// Reserve before preparation or materialization touches the guest filesystem.
func (r *computerOperationRegistry) reserveMaterialization() (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.captureRequest != nil {
		return nil, errors.New("computer capture has sealed materialization")
	}
	r.materializations++
	return func() { r.mu.Lock(); r.materializations--; r.mu.Unlock() }, nil
}
