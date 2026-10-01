package guestd

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
	"io"
	"strings"
	"time"
)

func validateComputerRunAuthority(entry *computerMountEntry, authority *computerv0.ComputerRunAuthority, now time.Time) error {
	if entry == nil || authority == nil || authority.GetFence() == nil {
		return errors.New("computer run authority is required")
	}
	fence := authority.GetFence()
	if strings.TrimSpace(fence.GetWorkerHostId()) == "" ||
		fence.GetWorkerEpoch() <= 0 ||
		strings.TrimSpace(fence.GetComputerInstanceId()) == "" ||
		strings.TrimSpace(fence.GetVmPlatformId()) == "" ||
		strings.TrimSpace(fence.GetComputerId()) == "" ||
		strings.TrimSpace(fence.GetRunId()) == "" ||
		fence.GetAttemptNumber() == 0 ||
		strings.TrimSpace(fence.GetRunLeaseId()) == "" ||
		fence.GetLeaseSequence() <= 0 ||
		fence.GetWriterGeneration() <= 0 ||
		fence.GetExpiresAtUnixNano() <= now.UnixNano() ||
		strings.TrimSpace(fence.GetBaseComputerDiskVersionId()) == "" ||
		strings.TrimSpace(authority.GetChannelCredential()) == "" ||
		strings.TrimSpace(authority.GetWriteCapability()) == "" {
		return errors.New("computer run authority is incomplete or expired")
	}
	if fence.GetWriterGeneration() != int64(entry.currentWriterGeneration()) ||
		fence.GetComputerId() != entry.computerID ||
		fence.GetComputerInstanceId() != entry.computerInstanceID ||
		subtle.ConstantTimeCompare([]byte(authority.GetChannelCredential()), []byte(entry.channelCredential)) != 1 {
		return errors.New("computer run authority does not match the mounted runtime")
	}
	return nil
}

func handleProgramResumeGrantConnection(
	conn programConnection,
	bodyLen uint64,
	mounts *computerOperationRegistry,
	waits *waitingRunRegistry,
	clock func() time.Time,
) error {
	if bodyLen != 0 {
		return errors.New("program resume grant stream body must be empty")
	}
	if err := conn.SetReadDeadline(time.Now().Add(resumeAttachTimeout)); err != nil {
		return fmt.Errorf("bound program resume grant read: %w", err)
	}
	defer conn.SetReadDeadline(time.Time{})
	var request computerv0.GrantProgramResumeRequest
	if err := frameio.ReadProtoFrameBounded(conn, maxProgramControlFrameBytes, &request); err != nil {
		return fmt.Errorf("read program resume grant: %w", err)
	}
	authority := request.GetAuthority()
	if authority == nil || authority.GetFence() == nil {
		return errors.New("program resume grant authority is required")
	}
	fence := authority.GetFence()
	entry, release, ok := mounts.acquireAuthorityMount(
		fence.GetComputerInstanceId(), fence.GetComputerId(), authority.GetChannelCredential(),
	)
	if !ok {
		return errors.New("program resume grant does not match the mounted runtime")
	}
	defer release()
	if clock == nil {
		clock = time.Now
	}
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	if mounts.captureSealed() {
		return errors.New("computer capture has sealed program resume")
	}
	if err := mounts.installResumedProgramAuthorityLocked(entry, authority, clock); err != nil {
		return err
	}
	grant, err := waits.resumeAttachment(fence, &request)
	if err != nil {
		return err
	}

	installed := proto.Clone(authority).(*computerv0.ComputerRunAuthority)
	if err := waits.grantProgramResume(&programResumeGrant{
		attach: grant,
		lock:   entry.finalizationMu.Lock,
		unlock: entry.finalizationMu.Unlock,
		valid: func(now time.Time) bool {
			entry.processesMu.Lock()
			live := !entry.recoveryRequired
			entry.processesMu.Unlock()
			mounts.mu.Lock()
			claim := mounts.programClaimLocked(entry, installed)
			current := claim != nil && computerRunAuthoritiesEqual(claim.authority, installed)
			mounts.mu.Unlock()
			return live && current && !mounts.captureSealed() && installed.GetFence().GetExpiresAtUnixNano() > now.UnixNano()
		},
	}); err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(resumeAttachTimeout)); err != nil {
		return fmt.Errorf("bound program resume grant response: %w", err)
	}
	defer conn.SetWriteDeadline(time.Time{})
	return frameio.WriteProtoFrame(conn, &computerv0.GrantProgramResumeResponse{
		Fence: proto.Clone(fence).(*computerv0.ComputerAuthorityFence), Attach: grant,
	})
}

func computerRunAuthorityEqualExceptExpiry(left, right *computerv0.ComputerRunAuthority) bool {
	if left == nil || right == nil || left.GetFence() == nil || right.GetFence() == nil {
		return false
	}
	leftCopy := proto.Clone(left).(*computerv0.ComputerRunAuthority)
	rightCopy := proto.Clone(right).(*computerv0.ComputerRunAuthority)
	leftCopy.GetFence().ExpiresAtUnixNano = 0
	rightCopy.GetFence().ExpiresAtUnixNano = 0
	return computerRunAuthoritiesEqual(leftCopy, rightCopy)
}

