package guestd

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
	"time"
)

func handleComputerRestoreInstallation(conn programConnection, bodyLen uint64, mounts *computerOperationRegistry, waits *waitingRunRegistry, activate bool) error {
	if bodyLen != 0 {
		return errors.New("restore installation body must be empty")
	}
	if err := conn.SetReadDeadline(time.Now().Add(resumeAttachTimeout)); err != nil {
		return err
	}
	defer conn.SetReadDeadline(time.Time{})
	var request computerv0.ComputerRestoreInstallation
	if err := frameio.ReadProtoFrameBounded(conn, maxProgramControlFrameBytes, &request); err != nil {
		return err
	}
	if err := mounts.installComputerRestore(waits, &request, activate, time.Now); err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(resumeAttachTimeout)); err != nil {
		return err
	}
	defer conn.SetWriteDeadline(time.Time{})
	return frameio.WriteProtoFrame(conn, &computerv0.ComputerRestoreInstallationResponse{Installation: &request})
}

// Install is atomic for the complete captured set. Activate is sent only after
// the Control Plane durably acknowledges this installation; it opens admission
// but does not deliver a decision or unfreeze an individual Program.
func (r *computerOperationRegistry) installComputerRestore(waits *waitingRunRegistry, request *computerv0.ComputerRestoreInstallation, activate bool, clock func() time.Time) error {
	if waits == nil || request == nil || request.Envelope == nil || request.CheckpointId == "" || request.DesiredVersion <= 0 {
		return errors.New("restore installation is incomplete")
	}
	envelope := request.Envelope
	entry, release, ok := r.acquireExact(envelope.ComputerInstanceId, envelope.ComputerId, envelope.ChannelToken, envelope.WriterGeneration)
	if !ok {
		return errors.New("restore installation does not match physical authority")
	}
	defer release()
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	waits.mu.Lock()
	defer waits.mu.Unlock()
	if r.entries[envelope.ComputerInstanceId] != entry || entry.retired || entry.stopping || r.restoredMaterialization == nil || r.restoredMaterialization.RestoredCheckpointId != request.CheckpointId || !proto.Equal(r.restoredMaterialization.Envelope, envelope) {
		return errors.New("restore materialization is not current")
	}
	entry.processesMu.Lock()
	unavailable := entry.recoveryRequired
	entry.processesMu.Unlock()
	if unavailable {
		return errors.New("restored computer requires recovery")
	}
	if r.restoreInstallation != nil {
		if !proto.Equal(r.restoreInstallation, request) {
			return errors.New("restore installation conflicts with retained receipt")
		}
		if r.restoreActivated {
			return nil
		}
	} else if activate {
		return errors.New("restore grants have not been installed")
	}
	capture := r.captureRequest
	if capture == nil || capture.CheckpointId != request.CheckpointId || len(request.Grants) != len(capture.Runs) || len(request.Grants) != len(r.programClaims) || len(request.Grants) != len(waits.slots) {
		return errors.New("restore installation membership differs")
	}
	if clock == nil {
		clock = time.Now
	}
	now := clock()
	grants := make(map[string]*computerv0.ComputerRunAuthority, len(request.Grants))
	for _, grant := range request.Grants {
		if err := validateComputerRunAuthority(entry, grant, now); err != nil {
			return err
		}
		id := grant.Fence.RunId
		if grants[id] != nil {
			return errors.New("restore installation duplicates a Run")
		}
		grants[id] = grant
	}
	claims := make([]*managedProgramClaim, 0, len(capture.Runs))
	seen := make(map[string]bool, len(capture.Runs))
	for _, member := range capture.Runs {
		if member == nil || seen[member.RunId] {
			return errors.New("captured membership is invalid")
		}
		seen[member.RunId] = true
		grant := grants[member.RunId]
		if grant == nil || grant.Fence.AttemptNumber != member.AttemptNumber {
			return errors.New("restore grant is missing captured member")
		}
		claim := r.programClaimLocked(entry, grant)
		slot := waits.slots[member.RunWaitId]
		if claim == nil || slot == nil || !slot.frozen || slot.runID != member.RunId || slot.attemptNumber != member.AttemptNumber || slot.runLeaseID != member.RunLeaseId || slot.checkpointID != request.CheckpointId || slot.granted != nil || slot.accepted != nil || slot.appliedDecision != nil || slot.appliedAck != nil {
			return errors.New("restore member is not frozen")
		}
		if r.restoreInstallation == nil {
			old, next := claim.authority.GetFence(), grant.Fence
			if old.GetComputerInstanceId() != capture.ComputerInstanceId || old.GetWriterGeneration() != capture.WriterGeneration || old.GetRunLeaseId() != member.RunLeaseId || next.RunLeaseId == old.GetRunLeaseId() || next.LeaseSequence <= old.GetLeaseSequence() {
				return errors.New("restore grant does not advance captured authority")
			}
		} else if !computerRunAuthoritiesEqual(claim.authority, grant) {
			return errors.New("installed restore grant changed")
		}
		claims = append(claims, claim)
	}
	if r.restoreInstallation == nil {
		for _, claim := range claims {
			claim.authority = proto.Clone(grants[claim.authority.Fence.RunId]).(*computerv0.ComputerRunAuthority)
			claim.previousExpiry = 0
		}
		r.restoreInstallation = proto.Clone(request).(*computerv0.ComputerRestoreInstallation)
	}
	if activate {
		r.captureRequest = nil
		r.restoreActivated = true
	}
	return nil
}
