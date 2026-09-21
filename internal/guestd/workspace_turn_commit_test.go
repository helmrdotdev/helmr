package guestd

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

func TestTurnSettleBarrierBlocksProcessesAndAdvancesAuthorityFrontier(t *testing.T) {
	entry := testWorkspaceAuthorityEntry()
	entry.authority = testWorkspaceRunAuthority(time.Now().Add(time.Minute))
	registry := newWorkspaceOperationRegistry()
	registry.register("mount-1", entry)
	releaseProgram, err := registry.claimProgramLocked(entry, entry.authority)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseProgram()
	run := &programv0.ProgramRunRequest{
		RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1",
	}
	pause := &programv0.TurnSettlePauseRequest{
		RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1",
		ExpectedBaseWorkspaceVersionId: "version-1",
	}

	release, expiresAt, err := entry.acquireTurnSettle(run, pause)
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
	previous := proto.Clone(entry.authority).(*workspacev0.WorkspaceRunAuthority)
	renewedExpiry := expiresAt.Add(time.Minute)
	renewed := make(chan error, 1)
	go func() {
		_, renewErr := registry.renewCurrentWorkspaceRunAuthority(entry, &workspacev0.RenewWorkspaceAuthorityRequest{
			Previous: previous, NewExpiresAtUnixNano: renewedExpiry.UnixNano(),
		}, time.Now())
		renewed <- renewErr
	}()
	select {
	case err := <-renewed:
		if err != nil {
			release()
			t.Fatalf("renew during Actor turn barrier: %v", err)
		}
	case <-time.After(time.Second):
		release()
		t.Fatal("Actor turn barrier blocked Workspace authority renewal")
	}
	if err := registry.advanceActorTurnWorkspaceFrontier(entry, pause, "version-1", "version-2"); err != nil {
		release()
		t.Fatalf("advance Actor turn Workspace frontier: %v", err)
	}
	if entry.baseWorkspaceVersionID != "version-2" || entry.authority.GetFence().GetBaseWorkspaceVersionId() != "version-2" {
		release()
		t.Fatal("Actor turn commit did not advance both Workspace frontiers")
	}
	registry.mu.Lock()
	claimBase := registry.programClaims[0].authority.GetFence().GetBaseWorkspaceVersionId()
	registry.mu.Unlock()
	if claimBase != "version-2" {
		release()
		t.Fatalf("active Program frontier = %q, want version-2", claimBase)
	}
	current := proto.Clone(entry.authority).(*workspacev0.WorkspaceRunAuthority)
	if _, err := registry.renewCurrentWorkspaceRunAuthority(entry, &workspacev0.RenewWorkspaceAuthorityRequest{
		Previous: current, NewExpiresAtUnixNano: renewedExpiry.Add(time.Minute).UnixNano(),
	}, time.Now()); err != nil {
		release()
		t.Fatalf("renew after Actor turn frontier advance: %v", err)
	}
	release()

	releaseAdmission, err := entry.beginWorkspaceExecAdmission()
	if err != nil {
		t.Fatalf("process admission after release: %v", err)
	}
	releaseAdmission()
}

func TestActorTurnAuthorityContextTracksRenewedExpiry(t *testing.T) {
	entry := testWorkspaceAuthorityEntry()
	entry.authority = testWorkspaceRunAuthority(time.Now().Add(80 * time.Millisecond))
	ctx, cancel := actorTurnAuthorityContext(context.Background(), entry)
	defer cancel()
	time.Sleep(20 * time.Millisecond)
	entry.authorityMu.Lock()
	entry.authority.Fence.ExpiresAtUnixNano = time.Now().Add(120 * time.Millisecond).UnixNano()
	entry.authorityMu.Unlock()
	select {
	case <-ctx.Done():
		t.Fatal("Actor turn context expired at the superseded authority deadline")
	case <-time.After(90 * time.Millisecond):
	}
	select {
	case <-ctx.Done():
	case <-time.After(150 * time.Millisecond):
		t.Fatal("Actor turn context did not expire after renewal stopped")
	}
}

