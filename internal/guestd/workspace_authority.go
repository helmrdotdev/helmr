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

	"uuid"

	"github.com/helmrdotdev/helmr/internal/frameio"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

func validateWorkspaceRunAuthority(entry *workspaceMountEntry, authority *workspacev0.WorkspaceRunAuthority, now time.Time) error {
	if entry == nil || authority == nil || authority.GetFence() == nil {
		return errors.New("workspace run authority is required")
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
		return errors.New("workspace run authority is incomplete or expired")
	}
	if fence.GetWorkspaceId() != entry.workspaceID ||
		fence.GetWorkspaceMountId() != entry.workspaceMountID ||
		fence.GetRuntimeInstanceId() != entry.runtimeInstanceID ||
		subtle.ConstantTimeCompare([]byte(authority.GetChannelToken()), []byte(entry.channelToken)) != 1 {
		return errors.New("workspace run authority does not match the mounted runtime")
	}
	return nil
}

func handleProgramResumeGrantConnection(
	conn programConnection,
	bodyLen uint64,
	mounts *workspaceOperationRegistry,
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
	var request workspacev0.GrantProgramResumeRequest
	if err := frameio.ReadProtoFrameBounded(conn, maxProgramControlFrameBytes, &request); err != nil {
		return fmt.Errorf("read program resume grant: %w", err)
	}
	authority := request.GetAuthority()
	if authority == nil || authority.GetFence() == nil {
		return errors.New("program resume grant authority is required")
	}
	fence := authority.GetFence()
	entry, release, ok := mounts.acquireAuthorityMount(
		fence.GetWorkspaceMountId(), fence.GetWorkspaceId(), authority.GetChannelToken(),
	)
	if !ok {
		return errors.New("program resume grant does not match the mounted runtime")
	}
	defer release()
	if clock == nil {
		clock = time.Now
	}
	entry.turnCommitMu.Lock()
	defer entry.turnCommitMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	if err := mounts.installResumedProgramAuthorityLocked(entry, authority, clock()); err != nil {
		return err
	}
	grant := &programv0.ResumeAttach{
		RunId: fence.GetRunId(), AttemptNumber: fence.GetAttemptNumber(),
		RunLeaseId: fence.GetRunLeaseId(), RunWaitId: request.GetRunWaitId(),
		CheckpointId: request.GetCheckpointId(), ResumeAttachId: request.GetResumeAttachId(),
		ResumeRequestVersion: request.GetResumeRequestVersion(), CorrelationId: request.GetCorrelationId(),
	}
	installed := proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority)
	if err := waits.grantProgramResume(&programResumeGrant{
		attach: grant,
		lock:   entry.finalizationMu.Lock,
		unlock: entry.finalizationMu.Unlock,
		valid: func(now time.Time) bool {
			entry.processesMu.Lock()
			live := entry.authorityState == workspaceAuthorityLive && !entry.recoveryRequired
			entry.processesMu.Unlock()
			entry.authorityMu.Lock()
			current := workspaceRunAuthoritiesEqual(entry.authority, installed)
			entry.authorityMu.Unlock()
			return live && current && installed.GetFence().GetExpiresAtUnixNano() > now.UnixNano()
		},
	}); err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(resumeAttachTimeout)); err != nil {
		return fmt.Errorf("bound program resume grant response: %w", err)
	}
	defer conn.SetWriteDeadline(time.Time{})
	return frameio.WriteProtoFrame(conn, &workspacev0.GrantProgramResumeResponse{
		Fence:     proto.Clone(fence).(*workspacev0.WorkspaceAuthorityFence),
		RunWaitId: request.GetRunWaitId(), CheckpointId: request.GetCheckpointId(),
		ResumeAttachId: request.GetResumeAttachId(), ResumeRequestVersion: request.GetResumeRequestVersion(),
		CorrelationId: request.GetCorrelationId(),
	})
}

