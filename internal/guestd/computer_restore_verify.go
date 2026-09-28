package guestd

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

func handleComputerRestoreVerifyConnection(conn programConnection, bodyLen uint64, mounts *computerOperationRegistry, waits *waitingRunRegistry) error {
	if bodyLen != 0 {
		return errors.New("computer restore verification stream body must be empty")
	}
	if err := conn.SetReadDeadline(time.Now().Add(resumeAttachTimeout)); err != nil {
		return err
	}
	defer conn.SetReadDeadline(time.Time{})
	var request computerv0.VerifyComputerRestoreRequest
	if err := frameio.ReadProtoFrameBounded(conn, maxProgramControlFrameBytes, &request); err != nil {
		return fmt.Errorf("read computer restore verification: %w", err)
	}
	if err := verifyFrozenComputer(mounts, waits, request.GetIdentity()); err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(resumeAttachTimeout)); err != nil {
		return err
	}
	defer conn.SetWriteDeadline(time.Time{})
	return frameio.WriteProtoFrame(conn, &computerv0.VerifyComputerRestoreResponse{Identity: proto.Clone(request.GetIdentity()).(*computerv0.ComputerRestoreIdentity)})
}

// Verification observes the complete frozen set. It does not install fresh grants
// or release any user process; activation owns those transitions separately.
func verifyFrozenComputer(mounts *computerOperationRegistry, waits *waitingRunRegistry, identity *computerv0.ComputerRestoreIdentity) error {
	if mounts == nil || waits == nil || identity == nil || strings.TrimSpace(identity.GetComputerId()) == "" || strings.TrimSpace(identity.GetSourceComputerInstanceId()) == "" || identity.GetWriterGeneration() <= 0 || strings.TrimSpace(identity.GetCheckpointId()) == "" {
		return errors.New("computer restore verification identity is incomplete")
	}

	// Respect admission's lifecycle -> finalization -> registry order while
	// verifying that the mounted entry did not change during lock acquisition.
	mounts.mu.Lock()
	var entry *computerMountEntry
	for _, candidate := range mounts.entries {
		if candidate == nil || (entry != nil && entry != candidate) {
			mounts.mu.Unlock()
			return errors.New("computer restore has ambiguous mounted identity")
		}
		entry = candidate
	}
	mounts.mu.Unlock()
	if entry != nil {
		entry.lifecycleMu.Lock()
		defer entry.lifecycleMu.Unlock()
		entry.finalizationMu.Lock()
		defer entry.finalizationMu.Unlock()
	}
	mounts.mu.Lock()
	defer mounts.mu.Unlock()
	var current *computerMountEntry
	for _, candidate := range mounts.entries {
		if candidate == nil || candidate.retired || (current != nil && current != candidate) {
			return errors.New("computer restore has ambiguous mounted identity")
		}
		current = candidate
	}
	if current != entry {
		return errors.New("computer restore mounted identity changed during verification")
	}
	waits.mu.Lock()
	defer waits.mu.Unlock()
	return verifyFrozenComputerLocked(mounts, waits, entry, identity)
}

// Caller holds the physical lifecycle/control locks and both registry locks.
func verifyFrozenComputerLocked(mounts *computerOperationRegistry, waits *waitingRunRegistry, entry *computerMountEntry, identity *computerv0.ComputerRestoreIdentity) error {
	if len(waits.slots) != len(identity.Runs) || len(mounts.programClaims) != len(identity.Runs) {
		return errors.New("computer restore verification membership differs from the frozen guest")
	}
	if entry != nil {
		if entry.stopping || mounts.preparedRuntime != nil || entry.computerID != identity.ComputerId || entry.computerInstanceID != identity.SourceComputerInstanceId || entry.writerGeneration != identity.WriterGeneration {
			return errors.New("computer restore mounted identity differs from capture")
		}
		entry.processesMu.Lock()
		busy := entry.processAdmissions != 0 || entry.recoveryRequired
		entry.processesMu.Unlock()
		if busy || entry.hasUnreleasedCommands() {
			return errors.New("computer restore contains an unreconciled command")
		}
	} else {
		prepared := mounts.preparedRuntime
		if len(identity.Runs) != 0 || prepared == nil || prepared.computerID != identity.ComputerId || prepared.computerInstanceID != identity.SourceComputerInstanceId || prepared.writerGeneration != identity.WriterGeneration {
			return errors.New("computer restore prepared identity differs from capture")
		}
	}
	seen := make(map[string]bool, len(identity.Runs))
	for _, member := range identity.Runs {
		if member == nil || strings.TrimSpace(member.RunId) == "" || member.AttemptNumber == 0 || strings.TrimSpace(member.RunWaitId) == "" || strings.TrimSpace(member.RunLeaseId) == "" || strings.TrimSpace(member.CorrelationId) == "" || seen[member.RunId] {
			return errors.New("computer restore captured member is invalid")
		}
		seen[member.RunId] = true
		slot := waits.slots[member.RunWaitId]
		if slot == nil || !slot.frozen || slot.runID != member.RunId || slot.attemptNumber != member.AttemptNumber || slot.runLeaseID != member.RunLeaseId || slot.checkpointID != identity.CheckpointId || slot.correlationID != member.CorrelationId || slot.accepted != nil || slot.appliedDecision != nil || slot.appliedAck != nil || slot.granted != nil {
			return errors.New("computer restore member differs from its frozen wait")
		}
		matched := false
		for _, claim := range mounts.programClaims {
			if claim == nil || claim.entry != entry || claim.authority == nil {
				continue
			}
			fence := claim.authority.GetFence()
			if fence.GetRunId() == member.RunId && fence.GetAttemptNumber() == member.AttemptNumber && fence.GetRunLeaseId() == member.RunLeaseId && fence.GetComputerInstanceId() == identity.SourceComputerInstanceId && fence.GetComputerId() == identity.ComputerId && fence.GetWriterGeneration() == identity.WriterGeneration {
				if matched {
					return errors.New("computer restore contains duplicate program claims")
				}
				matched = true
			}
		}
		if !matched {
			return errors.New("computer restore member has no matching frozen program claim")
		}
	}
	return nil
}
