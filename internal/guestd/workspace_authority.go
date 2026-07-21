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

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/workspace"
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
		finalizing := entry.authorityState == workspaceAuthorityFinalizing
		entry.processesMu.Unlock()
		if !finalizing {
			return errors.New("Workspace Run authority is already installed")
		}
		journal, found, err := entry.readWorkspaceFinalizationJournal()
		if err != nil {
			return err
		}
		if !found || journal.Phase != "committed" || validateWorkspaceFinalizationBeginJournal(
			journal,
			entry.finalizationID,
			entry.finalizationKind,
			workspaceFinalizationFence(entry.authority.GetFence()),
		) != nil {
			return errors.New("Workspace finalization is not committed")
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
		entry.authorityState = workspaceAuthorityLive
		entry.finalizationID = ""
		entry.finalizationKind = ""
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
	journal, found, err := entry.readWorkspaceFinalizationJournal()
	if err != nil {
		return fmt.Errorf("read Workspace finalization state for pruning: %w", err)
	}
	if found && journal.Kind == workspace.FinalizationResetKind && strings.TrimSpace(journal.OperationID) != "" {
		operationID, err := uuid.Parse(journal.OperationID)
		if err != nil || operationID.String() != journal.OperationID {
			return errors.New("Workspace Reset journal operation ID is invalid")
		}
		if err := os.RemoveAll(entry.workspaceResetStagingPath(journal.OperationID)); err != nil {
			return fmt.Errorf("prune Workspace Reset staging tree: %w", err)
		}
		if err := syncDirectory(filepath.Dir(entry.workspaceRoot)); err != nil {
			return fmt.Errorf("sync pruned Workspace Reset staging tree: %w", err)
		}
	}
	for _, name := range []string{workspaceCaptureArtifactName, workspaceFinalizationJournalName} {
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
	finalizing := entry.authorityState == workspaceAuthorityFinalizing
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

func (registry *workspaceOperationRegistry) beginCurrentWorkspaceFinalization(
	entry *workspaceMountEntry,
	request *workspacev0.BeginWorkspaceFinalizationRequest,
	clock func() time.Time,
) (*workspacev0.BeginWorkspaceFinalizationResponse, error) {
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
		return nil, errors.New("Workspace finalization authority is not current for the Workspace Mount")
	}
	if clock == nil {
		clock = time.Now
	}
	return entry.beginWorkspaceFinalizationLocked(request, clock())
}

func (entry *workspaceMountEntry) beginWorkspaceFinalizationLocked(
	request *workspacev0.BeginWorkspaceFinalizationRequest,
	now time.Time,
) (*workspacev0.BeginWorkspaceFinalizationResponse, error) {
	if request == nil || request.GetPrevious() == nil || request.GetPrevious().GetFence() == nil {
		return nil, errors.New("previous Workspace Run authority is required")
	}
	rawOperationID := request.GetOperationId()
	operationID := strings.TrimSpace(rawOperationID)
	parsedOperationID, err := uuid.Parse(operationID)
	if err != nil || rawOperationID != operationID || parsedOperationID.String() != operationID {
		return nil, errors.New("Workspace finalization operation_id must be a canonical UUID")
	}
	kind := request.GetKind()
	if kind != workspace.FinalizationCaptureKind && kind != workspace.FinalizationResetKind {
		return nil, errors.New("Workspace finalization kind must be capture or reset")
	}
	previous := request.GetPrevious()
	previousExpiry := previous.GetFence().GetExpiresAtUnixNano()
	finalizationExpiry := request.GetFinalizationExpiresAtUnixNano()
	if finalizationExpiry <= previousExpiry || finalizationExpiry <= now.UnixNano() {
		return nil, errors.New("Workspace finalization expiry must advance and remain live")
	}
	if entry.finalizationRoot == "" {
		return nil, errors.New("Workspace finalization state is unavailable")
	}

	entry.authorityMu.Lock()
	defer entry.authorityMu.Unlock()
	if entry.authority == nil || entry.authority.GetFence() == nil {
		return nil, errors.New("Workspace Run authority is not installed")
	}
	currentExpiry := entry.authority.GetFence().GetExpiresAtUnixNano()
	entry.processesMu.Lock()
	defer entry.processesMu.Unlock()
	if entry.recoveryRequired {
		return nil, errors.New("Workspace finalization requires a recoverable Workspace")
	}

	switch entry.authorityState {
	case workspaceAuthorityLive:
		if !workspaceRunAuthoritiesEqual(entry.authority, previous) {
			return nil, errors.New("previous Workspace Run authority does not match current authority")
		}
		frozen := proto.Clone(entry.authority).(*workspacev0.WorkspaceRunAuthority)
		frozen.GetFence().ExpiresAtUnixNano = finalizationExpiry
		journal, found, err := entry.readWorkspaceFinalizationJournal()
		if err != nil {
			return nil, err
		}
		if found {
			if err := validateWorkspaceFinalizationBeginJournal(
				journal, operationID, kind, workspaceFinalizationFence(frozen.GetFence()),
			); err != nil || journal.Phase != "begun" {
				return nil, errors.New("Workspace finalization conflicts with retained state")
			}
		} else {
			if currentExpiry <= now.UnixNano() {
				return nil, errors.New("Workspace Run authority expired before finalization began")
			}
			journal = workspaceFinalizationJournal{
				Version: workspaceFinalizationJournalVersion,
				Kind:    kind, OperationID: operationID,
				Fence: workspaceFinalizationFence(frozen.GetFence()), Phase: "begun",
			}
			if err := entry.writeWorkspaceFinalizationJournal(journal); err != nil {
				return nil, err
			}
		}
		entry.authority = frozen
		entry.previousExpiry = previousExpiry
		entry.authorityState = workspaceAuthorityFinalizing
		entry.finalizationID = operationID
		entry.finalizationKind = kind
	case workspaceAuthorityFinalizing:
		if currentExpiry <= now.UnixNano() ||
			currentExpiry != finalizationExpiry ||
			entry.previousExpiry != previousExpiry ||
			entry.finalizationID != operationID ||
			entry.finalizationKind != kind ||
			!workspaceRunAuthorityEqualExceptExpiry(entry.authority, previous) {
			return nil, errors.New("Workspace finalization request does not match current finalization")
		}
		journal, found, err := entry.readWorkspaceFinalizationJournal()
		if err != nil {
			return nil, err
		}
		if !found || validateWorkspaceFinalizationBeginJournal(
			journal, operationID, kind, workspaceFinalizationFence(entry.authority.GetFence()),
		) != nil {
			return nil, errors.New("Workspace finalization state is unavailable or conflicting")
		}
	default:
		return nil, errors.New("Workspace Run authority state is invalid")
	}

	return &workspacev0.BeginWorkspaceFinalizationResponse{
		Fence:       proto.Clone(entry.authority.GetFence()).(*workspacev0.WorkspaceAuthorityFence),
		OperationId: operationID,
		Kind:        kind,
	}, nil
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

func handleWorkspaceFinalizationBeginConnection(ctx context.Context, conn io.ReadWriter, registry *workspaceOperationRegistry) error {
	if err := handleWorkspaceFinalizationBegin(ctx, conn, registry, time.Now); err != nil {
		if writeErr := frameio.WriteProtoFrame(conn, &workspacev0.BeginWorkspaceFinalizationResponse{Error: err.Error()}); writeErr != nil {
			return errors.Join(err, fmt.Errorf("write Workspace finalization Begin failure: %w", writeErr))
		}
	}
	return nil
}

func handleWorkspaceFinalizationBegin(
	ctx context.Context,
	conn io.ReadWriter,
	registry *workspaceOperationRegistry,
	clock func() time.Time,
) error {
	var request workspacev0.BeginWorkspaceFinalizationRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read Workspace finalization Begin: %w", err)
	}
	previous := request.GetPrevious()
	if previous == nil || previous.GetFence() == nil {
		return errors.New("Workspace finalization previous authority is required")
	}
	fence := previous.GetFence()
	entry, release, ok := registry.acquireExact(
		fence.GetWorkspaceMountId(),
		fence.GetWorkspaceId(),
		previous.GetChannelToken(),
		uint64(fence.GetMountFencingGeneration()),
	)
	if !ok {
		return errors.New("Workspace finalization does not match the mounted runtime")
	}
	defer release()
	if clock == nil {
		clock = time.Now
	}
	response, err := registry.beginCurrentWorkspaceFinalization(entry, &request, clock)
	if err != nil {
		return err
	}
	if err := frameio.WriteProtoFrame(conn, response); err != nil {
		return fmt.Errorf("write Workspace finalization Begin response: %w", err)
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