func (r *workspaceOperationRegistry) installResumedProgramAuthorityLocked(
	entry *workspaceMountEntry,
	authority *workspacev0.WorkspaceRunAuthority,
	now time.Time,
) error {
	if err := validateWorkspaceRunAuthority(entry, authority, now); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	claim := r.programClaimLocked(entry, authority)
	if claim == nil {
		return errors.New("resumed program claim is not active")
	}
	entry.authorityMu.Lock()
	missing := entry.authority == nil
	current := workspaceRunAuthoritiesEqual(entry.authority, authority)
	entry.authorityMu.Unlock()
	entry.processesMu.Lock()
	state := entry.authorityState
	recoveryRequired := entry.recoveryRequired
	entry.processesMu.Unlock()
	if !current && !recoveryRequired &&
		(missing && state == workspaceAuthorityLive ||
			!missing && state == workspaceAuthorityFinalizing) {
		if err := entry.installWorkspaceRunAuthorityLocked(authority, now); err != nil {
			return fmt.Errorf("install restored program authority: %w", err)
		}
		current = true
	}
	entry.processesMu.Lock()
	live := entry.authorityState == workspaceAuthorityLive && !entry.recoveryRequired
	entry.processesMu.Unlock()
	if !current || !live {
		return errors.New("program resume grant authority is not installed")
	}
	claim.authority = proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority)
	return nil
}

func handleProgramRestoreVerifyConnection(
	conn programConnection,
	bodyLen uint64,
	mounts *workspaceOperationRegistry,
	waits *waitingRunRegistry,
) error {
	if bodyLen != 0 {
		return errors.New("program restore verification stream body must be empty")
	}
	if err := conn.SetReadDeadline(time.Now().Add(resumeAttachTimeout)); err != nil {
		return err
	}
	defer conn.SetReadDeadline(time.Time{})
	var request workspacev0.VerifyProgramRestoreRequest
	if err := frameio.ReadProtoFrameBounded(conn, maxProgramControlFrameBytes, &request); err != nil {
		return fmt.Errorf("read program restore verification: %w", err)
	}
	if strings.TrimSpace(request.GetRunId()) == "" || request.GetAttemptNumber() == 0 ||
		strings.TrimSpace(request.GetRunWaitId()) == "" || strings.TrimSpace(request.GetCheckpointId()) == "" ||
		strings.TrimSpace(request.GetCorrelationId()) == "" || waits == nil || mounts == nil {
		return errors.New("program restore verification identity is incomplete")
	}
	if !waits.verifyFrozenProgram(&request) {
		return errors.New("program restore verification did not match a frozen program")
	}
	if mounts.currentProgramEntry(request.GetRunId(), request.GetAttemptNumber()) == nil {
		return errors.New("program restore verification did not match an active program claim")
	}
	if err := conn.SetWriteDeadline(time.Now().Add(resumeAttachTimeout)); err != nil {
		return err
	}
	defer conn.SetWriteDeadline(time.Time{})
	return frameio.WriteProtoFrame(conn, &workspacev0.VerifyProgramRestoreResponse{
		RunId: request.GetRunId(), AttemptNumber: request.GetAttemptNumber(),
		RunWaitId: request.GetRunWaitId(), CheckpointId: request.GetCheckpointId(),
		CorrelationId: request.GetCorrelationId(),
	})
}

