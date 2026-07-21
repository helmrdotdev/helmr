package guestd

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"google.golang.org/protobuf/proto"
)

func validateWorkspaceRunAuthority(entry *workspaceMountEntry, authority *workspacev0.WorkspaceRunAuthority, now time.Time) error {
	if entry == nil || authority == nil || authority.GetFence() == nil {
		return errors.New("Workspace Run authority is required")
	}
	fence := authority.GetFence()
	if strings.TrimSpace(fence.GetWorkerInstanceId()) == "" ||
		fence.GetWorkerEpoch() <= 0 ||
		strings.TrimSpace(fence.GetRuntimeInstanceId()) == "" ||
		strings.TrimSpace(fence.GetRuntimeIdentityId()) == "" ||
		strings.TrimSpace(fence.GetWorkspaceId()) == "" ||
		strings.TrimSpace(fence.GetWorkspaceMountId()) == "" ||
		strings.TrimSpace(fence.GetRunId()) == "" ||
		fence.GetAttemptNumber() == 0 ||
		strings.TrimSpace(fence.GetRunLeaseId()) == "" ||
		fence.GetLeaseSequence() <= 0 ||
		strings.TrimSpace(fence.GetWorkspaceLeaseId()) == "" ||
		fence.GetOwnershipGeneration() <= 0 ||
		fence.GetWriterGeneration() <= 0 ||
		fence.GetMountFencingGeneration() <= 0 ||
		fence.GetExpiresAtUnixNano() <= now.UnixNano() ||
		strings.TrimSpace(fence.GetBaseWorkspaceVersionId()) == "" ||
		strings.TrimSpace(authority.GetChannelToken()) == "" ||
		strings.TrimSpace(authority.GetWriteCapability()) == "" {
		return errors.New("Workspace Run authority is incomplete or expired")
	}
	if fence.GetWorkspaceId() != entry.workspaceID ||
		fence.GetWorkspaceMountId() != entry.workspaceMountID ||
		fence.GetRuntimeInstanceId() != entry.runtimeInstanceID ||
		subtle.ConstantTimeCompare([]byte(authority.GetChannelToken()), []byte(entry.channelToken)) != 1 {
		return errors.New("Workspace Run authority does not match the mounted runtime")
	}
	return nil
}

func (entry *workspaceMountEntry) installWorkspaceRunAuthority(authority *workspacev0.WorkspaceRunAuthority, now time.Time) error {
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	return entry.installWorkspaceRunAuthorityLocked(authority, now)
}

func (entry *workspaceMountEntry) installWorkspaceRunAuthorityLocked(authority *workspacev0.WorkspaceRunAuthority, now time.Time) error {
	if err := validateWorkspaceRunAuthority(entry, authority, now); err != nil {
		return err
	}
	entry.authorityMu.Lock()
	defer entry.authorityMu.Unlock()
	mountGeneration := uint64(authority.GetFence().GetMountFencingGeneration())
	if entry.authority == nil {
		if authority.GetFence().GetBaseWorkspaceVersionId() != entry.baseVersionID {
			return errors.New("Workspace Run authority base version does not match the mounted frontier")
		}
		if mountGeneration < entry.currentFencingGeneration() {
			return errors.New("Workspace Run authority mount fencing generation is stale")
		}
	} else {
		entry.processesMu.Lock()
		finalizing := entry.finalizing
		entry.processesMu.Unlock()
		if !finalizing {
			return errors.New("Workspace Run authority is already installed")
		}
		if authority.GetFence().GetWriterGeneration() <= entry.authority.GetFence().GetWriterGeneration() {
			return errors.New("Workspace Run authority does not advance the writer generation")
		}
		if mountGeneration <= entry.currentFencingGeneration() {
			return errors.New("Workspace Run authority does not advance the mount fencing generation")
		}
		if err := entry.pruneWorkspaceFinalizationState(); err != nil {
			return err
		}
		entry.baseVersionID = authority.GetFence().GetBaseWorkspaceVersionId()
		entry.processesMu.Lock()
		entry.finalizing = false
		entry.processesMu.Unlock()
	}
	entry.setFencingGeneration(mountGeneration)
	entry.authority = proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority)
	entry.previousExpiry = 0
	return nil
}

