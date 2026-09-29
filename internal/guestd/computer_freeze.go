package guestd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
)

// flushComputerDisk writes every dirty Guest page to disk before the freeze is
// acknowledged. It is system-wide and not interruptible by the connection.
var flushComputerDisk = func() { syscall.Sync() }

// Member control owners freeze their cgroups and drain output. This connection
// seals physical admission and acknowledges only the complete frozen set; it
// never acknowledges a partial pause or independently resumes a member.
func handleComputerFreezeConnection(ctx context.Context, conn programConnection, bodyLen uint64, mounts *computerOperationRegistry, waits *waitingRunRegistry) error {
	if bodyLen != 0 || mounts == nil || waits == nil {
		return errors.New("computer freeze requires an empty stream body and registries")
	}
	ctx, cancel := context.WithTimeout(ctx, resumeAttachTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := conn.SetReadDeadline(time.Now().Add(resumeAttachTimeout)); err != nil {
		return err
	}
	defer conn.SetReadDeadline(time.Time{})
	var request computerv0.FreezeComputerRequest
	if err := frameio.ReadProtoFrameBounded(conn, maxProgramControlFrameBytes, &request); err != nil {
		return fmt.Errorf("read computer freeze: %w", err)
	}
	if err := mounts.sealComputerCapture(&request, time.Now); err != nil {
		return err
	}
	for {
		identity, changed, err := waits.computerCaptureProgress(&request)
		if err != nil {
			return err
		}
		if identity != nil {
			if err := verifyFrozenComputer(mounts, waits, identity); err != nil {
				return err
			}
			// Disk-only replacement discards Guest RAM, including dirty pages.
			// Flush even when there are no Run members to perform a pause.
			flushComputerDisk()
			if err := conn.SetWriteDeadline(time.Now().Add(resumeAttachTimeout)); err != nil {
				return err
			}
			defer conn.SetWriteDeadline(time.Time{})
			return frameio.WriteProtoFrame(conn, &computerv0.FreezeComputerResponse{Identity: identity, DesiredVersion: request.DesiredVersion, MembershipRevision: request.MembershipRevision})
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// A missing registration means the member's pause command has not arrived yet.
// A different registration is a rejected operation, never a partial proof.
func (r *waitingRunRegistry) computerCaptureProgress(request *computerv0.FreezeComputerRequest) (*computerv0.ComputerRestoreIdentity, <-chan struct{}, error) {
	// A newly sealed capture retires applied receipts from older captures.
	r.retireAppliedCaptures(request.CheckpointId)
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := r.changeChannelLocked()
	expected := make(map[string]*computerv0.ComputerCaptureRun, len(request.Runs))
	for _, member := range request.Runs {
		if expected[member.RunWaitId] != nil {
			return nil, changed, errors.New("computer capture has duplicate wait identities")
		}
		expected[member.RunWaitId] = member
	}
	for id := range r.slots {
		if expected[id] == nil {
			return nil, changed, errors.New("computer capture contains an unexpected wait")
		}
	}
	identity := &computerv0.ComputerRestoreIdentity{ComputerId: request.ComputerId, SourceComputerInstanceId: request.ComputerInstanceId, WriterGeneration: request.WriterGeneration, CheckpointId: request.CheckpointId, Runs: make([]*computerv0.CapturedRun, 0, len(request.Runs))}
	pending := false
	for _, member := range request.Runs {
		slot := r.slots[member.RunWaitId]
		if slot == nil {
			pending = true
			continue
		}
		if slot.runID != member.RunId || slot.attemptNumber != member.AttemptNumber || slot.runLeaseID != member.RunLeaseId || slot.checkpointID != request.CheckpointId || slot.checkpointRequestVersion != request.DesiredVersion || strings.TrimSpace(slot.correlationID) == "" || slot.accepted != nil || slot.granted != nil || slot.appliedAck != nil || slot.appliedDecision != nil {
			return nil, changed, errors.New("computer capture wait authority differs")
		}
		if !slot.frozen {
			pending = true
			continue
		}
		identity.Runs = append(identity.Runs, &computerv0.CapturedRun{RunId: slot.runID, AttemptNumber: slot.attemptNumber, RunWaitId: member.RunWaitId, RunLeaseId: slot.runLeaseID, CorrelationId: slot.correlationID})
	}
	if pending {
		return nil, changed, nil
	}
	return identity, changed, nil
}

func (r *waitingRunRegistry) changeChannelLocked() <-chan struct{} {
	if r.changed == nil {
		r.changed = make(chan struct{})
	}
	return r.changed
}
func (r *waitingRunRegistry) notifyChangedLocked() {
	if r.changed != nil {
		close(r.changed)
	}
	r.changed = make(chan struct{})
}
