package guestd

import (
	"context"
	"errors"
	"strings"
	"time"

	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
)

func (entry *workspaceMountEntry) acquireActorTurnCommit(
	run *programv0.ProgramRunRequest,
	request *programv0.ActorTurnCommitPauseRequest,
) (func(), time.Time, error) {
	if entry == nil || run == nil || request == nil {
		return func() {}, time.Time{}, errors.New("actor turn commit authority is required")
	}
	if request.GetRunId() != run.GetRunId() ||
		request.GetAttemptNumber() != run.GetAttemptNumber() ||
		request.GetRunLeaseId() != run.GetRunLeaseId() {
		return func() {}, time.Time{}, errors.New("actor turn commit does not match the program run")
	}
	entry.turnCommitMu.Lock()
	entry.finalizationMu.Lock()
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		entry.finalizationMu.Lock()
		entry.processesMu.Lock()
		entry.turnCommitBlocked = false
		entry.processesMu.Unlock()
		entry.finalizationMu.Unlock()
		entry.turnCommitMu.Unlock()
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
		entry.finalizationMu.Unlock()
		release()
		return func() {}, time.Time{}, errors.New("actor turn commit workspace authority is not current")
	}

	entry.processesMu.Lock()
	if entry.authorityState != workspaceAuthorityLive || entry.recoveryRequired || entry.turnCommitBlocked {
		entry.processesMu.Unlock()
		entry.finalizationMu.Unlock()
		release()
		return func() {}, time.Time{}, errors.New("workspace is unavailable for actor turn commit")
	}
	if entry.processAdmissions != 0 {
		entry.processesMu.Unlock()
		entry.finalizationMu.Unlock()
		release()
		return func() {}, time.Time{}, errors.New("actor turn commit requires no active exec")
	}
	entry.turnCommitBlocked = true
	entry.processesMu.Unlock()
	entry.finalizationMu.Unlock()
	return release, expiresAt, nil
}

func actorTurnAuthorityContext(parent context.Context, entry *workspaceMountEntry) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		for {
			entry.authorityMu.Lock()
			expiresAt := int64(0)
			if entry.authority != nil && entry.authority.GetFence() != nil {
				expiresAt = entry.authority.GetFence().GetExpiresAtUnixNano()
			}
			entry.authorityMu.Unlock()
			delay := time.Until(time.Unix(0, expiresAt))
			if delay <= 0 {
				cancel()
				return
			}
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return
			}
		}
	}()
	return ctx, cancel
}

func (registry *workspaceOperationRegistry) advanceActorTurnWorkspaceFrontier(
	entry *workspaceMountEntry,
	request *programv0.ActorTurnCommitPauseRequest,
	expected string,
	next string,
) error {
	if request == nil || strings.TrimSpace(expected) == "" || strings.TrimSpace(next) == "" {
		return errors.New("actor turn commit workspace frontier is incomplete")
	}
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	entry.authorityMu.Lock()
	defer entry.authorityMu.Unlock()
	entry.processesMu.Lock()
	live := entry.authorityState == workspaceAuthorityLive && !entry.recoveryRequired && entry.turnCommitBlocked
	entry.processesMu.Unlock()
	if registry.entries[entry.workspaceMountID] != entry || entry.retired || !live ||
		entry.authority == nil || entry.authority.GetFence() == nil ||
		entry.authority.GetFence().GetExpiresAtUnixNano() <= time.Now().UnixNano() ||
		entry.authority.GetFence().GetRunId() != request.GetRunId() ||
		entry.authority.GetFence().GetAttemptNumber() != request.GetAttemptNumber() ||
		entry.authority.GetFence().GetRunLeaseId() != request.GetRunLeaseId() ||
		entry.baseVersionID != expected || entry.authority.GetFence().GetBaseWorkspaceVersionId() != expected {
		return errors.New("actor turn commit workspace frontier is stale")
	}
	claim := registry.programClaimLocked(entry, entry.authority)
	if claim == nil || claim.authority == nil || claim.authority.GetFence() == nil ||
		claim.authority.GetFence().GetBaseWorkspaceVersionId() != expected {
		return errors.New("actor turn commit active Program frontier is stale")
	}
	entry.baseVersionID = next
	entry.authority.Fence.BaseWorkspaceVersionId = next
	claim.authority.Fence.BaseWorkspaceVersionId = next
	entry.previousExpiry = 0
	return nil
}
