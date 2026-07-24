package guestd

import (
	"errors"
	"strings"
	"time"

	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
)

func (entry *workspaceMountEntry) acquireActorTurnCommit(
	run *runv0.ProgramRunRequest,
	request *runv0.ActorTurnCommitPauseRequest,
) (func(), time.Time, error) {
	if entry == nil || run == nil || request == nil {
		return func() {}, time.Time{}, errors.New("Actor turn commit authority is required")
	}
	if request.GetRunId() != run.GetRunId() ||
		request.GetAttemptNumber() != run.GetAttemptNumber() ||
		request.GetRunLeaseId() != run.GetRunLeaseId() {
		return func() {}, time.Time{}, errors.New("Actor turn commit does not match the Program Run")
	}
	entry.finalizationMu.Lock()
	releaseFinalization := true
	release := func() {
		if !releaseFinalization {
			return
		}
		releaseFinalization = false
		entry.processesMu.Lock()
		entry.turnCommitBlocked = false
		entry.processesMu.Unlock()
		entry.finalizationMu.Unlock()
	}

	entry.authorityMu.Lock()
	authority := entry.authority
	expiresAt := time.Time{}
	if authority != nil && authority.GetFence() != nil {
		expiresAt = time.Unix(0, authority.GetFence().GetExpiresAtUnixNano())
	}
	current := authority != nil && authority.GetFence() != nil &&
		authority.GetFence().GetRunId() == run.GetRunId() &&
		authority.GetFence().GetAttemptNumber() == run.GetAttemptNumber() &&
		authority.GetFence().GetRunLeaseId() == run.GetRunLeaseId() &&
		authority.GetFence().GetExpiresAtUnixNano() > time.Now().UnixNano()
	entry.authorityMu.Unlock()
	if !current {
		release()
		return func() {}, time.Time{}, errors.New("Actor turn commit Workspace authority is not current")
	}

	entry.processesMu.Lock()
	if entry.authorityState != workspaceAuthorityLive || entry.recoveryRequired || entry.turnCommitBlocked {
		entry.processesMu.Unlock()
		release()
		return func() {}, time.Time{}, errors.New("Workspace is unavailable for Actor turn commit")
	}
	if entry.processAdmissions != 0 {
		entry.processesMu.Unlock()
		release()
		return func() {}, time.Time{}, errors.New("Actor turn commit requires no active exec")
	}
	entry.turnCommitBlocked = true
	entry.processesMu.Unlock()
	return release, expiresAt, nil
}

// advanceActorTurnWorkspaceFrontierLocked advances the mounted and installed
// authority frontier while finalizationMu is held by the turn-commit barrier.
func (entry *workspaceMountEntry) advanceActorTurnWorkspaceFrontierLocked(expected, next string) error {
	if strings.TrimSpace(expected) == "" || strings.TrimSpace(next) == "" {
		return errors.New("Actor turn commit Workspace frontier is incomplete")
	}
	entry.authorityMu.Lock()
	defer entry.authorityMu.Unlock()
	if entry.authority == nil || entry.authority.GetFence() == nil ||
		entry.baseVersionID != expected || entry.authority.GetFence().GetBaseWorkspaceVersionId() != expected {
		return errors.New("Actor turn commit Workspace frontier is stale")
	}
	entry.baseVersionID = next
	entry.authority.Fence.BaseWorkspaceVersionId = next
	entry.previousExpiry = 0
	return nil
}
