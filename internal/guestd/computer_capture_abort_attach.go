package guestd

import (
	"context"
	"errors"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

// Attachment prepares transport only. The complete abort activation owns thaw.
func handleComputerCaptureAbortAttach(ctx context.Context, conn programConnection, bodyLen uint64, r *computerOperationRegistry, waits *waitingRunRegistry) (bool, error) {
	if bodyLen != 0 || r == nil || waits == nil {
		return false, errors.New("capture abort attachment requires registries and an empty body")
	}
	ctx, cancel := context.WithTimeout(ctx, resumeAttachTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := conn.SetReadDeadline(time.Now().Add(resumeAttachTimeout)); err != nil {
		return false, err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(resumeAttachTimeout)); err != nil {
		return false, err
	}
	var q computerv0.ComputerCaptureAbortAttachRequest
	if err := frameio.ReadProtoFrameBounded(conn, maxProgramControlFrameBytes, &q); err != nil {
		return false, err
	}
	r.captureAbortMu.Lock()
	defer r.captureAbortMu.Unlock()
	if err := r.parkCaptureAbortStream(waits, &q, conn, time.Now()); err != nil {
		return false, err
	}
	response := &computerv0.ComputerCaptureAbortAttachResponse{CheckpointId: q.CheckpointId, AbortDesiredVersion: q.AbortDesiredVersion, Member: q.Member, AttachSequence: q.AttachSequence}
	if err := frameio.WriteProtoFrame(conn, response); err != nil {
		return false, err
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return false, err
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		return false, err
	}
	if !stop() {
		return false, ctx.Err()
	}
	return true, nil
}

// captureAbortMu serializes preparation with whole-set activation and its replay.
func (r *computerOperationRegistry) parkCaptureAbortStream(waits *waitingRunRegistry, q *computerv0.ComputerCaptureAbortAttachRequest, conn programConnection, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	waits.mu.Lock()
	defer waits.mu.Unlock()
	installed := r.captureAbort
	if q == nil || q.Member == nil || q.AttachSequence == 0 || conn == nil || installed == nil || installed.activated || installed.capture.CheckpointId != q.CheckpointId || installed.version != q.AbortDesiredVersion {
		return errors.New("capture abort attachment differs from installed abort")
	}
	member := installed.members[q.Member.RunId]
	if member == nil || !proto.Equal(member.identity, q.Member) || member.cancelled || member.released || member.claim == nil || member.claim.stopRequested || member.slot == nil || waits.slots[q.Member.RunWaitId] != member.slot {
		return errors.New("capture abort attachment member is not held")
	}
	if err := validateComputerRunAuthority(member.claim.entry, q.Authority, now); err != nil {
		return err
	}
	if !computerRunAuthorityEqualExceptExpiry(member.claim.authority, q.Authority) || q.Authority.Fence.ExpiresAtUnixNano > member.claim.authority.Fence.ExpiresAtUnixNano {
		return errors.New("capture abort attachment grant differs")
	}
	slot := member.slot
	if q.AttachSequence <= slot.abortSequence {
		return errors.New("capture abort attachment sequence is stale")
	}
	if slot.abortStream != nil {
		_ = slot.abortStream.Close()
	}
	slot.abortSequence, slot.abortStream = q.AttachSequence, conn
	return nil
}