func computerRunAuthoritiesEqual(left, right *computerv0.ComputerRunAuthority) bool {
	if left == nil || right == nil {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(left.GetChannelCredential()), []byte(right.GetChannelCredential())) != 1 ||
		subtle.ConstantTimeCompare([]byte(left.GetWriteCapability()), []byte(right.GetWriteCapability())) != 1 {
		return false
	}
	leftCopy := proto.Clone(left).(*computerv0.ComputerRunAuthority)
	rightCopy := proto.Clone(right).(*computerv0.ComputerRunAuthority)
	leftCopy.ChannelCredential = ""
	leftCopy.WriteCapability = ""
	rightCopy.ChannelCredential = ""
	rightCopy.WriteCapability = ""
	return proto.Equal(leftCopy, rightCopy)
}

func handleComputerAuthorityRenewConnection(_ context.Context, conn io.ReadWriter, registry *computerOperationRegistry) error {
	if err := handleComputerAuthorityRenew(conn, registry, time.Now); err != nil {
		if writeErr := frameio.WriteProtoFrame(conn, &computerv0.RenewComputerAuthorityResponse{Error: err.Error()}); writeErr != nil {
			return errors.Join(err, fmt.Errorf("write computer authority renewal failure: %w", writeErr))
		}
	}
	return nil
}

func handleComputerAuthorityRenew(conn io.ReadWriter, registry *computerOperationRegistry, clock func() time.Time) error {
	var request computerv0.RenewComputerAuthorityRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read computer authority renewal: %w", err)
	}
	previous := request.GetPrevious()
	if previous == nil || previous.GetFence() == nil {
		return errors.New("computer authority renewal previous authority is required")
	}
	fence := previous.GetFence()
	entry, release, ok := registry.acquireExact(
		fence.GetComputerInstanceId(),
		fence.GetComputerId(),
		previous.GetChannelCredential(),
		uint64(fence.GetWriterGeneration()),
	)
	if !ok {
		return errors.New("computer authority renewal does not match the mounted runtime")
	}
	defer release()
	if clock == nil {
		clock = time.Now
	}
	renewed, err := registry.renewCurrentComputerRunAuthority(entry, &request, clock)
	if err != nil {
		return err
	}
	if err := frameio.WriteProtoFrame(conn, &computerv0.RenewComputerAuthorityResponse{Fence: renewed}); err != nil {
		return fmt.Errorf("write computer authority renewal response: %w", err)
	}
	return nil
}

// Caller holds the physical lifecycle locks; each retained Program owns its grant.
func (r *computerOperationRegistry) installResumedProgramAuthorityLocked(entry *computerMountEntry, authority *computerv0.ComputerRunAuthority, clock func() time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if clock == nil {
		clock = time.Now
	}
	if err := validateComputerRunAuthority(entry, authority, clock()); err != nil {
		return err
	}
	claim := r.programClaimLocked(entry, authority)
	if claim == nil {
		return errors.New("resumed program claim is not active")
	}
	entry.processesMu.Lock()
	unavailable := entry.recoveryRequired
	entry.processesMu.Unlock()
	if unavailable {
		return errors.New("computer requires recovery")
	}
	if computerRunAuthoritiesEqual(claim.authority, authority) {
		return nil
	}
	previous, next := claim.authority.GetFence(), authority.GetFence()
	if previous.GetComputerInstanceId() == next.GetComputerInstanceId() || previous.GetWriterGeneration() >= next.GetWriterGeneration() || previous.GetRunLeaseId() == next.GetRunLeaseId() || previous.GetLeaseSequence() >= next.GetLeaseSequence() {
		return errors.New("program resume grant does not advance restored authority")
	}
	claim.authority = proto.Clone(authority).(*computerv0.ComputerRunAuthority)
	claim.previousExpiry = 0
	return nil
}

func (r *computerOperationRegistry) renewCurrentComputerRunAuthority(entry *computerMountEntry, request *computerv0.RenewComputerAuthorityRequest, clock func() time.Time) (*computerv0.ComputerAuthorityFence, error) {
	if request == nil || request.GetPrevious() == nil || request.GetPrevious().GetFence() == nil {
		return nil, errors.New("previous program authority is required")
	}
	previous := request.GetPrevious()
	fence := previous.GetFence()
	if request.GetNewExpiresAtUnixNano() <= fence.GetExpiresAtUnixNano() {
		return nil, errors.New("renewed program authority expiry must advance")
	}
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries[fence.GetComputerInstanceId()] != entry || !computerEntryMatches(entry, fence.GetComputerInstanceId(), fence.GetComputerId(), previous.GetChannelCredential()) || entry.currentWriterGeneration() != uint64(fence.GetWriterGeneration()) {
		return nil, errors.New("program authority is not current for the computer instance")
	}
	claim := r.programClaimLocked(entry, previous)
	if claim == nil {
		return nil, errors.New("program claim is not active")
	}
	if clock == nil {
		clock = time.Now
	}
	if claim.authority.GetFence().GetExpiresAtUnixNano() <= clock().UnixNano() {
		return nil, errors.New("program authority expired")
	}
	currentExpiry := claim.authority.GetFence().GetExpiresAtUnixNano()
	switch {
	case computerRunAuthoritiesEqual(claim.authority, previous):
		claim.previousExpiry = currentExpiry
		claim.authority.Fence.ExpiresAtUnixNano = request.GetNewExpiresAtUnixNano()
	case claim.previousExpiry == fence.GetExpiresAtUnixNano() && currentExpiry == request.GetNewExpiresAtUnixNano() && computerRunAuthorityEqualExceptExpiry(claim.authority, previous):
	default:
		return nil, errors.New("previous program authority does not match active claim")
	}
	return proto.Clone(claim.authority.Fence).(*computerv0.ComputerAuthorityFence), nil
}