func (entry *workspaceMountEntry) installWorkspaceRunAuthority(authority *workspacev0.WorkspaceRunAuthority, now time.Time) error {
	entry.turnCommitMu.Lock()
	defer entry.turnCommitMu.Unlock()
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
			return errors.New("workspace run authority base version does not match the mounted frontier")
		}
		if mountGeneration < entry.currentFencingGeneration() {
			return errors.New("workspace run authority mount fencing generation is stale")
		}
	} else {
		entry.processesMu.Lock()
		finalizing := entry.authorityState == workspaceAuthorityFinalizing
		entry.processesMu.Unlock()
		if !finalizing {
			return errors.New("workspace run authority is already installed")
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
			return errors.New("workspace finalization is not committed")
		}
		if authority.GetFence().GetWriterGeneration() <= entry.authority.GetFence().GetWriterGeneration() {
			return errors.New("workspace run authority does not advance the writer generation")
		}
		if mountGeneration <= entry.currentFencingGeneration() {
			return errors.New("workspace run authority does not advance the mount fencing generation")
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
		return fmt.Errorf("read workspace finalization state for pruning: %w", err)
	}
	if found && journal.Kind == workspace.FinalizationResetKind && strings.TrimSpace(journal.OperationID) != "" {
		operationID, err := uuid.Parse(journal.OperationID)
		if err != nil || operationID.String() != journal.OperationID {
			return errors.New("workspace reset journal operation ID is invalid")
		}
		if err := os.RemoveAll(entry.workspaceResetStagingPath(journal.OperationID)); err != nil {
			return fmt.Errorf("prune workspace reset staging tree: %w", err)
		}
		if err := syncDirectory(filepath.Dir(entry.workspaceRoot)); err != nil {
			return fmt.Errorf("sync pruned workspace reset staging tree: %w", err)
		}
	}
	for _, name := range []string{workspaceCaptureArtifactName, workspaceFinalizationJournalName} {
		if err := os.Remove(filepath.Join(entry.finalizationRoot, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("prune workspace finalization state: %w", err)
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
		return nil, errors.New("previous workspace run authority is required")
	}
	if request.GetNewExpiresAtUnixNano() <= request.GetPrevious().GetFence().GetExpiresAtUnixNano() {
		return nil, errors.New("renewed workspace run authority expiry must advance")
	}
	entry.processesMu.Lock()
	finalizing := entry.authorityState == workspaceAuthorityFinalizing
	entry.processesMu.Unlock()
	if finalizing {
		return nil, errors.New("workspace run authority cannot be renewed after finalization begins")
	}
	entry.authorityMu.Lock()
	defer entry.authorityMu.Unlock()
	if entry.authority == nil || entry.authority.GetFence() == nil {
		return nil, errors.New("workspace run authority is not installed")
	}
	currentExpiry := entry.authority.GetFence().GetExpiresAtUnixNano()
	if currentExpiry <= now.UnixNano() {
		return nil, errors.New("workspace run authority expired")
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
		return nil, errors.New("previous workspace run authority does not match current authority")
	}
	return proto.Clone(entry.authority.GetFence()).(*workspacev0.WorkspaceAuthorityFence), nil
}

func (r *workspaceOperationRegistry) renewCurrentWorkspaceRunAuthority(entry *workspaceMountEntry, request *workspacev0.RenewWorkspaceAuthorityRequest, now time.Time) (*workspacev0.WorkspaceAuthorityFence, error) {
	if request == nil || request.GetPrevious() == nil || request.GetPrevious().GetFence() == nil {
		return nil, errors.New("previous workspace run authority is required")
	}
	previous := request.GetPrevious()
	fence := previous.GetFence()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries[fence.GetWorkspaceMountId()] != entry ||
		!workspaceEntryMatches(
			entry,
			fence.GetWorkspaceMountId(),
			fence.GetWorkspaceId(),
			previous.GetChannelToken(),
		) ||
		entry.currentFencingGeneration() != uint64(fence.GetMountFencingGeneration()) {
		return nil, errors.New("workspace run authority is not current for the workspace mount")
	}
	candidate := proto.Clone(previous).(*workspacev0.WorkspaceRunAuthority)
	candidate.Fence.ExpiresAtUnixNano = request.GetNewExpiresAtUnixNano()
	claim := r.programClaimLocked(entry, previous)
	if claim != nil &&
		!workspaceRunAuthoritiesEqual(claim.authority, previous) &&
		!workspaceRunAuthoritiesEqual(claim.authority, candidate) {
		return nil, errors.New("active program claim does not match the workspace authority renewal")
	}
	renewed, err := entry.renewWorkspaceRunAuthorityLocked(request, now)
	if err != nil {
		return nil, err
	}
	if claim != nil {
		renewedAuthority := proto.Clone(previous).(*workspacev0.WorkspaceRunAuthority)
		renewedAuthority.Fence = proto.Clone(renewed).(*workspacev0.WorkspaceAuthorityFence)
		claim.authority = renewedAuthority
	}
	return renewed, nil
}

func (r *workspaceOperationRegistry) beginCurrentWorkspaceFinalization(
	entry *workspaceMountEntry,
	request *workspacev0.BeginWorkspaceFinalizationRequest,
	clock func() time.Time,
) (*workspacev0.BeginWorkspaceFinalizationResponse, error) {
	if request == nil || request.GetPrevious() == nil || request.GetPrevious().GetFence() == nil {
		return nil, errors.New("previous workspace run authority is required")
	}
	previous := request.GetPrevious()
	fence := previous.GetFence()
	entry.turnCommitMu.Lock()
	defer entry.turnCommitMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	if !r.currentExactLocked(
		entry,
		fence.GetWorkspaceMountId(),
		fence.GetWorkspaceId(),
		previous.GetChannelToken(),
		uint64(fence.GetMountFencingGeneration()),
	) {
		return nil, errors.New("workspace finalization authority is not current for the workspace mount")
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
		return nil, errors.New("previous workspace run authority is required")
	}
	rawOperationID := request.GetOperationId()
	operationID := strings.TrimSpace(rawOperationID)
	parsedOperationID, err := uuid.Parse(operationID)
	if err != nil || rawOperationID != operationID || parsedOperationID.String() != operationID {
		return nil, errors.New("workspace finalization operation_id must be a canonical UUID")
	}
	kind := request.GetKind()
	if kind != workspace.FinalizationCaptureKind && kind != workspace.FinalizationResetKind {
		return nil, errors.New("workspace finalization kind must be capture or reset")
	}
	previous := request.GetPrevious()
	previousExpiry := previous.GetFence().GetExpiresAtUnixNano()
	finalizationExpiry := request.GetFinalizationExpiresAtUnixNano()
	if finalizationExpiry <= previousExpiry || finalizationExpiry <= now.UnixNano() {
		return nil, errors.New("workspace finalization expiry must advance and remain live")
	}
	if entry.finalizationRoot == "" {
		return nil, errors.New("workspace finalization state is unavailable")
	}

	entry.authorityMu.Lock()
	defer entry.authorityMu.Unlock()
	if entry.authority == nil || entry.authority.GetFence() == nil {
		return nil, errors.New("workspace run authority is not installed")
	}
	currentExpiry := entry.authority.GetFence().GetExpiresAtUnixNano()
	entry.processesMu.Lock()
	defer entry.processesMu.Unlock()
	if entry.recoveryRequired {
		return nil, errors.New("workspace finalization requires a recoverable workspace")
	}

	switch entry.authorityState {
	case workspaceAuthorityLive:
		if !workspaceRunAuthoritiesEqual(entry.authority, previous) {
			return nil, errors.New("previous workspace run authority does not match current authority")
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
				return nil, errors.New("workspace finalization conflicts with retained state")
			}
		} else {
			if currentExpiry <= now.UnixNano() {
				return nil, errors.New("workspace run authority expired before finalization began")
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
			return nil, errors.New("workspace finalization request does not match current finalization")
		}
		journal, found, err := entry.readWorkspaceFinalizationJournal()
		if err != nil {
			return nil, err
		}
		if !found || validateWorkspaceFinalizationBeginJournal(
			journal, operationID, kind, workspaceFinalizationFence(entry.authority.GetFence()),
		) != nil {
			return nil, errors.New("workspace finalization state is unavailable or conflicting")
		}
	default:
		return nil, errors.New("workspace run authority state is invalid")
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
			return errors.Join(err, fmt.Errorf("write workspace authority renewal failure: %w", writeErr))
		}
	}
	return nil
}

func handleWorkspaceFinalizationBeginConnection(conn io.ReadWriter, registry *workspaceOperationRegistry) error {
	if err := handleWorkspaceFinalizationBegin(conn, registry, time.Now); err != nil {
		if writeErr := frameio.WriteProtoFrame(conn, &workspacev0.BeginWorkspaceFinalizationResponse{Error: err.Error()}); writeErr != nil {
			return errors.Join(err, fmt.Errorf("write workspace finalization begin failure: %w", writeErr))
		}
	}
	return nil
}

func handleWorkspaceFinalizationBegin(
	conn io.ReadWriter,
	registry *workspaceOperationRegistry,
	clock func() time.Time,
) error {
	var request workspacev0.BeginWorkspaceFinalizationRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read workspace finalization begin: %w", err)
	}
	previous := request.GetPrevious()
	if previous == nil || previous.GetFence() == nil {
		return errors.New("workspace finalization previous authority is required")
	}
	fence := previous.GetFence()
	entry, release, ok := registry.acquireExact(
		fence.GetWorkspaceMountId(),
		fence.GetWorkspaceId(),
		previous.GetChannelToken(),
		uint64(fence.GetMountFencingGeneration()),
	)
	if !ok {
		return errors.New("workspace finalization does not match the mounted runtime")
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
		return fmt.Errorf("write workspace finalization begin response: %w", err)
	}
	return nil
}

func handleWorkspaceAuthorityRenew(conn io.ReadWriter, registry *workspaceOperationRegistry, clock func() time.Time) error {
	var request workspacev0.RenewWorkspaceAuthorityRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return fmt.Errorf("read workspace authority renewal: %w", err)
	}
	previous := request.GetPrevious()
	if previous == nil || previous.GetFence() == nil {
		return errors.New("workspace authority renewal previous authority is required")
	}
	fence := previous.GetFence()
	entry, release, ok := registry.acquireExact(
		fence.GetWorkspaceMountId(),
		fence.GetWorkspaceId(),
		previous.GetChannelToken(),
		uint64(fence.GetMountFencingGeneration()),
	)
	if !ok {
		return errors.New("workspace authority renewal does not match the mounted runtime")
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
		return fmt.Errorf("write workspace authority renewal response: %w", err)
	}
	return nil
}
