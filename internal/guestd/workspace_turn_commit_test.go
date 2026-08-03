package guestd

import (
	"strings"
	"testing"
	"time"

	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
)

func TestActorTurnCommitBarrierBlocksProcessesAndAdvancesAuthorityFrontier(t *testing.T) {
	entry := testWorkspaceAuthorityEntry()
	entry.authority = testWorkspaceRunAuthority(time.Now().Add(time.Minute))
	run := &runv0.ProgramRunRequest{
		RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1",
	}
	pause := &runv0.ActorTurnCommitPauseRequest{
		RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1",
		ExpectedBaseWorkspaceVersionId: "version-1",
	}

	release, expiresAt, err := entry.acquireActorTurnCommit(run, pause)
	if err != nil {
		t.Fatalf("acquire Actor turn commit: %v", err)
	}
	if !expiresAt.Equal(time.Unix(0, entry.authority.GetFence().GetExpiresAtUnixNano())) {
		release()
		t.Fatalf("Actor turn commit expiry = %v", expiresAt)
	}
	if _, err := entry.beginWorkspaceExecAdmission(); err == nil || !strings.Contains(err.Error(), "unavailable") {
		release()
		t.Fatalf("process admission while blocked = %v, want unavailable", err)
	}
	if err := entry.advanceActorTurnWorkspaceFrontierLocked("version-1", "version-2"); err != nil {
		release()
		t.Fatalf("advance Actor turn Workspace frontier: %v", err)
	}
	if entry.baseVersionID != "version-2" || entry.authority.GetFence().GetBaseWorkspaceVersionId() != "version-2" {
		release()
		t.Fatal("Actor turn commit did not advance both Workspace frontiers")
	}
	release()

	releaseAdmission, err := entry.beginWorkspaceExecAdmission()
	if err != nil {
		t.Fatalf("process admission after release: %v", err)
	}
	releaseAdmission()
}

func TestActorTurnCommitBarrierRejectsActiveWorkspaceExec(t *testing.T) {
	entry := testWorkspaceAuthorityEntry()
	entry.authority = testWorkspaceRunAuthority(time.Now().Add(time.Minute))
	entry.processAdmissions = 1
	_, _, err := entry.acquireActorTurnCommit(
		&runv0.ProgramRunRequest{RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1"},
		&runv0.ActorTurnCommitPauseRequest{RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1"},
	)
	if err == nil || !strings.Contains(err.Error(), "no active exec") {
		t.Fatalf("active process barrier error = %v", err)
	}
	if entry.turnCommitBlocked {
		t.Fatal("failed Actor turn commit left process admission blocked")
	}
}

func TestActorTurnCommitFrontierRejectsStaleBase(t *testing.T) {
	entry := testWorkspaceAuthorityEntry()
	entry.authority = testWorkspaceRunAuthority(time.Now().Add(time.Minute))
	if err := entry.advanceActorTurnWorkspaceFrontierLocked("version-stale", "version-2"); err == nil ||
		!strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale frontier error = %v", err)
	}
	if entry.baseVersionID != "version-1" || entry.authority.GetFence().GetBaseWorkspaceVersionId() != "version-1" {
		t.Fatal("stale Actor turn commit mutated Workspace frontier")
	}
}