func TestTurnSettleBarrierRejectsActiveWorkspaceExec(t *testing.T) {
	entry := testWorkspaceAuthorityEntry()
	entry.authority = testWorkspaceRunAuthority(time.Now().Add(time.Minute))
	entry.processAdmissions = 1
	_, _, err := entry.acquireTurnSettle(
		&programv0.ProgramRunRequest{RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1"},
		&programv0.TurnSettlePauseRequest{RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1"},
	)
	if err == nil || !strings.Contains(err.Error(), "no active exec") {
		t.Fatalf("active process barrier error = %v", err)
	}
	if entry.turnCommitBlocked {
		t.Fatal("failed Actor turn commit left process admission blocked")
	}
}

func TestTurnSettleFrontierRejectsStaleBase(t *testing.T) {
	entry := testWorkspaceAuthorityEntry()
	entry.authority = testWorkspaceRunAuthority(time.Now().Add(time.Minute))
	entry.turnCommitBlocked = true
	pause := &programv0.TurnSettlePauseRequest{
		RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1",
	}
	registry := newWorkspaceOperationRegistry()
	registry.register("mount-1", entry)
	releaseProgram, claimErr := registry.claimProgramLocked(entry, entry.authority)
	if claimErr != nil {
		t.Fatal(claimErr)
	}
	defer releaseProgram()
	if err := registry.advanceActorTurnWorkspaceFrontier(entry, pause, "version-stale", "version-2"); err == nil ||
		!strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale frontier error = %v", err)
	}
	if entry.baseWorkspaceVersionID != "version-1" || entry.authority.GetFence().GetBaseWorkspaceVersionId() != "version-1" {
		t.Fatal("stale Actor turn commit mutated Workspace frontier")
	}
}

func TestTurnSettleFrontierRejectsExpiredAuthority(t *testing.T) {
	entry := testWorkspaceAuthorityEntry()
	entry.authority = testWorkspaceRunAuthority(time.Now().Add(-time.Second))
	entry.turnCommitBlocked = true
	registry := newWorkspaceOperationRegistry()
	registry.register("mount-1", entry)
	releaseProgram, err := registry.claimProgramLocked(entry, entry.authority)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseProgram()
	pause := &programv0.TurnSettlePauseRequest{
		RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1",
	}
	if err := registry.advanceActorTurnWorkspaceFrontier(entry, pause, "version-1", "version-2"); err == nil ||
		!strings.Contains(err.Error(), "stale") {
		t.Fatalf("expired frontier error = %v", err)
	}
	claim := registry.programClaimLocked(entry, entry.authority)
	if entry.baseWorkspaceVersionID != "version-1" || entry.authority.GetFence().GetBaseWorkspaceVersionId() != "version-1" ||
		claim == nil || claim.authority.GetFence().GetBaseWorkspaceVersionId() != "version-1" {
		t.Fatal("expired Actor authority mutated a guest Workspace frontier")
	}
}

func TestTurnSettleBlocksWorkspaceFinalizationUntilReleased(t *testing.T) {
	entry := testWorkspaceAuthorityEntry()
	entry.authority = testWorkspaceRunAuthority(time.Now().Add(time.Minute))
	entry.finalizationRoot = t.TempDir()
	registry := newWorkspaceOperationRegistry()
	registry.register("mount-1", entry)
	releaseProgram, err := registry.claimProgramLocked(entry, entry.authority)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseProgram()
	run := &programv0.ProgramRunRequest{RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1"}
	pause := &programv0.TurnSettlePauseRequest{RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1"}
	releaseTurn, _, err := entry.acquireTurnSettle(run, pause)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := registry.beginCurrentWorkspaceFinalization(
			entry,
			testWorkspaceFinalizationBeginRequest(
				entry.authority, "019c10d5-a6f7-7af1-8f5f-000000000201", workspace.FinalizationCaptureKind,
			),
			time.Now,
		)
		result <- err
	}()
	select {
	case err := <-result:
		t.Fatalf("finalization crossed active Actor turn barrier: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	releaseTurn()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestTurnSettleBlocksMountRetirementUntilReleased(t *testing.T) {
	entry := testWorkspaceAuthorityEntry()
	entry.authority = testWorkspaceRunAuthority(time.Now().Add(time.Minute))
	registry := newWorkspaceOperationRegistry()
	registry.register("mount-1", entry)
	releaseProgram, err := registry.claimProgramLocked(entry, entry.authority)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseProgram()
	releaseTurn, _, err := entry.acquireTurnSettle(
		&programv0.ProgramRunRequest{RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1"},
		&programv0.TurnSettlePauseRequest{RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	retired := make(chan struct{})
	go func() {
		registry.retire("mount-1", entry)
		close(retired)
	}()
	select {
	case <-retired:
		t.Fatal("mount retirement crossed active Actor turn barrier")
	case <-time.After(50 * time.Millisecond):
	}
	releaseTurn()
	<-retired
	registry.mu.Lock()
	_, current := registry.entries["mount-1"]
	registry.mu.Unlock()
	if current {
		t.Fatal("mount retirement did not remove released Actor entry")
	}
}

func TestTurnSettleCaptureFailureDoesNotResumeCustomerExecution(t *testing.T) {
	entry := testWorkspaceAuthorityEntry()
	entry.authority = testWorkspaceRunAuthority(time.Now().Add(time.Minute))
	registry := newWorkspaceOperationRegistry()
	registry.register("mount-1", entry)
	run := &programv0.ProgramRunRequest{RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1"}
	requested := &programv0.TurnSettleRequested{CorrelationId: "settlement", TargetInputSequence: 1}
	pause := &programv0.TurnSettlePauseRequest{RunId: run.RunId, AttemptNumber: run.AttemptNumber, RunLeaseId: run.RunLeaseId, CorrelationId: requested.CorrelationId, TargetInputSequence: 1, ExpectedBaseWorkspaceVersionId: "version-1", ExpectedTreeDigest: workspace.CanonicalEmptyTreeDigest}
	cgroup := &testProgramCgroup{}
	process := &programProcess{cgroup: cgroup, workspaceRoot: filepath.Join(t.TempDir(), "missing-root")}
	err := pauseTurnSettle(t.Context(), nil, run, requested, pause, process, nil, &programOutputCoordinator{}, registry, entry)
	if err == nil || !strings.Contains(err.Error(), "capture actor turn workspace") {
		t.Fatalf("capture error=%v", err)
	}
	if _, err := entry.beginWorkspaceExecAdmission(); err == nil {
		t.Fatal("failed settlement reopened exec admission")
	}
	if cgroup.freezeCount() != 1 || cgroup.thawCount() != 0 {
		t.Fatalf("failed capture resumed customer execution: freeze=%d thaw=%d", cgroup.freezeCount(), cgroup.thawCount())
	}
}