func (entry *workspaceMountEntry) pruneWorkspaceFinalizationState() error {
	if entry.finalizationRoot == "" {
		return nil
	}
	for _, name := range []string{workspaceFinalizationJournalName, workspaceCaptureArtifactName} {
		if err := os.Remove(filepath.Join(entry.finalizationRoot, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("prune Workspace finalization state: %w", err)
		}
	}
	return syncDirectory(entry.finalizationRoot)
}

func (entry *workspaceMountEntry) renewWorkspaceRunAuthority(request *workspacev0.RenewWorkspaceAuthorityRequest, now time.Time) (*workspacev0.WorkspaceAuthorityFence, error) {
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	return entry.renewWorkspaceRunAuthorityLocked(request, now)
}

func (entry *workspaceMountEntry) renewWorkspaceRunAuthorityLocked(request *workspacev0.RenewWorkspaceAuthorityRequest, now time.Time) (*workspacev0.WorkspaceAuthorityFence, error) {
	if request == nil || request.GetPrevious() == nil || request.GetPrevious().GetFence() == nil {
		return nil, errors.New("previous Workspace Run authority is required")
	}
	if request.GetNewExpiresAtUnixNano() <= request.GetPrevious().GetFence().GetExpiresAtUnixNano() {
		return nil, errors.New("renewed Workspace Run authority expiry must advance")
	}
	entry.processesMu.Lock()
	finalizing := entry.finalizing
	entry.processesMu.Unlock()
	if finalizing {
		return nil, errors.New("Workspace Run authority cannot be renewed after finalization begins")
	}
	entry.authorityMu.Lock()
	defer entry.authorityMu.Unlock()
	if entry.authority == nil || entry.authority.GetFence() == nil {
		return nil, errors.New("Workspace Run authority is not installed")
	}
	currentExpiry := entry.authority.GetFence().GetExpiresAtUnixNano()
	if currentExpiry <= now.UnixNano() {
		return nil, errors.New("Workspace Run authority expired")
	}
	previousExpiry := request.GetPrevious().GetFence().GetExpiresAtUnixNano()
	switch {
	case workspaceRunAuthoritiesEqual(entry.authority, request.GetPrevious()):
		entry.previousExpiry = currentExpiry
		entry.authority.GetFence().ExpiresAtUnixNano = request.GetNewExpiresAtUnixNano()
	case entry.previousExpiry == previousExpiry &&
		currentExpiry == request.GetNewExpiresAtUnixNano() &&
		workspaceRunAuthorityEqualExceptExpiry(entry.authority, request.GetPrevious()):
	default:
		return nil, errors.New("previous Workspace Run authority does not match current authority")
	}
	return proto.Clone(entry.authority.GetFence()).(*workspacev0.WorkspaceAuthorityFence), nil
}

func (registry *workspaceOperationRegistry) renewCurrentWorkspaceRunAuthority(entry *workspaceMountEntry, request *workspacev0.RenewWorkspaceAuthorityRequest, now time.Time) (*workspacev0.WorkspaceAuthorityFence, error) {
	if request == nil || request.GetPrevious() == nil || request.GetPrevious().GetFence() == nil {
		return nil, errors.New("previous Workspace Run authority is required")
	}
	previous := request.GetPrevious()
	fence := previous.GetFence()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	if !registry.currentExactLocked(
		entry,
		fence.GetWorkspaceMountId(),
		fence.GetWorkspaceId(),
		previous.GetChannelToken(),
		uint64(fence.GetMountFencingGeneration()),
	) {
		return nil, errors.New("Workspace Run authority is not current for the Workspace Mount")
	}
	return entry.renewWorkspaceRunAuthorityLocked(request, now)
}

func workspaceRunAuthorityEqualExceptExpiry(left, right *workspacev0.WorkspaceRunAuthority) bool {
	if left == nil || right == nil || left.GetFence() == nil || right.GetFence() == nil {
		return false
	}
	leftCopy := proto.Clone(left).(*workspacev0.WorkspaceRunAuthority)
	rightCopy := proto.Clone(right).(*workspacev0.WorkspaceRunAuthority)
	leftCopy.GetFence().ExpiresAtUnixNano = 0
	rightCopy.GetFence().ExpiresAtUnixNano = 0
	return workspaceRunAuthoritiesEqual(leftCopy, rightCopy)
}

func workspaceRunAuthoritiesEqual(left, right *workspacev0.WorkspaceRunAuthority) bool {
	if left == nil || right == nil {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(left.GetChannelToken()), []byte(right.GetChannelToken())) != 1 ||
		subtle.ConstantTimeCompare([]byte(left.GetWriteCapability()), []byte(right.GetWriteCapability())) != 1 {
		return false
	}
	leftCopy := proto.Clone(left).(*workspacev0.WorkspaceRunAuthority)
	rightCopy := proto.Clone(right).(*workspacev0.WorkspaceRunAuthority)
	leftCopy.ChannelToken = ""
	leftCopy.WriteCapability = ""
	rightCopy.ChannelToken = ""
	rightCopy.WriteCapability = ""
	return proto.Equal(leftCopy, rightCopy)
}

func handleWorkspaceAuthorityRenewConnection(_ context.Context, conn io.ReadWriter, registry *workspaceOperationRegistry) error {
	if err := handleWorkspaceAuthorityRenew(conn, registry, time.Now); err != nil {
		if writeErr := frameio.WriteProtoFrame(conn, &workspacev0.RenewWorkspaceAuthorityResponse{Error: err.Error()}); writeErr != nil {
			return errors.Join(err, fmt.Errorf("write Workspace authority renewal failure: %w", writeErr))
		}
	}
	return nil
}

func handleWorkspaceAuthorityRenew(conn io.ReadWriter, registry *workspaceOperationRegistry, clock func() time.Time) error {
	var request workspacev0.RenewWorkspaceAuthorityRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read Workspace authority renewal: %w", err)
	}
	previous := request.GetPrevious()
	if previous == nil || previous.GetFence() == nil {
		return errors.New("Workspace authority renewal previous authority is required")
	}
	fence := previous.GetFence()
	entry, release, ok := registry.acquireExact(
		fence.GetWorkspaceMountId(),
		fence.GetWorkspaceId(),
		previous.GetChannelToken(),
		uint64(fence.GetMountFencingGeneration()),
	)
	if !ok {
		return errors.New("Workspace authority renewal does not match the mounted runtime")
	}
	defer release()
	if clock == nil {
		clock = time.Now
	}
	renewed, err := registry.renewCurrentWorkspaceRunAuthority(entry, &request, clock())
	if err != nil {
		return err
	}
	if err := frameio.WriteProtoFrame(conn, &workspacev0.RenewWorkspaceAuthorityResponse{Fence: renewed}); err != nil {
		return fmt.Errorf("write Workspace authority renewal response: %w", err)
	}
	return nil
}
