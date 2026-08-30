package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

type actorTurnCommitControlPlane struct {
	*testRunLeaseControlPlane
	request            workerapi.CommitActorTurnRequest
	workspaceVersionID string
}

type blockingActorTurnCommitControlPlane struct {
	*testRunLeaseControlPlane
	started       chan struct{}
	release       chan struct{}
	mu            sync.Mutex
	renewed       int
	projectedBase string
}

func (controlPlane *blockingActorTurnCommitControlPlane) CommitActorTurn(
	ctx context.Context,
	request workerapi.CommitActorTurnRequest,
) (workerapi.CommitActorTurnResponse, error) {
	select {
	case <-controlPlane.started:
	default:
		close(controlPlane.started)
	}
	select {
	case <-controlPlane.release:
	case <-ctx.Done():
		return workerapi.CommitActorTurnResponse{}, ctx.Err()
	}
	return workerapi.CommitActorTurnResponse{
		Lease: request.Lease, CorrelationID: request.CorrelationID,
		CommittedInputSequence: request.TargetInputSequence, WorkspaceVersionID: "version-2",
		Tree: request.Tree,
	}, nil
}

func (controlPlane *blockingActorTurnCommitControlPlane) RenewRunLease(
	_ context.Context,
	previous workerapi.RunLeaseAssignment,
) (workerapi.RunLeaseRenewResponse, error) {
	controlPlane.mu.Lock()
	controlPlane.renewed++
	projectedBase := controlPlane.projectedBase
	controlPlane.mu.Unlock()
	if projectedBase == "" {
		projectedBase = previous.BaseWorkspaceVersionID
	}
	return workerapi.RunLeaseRenewResponse{
		Lease: previous.Fence(), ExpiresAt: time.Now().Add(240 * time.Millisecond),
		BaseWorkspaceVersionID: projectedBase,
	}, nil
}

type actorTurnRenewalMounts struct {
	WorkspaceMountSessionRegistry
}

func (actorTurnRenewalMounts) RenewWorkspaceAuthority(
	_ context.Context,
	request *workspacev0.RenewWorkspaceAuthorityRequest,
) (*workspacev0.WorkspaceAuthorityFence, error) {
	fence := proto.Clone(request.GetPrevious().GetFence()).(*workspacev0.WorkspaceAuthorityFence)
	fence.ExpiresAtUnixNano = request.GetNewExpiresAtUnixNano()
	return fence, nil
}

func (controlPlane *actorTurnCommitControlPlane) CommitActorTurn(
	_ context.Context,
	request workerapi.CommitActorTurnRequest,
) (workerapi.CommitActorTurnResponse, error) {
	controlPlane.request = request
	workspaceVersionID := controlPlane.workspaceVersionID
	if workspaceVersionID == "" {
		workspaceVersionID = "version-2"
	}
	return workerapi.CommitActorTurnResponse{
		Lease: request.Lease, CorrelationID: request.CorrelationID,
		CommittedInputSequence: request.TargetInputSequence, WorkspaceVersionID: workspaceVersionID,
		Tree: request.Tree,
	}, nil
}

func TestHandleActorTurnCommitAdvancesAllLocalWorkspaceFrontiers(t *testing.T) {
	claim := testFreshProgramClaim(t)
	claim.Lease.WorkerGroupID = "workers"
	claim.Lease.RequestedCPUMillis = 1
	claim.Lease.RequestedMemoryBytes = 1
	claim.Lease.RequestedGuestEphemeralDiskBytes = 1
	claim.Lease.RequestedExecutionSlots = 1
	claim.Lease.MaxActiveDurationMs = 1
	target, err := workspace.EmptyResetTarget("version-1", workspace.TreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest})
	if err != nil {
		t.Fatal(err)
	}
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	renewedLease := claim.Lease
	renewedLease.ExpiresAt = claim.Lease.ExpiresAt.Add(time.Minute)
	controlPlane := &actorTurnCommitControlPlane{testRunLeaseControlPlane: &testRunLeaseControlPlane{
		trace: &runLeaseTrace{}, renewed: testRunLeaseRenewResponse(renewedLease),
	}}
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	authority := freshWorkspaceAuthority(&claim, "channel-1")
	task := &guestRunLeaseTask{
		program: freshProgram{session: fakeGuestSession{stream: host}},
		store:   store, controlPlane: controlPlane, resetTarget: target, lease: claim.Lease,
		authority: authority, waitWorkspace: workerapi.Workspace{BaseVersionID: "version-1"},
		checkpointer: &runtimeCheckpointer{}, mounts: actorTurnRenewalMounts{},
	}

	artifactBody := []byte("canonical workspace archive")
	artifactDigest := sha256sum.DigestBytes(artifactBody)
	tree := workspace.TreeIdentity{Digest: sha256sum.DigestBytes([]byte("logical tree")), SizeBytes: 12, EntryCount: 1}
	captureStarted := make(chan struct{})
	captureGate := make(chan struct{})
	decisionSeen := make(chan struct{})
	applyGate := make(chan struct{})
	guestResult := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(guest)
		header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
		if err != nil {
			guestResult <- err
			return
		}
		pause, err := wire.ReadActorTurnCommitPauseRequest(header, reader, bodyLen)
		if err != nil {
			guestResult <- err
			return
		}
		if pause.GetExpectedBaseWorkspaceVersionId() != "version-1" {
			guestResult <- errors.New("pause request carried the wrong workspace frontier")
			return
		}
		close(captureStarted)
		<-captureGate
		entryCount := 1
		if err := wire.WriteStreamFrameHeader(guest, wire.StreamHeader{
			Type: wire.StreamTypeWorkspaceArtifact, RunID: claim.Lease.RunID,
			BodyDigest: &artifactDigest, EntryCount: &entryCount,
		}, uint64(len(artifactBody))); err != nil {
			guestResult <- err
			return
		}
		if _, err := guest.Write(artifactBody); err != nil {
			guestResult <- err
			return
		}
		if err := wire.WriteActorTurnCommitPauseReady(guest, &programv0.ActorTurnCommitPauseReady{
			CorrelationId: pause.GetCorrelationId(), TargetInputSequence: pause.GetTargetInputSequence(),
			RunId: pause.GetRunId(), AttemptNumber: pause.GetAttemptNumber(), RunLeaseId: pause.GetRunLeaseId(),
			TreeDigest: tree.Digest, TreeSizeBytes: tree.SizeBytes,
			TreeEntryCount: uint32(tree.EntryCount), WorkspaceChanged: true,
		}); err != nil {
			guestResult <- err
			return
		}
		header, bodyLen, err = wire.ReadStreamFrameHeader(reader)
		if err != nil {
			guestResult <- err
			return
		}
		decision, err := wire.ReadResumeDecision(header, reader, bodyLen)
		if err != nil {
			guestResult <- err
			return
		}
		var payload struct {
			WorkspaceVersionID string `json:"workspace_version_id"`
		}
		if err := json.Unmarshal([]byte(decision.GetDataJson()), &payload); err != nil {
			guestResult <- err
			return
		}
		if decision.GetKind() != "committed" || payload.WorkspaceVersionID != "version-2" {
			guestResult <- errors.New("commit decision did not carry the new workspace frontier")
			return
		}
		close(decisionSeen)
		<-applyGate
		if err := wire.WriteActorTurnCommitApplied(guest, &programv0.ActorTurnCommitApplied{
			CorrelationId: pause.GetCorrelationId(), TargetInputSequence: pause.GetTargetInputSequence(),
			RunId: pause.GetRunId(), AttemptNumber: pause.GetAttemptNumber(), RunLeaseId: pause.GetRunLeaseId(),
			PreviousBaseWorkspaceVersionId: pause.GetExpectedBaseWorkspaceVersionId(),
			AppliedBaseWorkspaceVersionId:  payload.WorkspaceVersionID,
		}); err != nil {
			guestResult <- err
			return
		}
		guestResult <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hostResult := make(chan error, 1)
	go func() {
		hostResult <- task.handleActorTurnCommit(ctx, &programv0.ActorTurnCommitRequested{
			CorrelationId: "019c10d5-a6f7-7af1-8f5f-000000000099", TargetInputSequence: 1,
		})
	}()
	<-captureStarted
	if _, err := task.RenewRunLease(ctx); err != nil {
		t.Fatalf("renew during Actor turn capture: %v", err)
	}
	close(captureGate)
	<-decisionSeen
	task.mu.Lock()
	baseBeforeProof := task.lease.BaseWorkspaceVersionID
	task.mu.Unlock()
	if baseBeforeProof != "version-1" {
		t.Fatalf("host installed Actor frontier before guest proof: %q", baseBeforeProof)
	}
	close(applyGate)
	if err := <-hostResult; err != nil {
		t.Fatal(err)
	}
	if err := <-guestResult; err != nil {
		t.Fatal(err)
	}
	if task.lease.BaseWorkspaceVersionID != "version-2" || task.resetTarget.BaseVersionID != "version-2" ||
		task.waitWorkspace.BaseVersionID != "version-2" || task.authority.GetFence().GetBaseWorkspaceVersionId() != "version-2" {
		t.Fatalf("local Actor turn frontiers were not advanced: lease=%q reset=%q wait=%q authority=%q",
			task.lease.BaseWorkspaceVersionID, task.resetTarget.BaseVersionID,
			task.waitWorkspace.BaseVersionID, task.authority.GetFence().GetBaseWorkspaceVersionId())
	}
	checkpointBase := task.checkpointer.(*runtimeCheckpointer).workspace
	if task.waitWorkspace.Artifact == nil || task.waitWorkspace.Artifact.Digest != artifactDigest ||
		checkpointBase.ArtifactDigest != artifactDigest ||
		checkpointBase.ArtifactSizeBytes != int64(len(artifactBody)) ||
		checkpointBase.ArtifactMediaType != workspace.ArtifactMediaType ||
		checkpointBase.ArtifactEncoding != workspace.ArtifactEncoding ||
		checkpointBase.MountPath != "/workspace" {
		t.Fatal("Actor turn commit did not advance Wait and checkpoint Workspace artifacts")
	}
	if controlPlane.request.Artifact == nil || controlPlane.request.Artifact.Digest != artifactDigest ||
		controlPlane.request.BaseWorkspaceVersionID != "version-1" || controlPlane.request.Tree.Digest != tree.Digest {
		t.Fatalf("Actor turn Control Plane request = %+v", controlPlane.request)
	}
}

func TestHandleActorTurnCommitRejectsMismatchedAppliedProofWithoutInstallingFrontier(t *testing.T) {
	claim := testFreshProgramClaim(t)
	target, err := workspace.EmptyResetTarget("version-1", workspace.TreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest})
	if err != nil {
		t.Fatal(err)
	}
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	task := &guestRunLeaseTask{
		program: freshProgram{session: fakeGuestSession{stream: host}}, store: store,
		controlPlane: &actorTurnCommitControlPlane{
			testRunLeaseControlPlane: &testRunLeaseControlPlane{}, workspaceVersionID: "version-1",
		},
		resetTarget: target, lease: claim.Lease, authority: freshWorkspaceAuthority(&claim, "channel-1"),
		waitWorkspace: workerapi.Workspace{BaseVersionID: "version-1"},
	}
	guestResult := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(guest)
		header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
		if err != nil {
			guestResult <- err
			return
		}
		pause, err := wire.ReadActorTurnCommitPauseRequest(header, reader, bodyLen)
		if err != nil {
			guestResult <- err
			return
		}
		if err := wire.WriteActorTurnCommitPauseReady(guest, &programv0.ActorTurnCommitPauseReady{
			CorrelationId: pause.GetCorrelationId(), TargetInputSequence: pause.GetTargetInputSequence(),
			RunId: pause.GetRunId(), AttemptNumber: pause.GetAttemptNumber(), RunLeaseId: pause.GetRunLeaseId(),
			TreeDigest: target.Tree.Digest, TreeSizeBytes: target.Tree.SizeBytes,
			TreeEntryCount: uint32(target.Tree.EntryCount), WorkspaceChanged: false,
		}); err != nil {
			guestResult <- err
			return
		}
		header, bodyLen, err = wire.ReadStreamFrameHeader(reader)
		if err != nil {
			guestResult <- err
			return
		}
		decision, err := wire.ReadResumeDecision(header, reader, bodyLen)
		if err != nil {
			guestResult <- err
			return
		}
		if err := wire.WriteActorTurnCommitApplied(guest, &programv0.ActorTurnCommitApplied{
			CorrelationId: pause.GetCorrelationId(), TargetInputSequence: pause.GetTargetInputSequence(),
			RunId: pause.GetRunId(), AttemptNumber: pause.GetAttemptNumber(), RunLeaseId: pause.GetRunLeaseId(),
			PreviousBaseWorkspaceVersionId: pause.GetExpectedBaseWorkspaceVersionId(),
			AppliedBaseWorkspaceVersionId:  decision.GetDataJson(),
		}); err != nil {
			guestResult <- err
			return
		}
		guestResult <- nil
	}()
	err = task.handleActorTurnCommit(t.Context(), &programv0.ActorTurnCommitRequested{
		CorrelationId: "019c10d5-a6f7-7af1-8f5f-000000000100", TargetInputSequence: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "applied proof") {
		t.Fatalf("mismatched applied proof error = %v", err)
	}
	if guestErr := <-guestResult; guestErr != nil {
		t.Fatal(guestErr)
	}
	if task.lease.BaseWorkspaceVersionID != "version-1" || task.authority.GetFence().GetBaseWorkspaceVersionId() != "version-1" {
		t.Fatal("mismatched guest proof installed the Actor Workspace frontier")
	}
}

func TestHandleActorTurnCommitStopsMissingAppliedProofAtLeaseExpiry(t *testing.T) {
	claim := testFreshProgramClaim(t)
	claim.Lease.ExpiresAt = time.Now().Add(180 * time.Millisecond)
	target, err := workspace.EmptyResetTarget("version-1", workspace.TreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest})
	if err != nil {
		t.Fatal(err)
	}
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host, guest := net.Pipe()
	defer guest.Close()
	task := &guestRunLeaseTask{
		program: freshProgram{session: fakeGuestSession{stream: host}}, store: store,
		controlPlane: &actorTurnCommitControlPlane{
			testRunLeaseControlPlane: &testRunLeaseControlPlane{}, workspaceVersionID: "version-1",
		},
		resetTarget: target, lease: claim.Lease, authority: freshWorkspaceAuthority(&claim, "channel-1"),
		waitWorkspace: workerapi.Workspace{BaseVersionID: "version-1"},
	}
	guestResult := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(guest)
		header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
		if err != nil {
			guestResult <- err
			return
		}
		pause, err := wire.ReadActorTurnCommitPauseRequest(header, reader, bodyLen)
		if err != nil {
			guestResult <- err
			return
		}
		if err := wire.WriteActorTurnCommitPauseReady(guest, &programv0.ActorTurnCommitPauseReady{
			CorrelationId: pause.GetCorrelationId(), TargetInputSequence: pause.GetTargetInputSequence(),
			RunId: pause.GetRunId(), AttemptNumber: pause.GetAttemptNumber(), RunLeaseId: pause.GetRunLeaseId(),
			TreeDigest: target.Tree.Digest, TreeSizeBytes: target.Tree.SizeBytes,
			TreeEntryCount: uint32(target.Tree.EntryCount), WorkspaceChanged: false,
		}); err != nil {
			guestResult <- err
			return
		}
		header, bodyLen, err = wire.ReadStreamFrameHeader(reader)
		if err == nil {
			_, err = wire.ReadResumeDecision(header, reader, bodyLen)
		}
		if err != nil {
			guestResult <- err
			return
		}
		buffer := make([]byte, 1)
		_, err = guest.Read(buffer)
		guestResult <- err
	}()
	started := time.Now()
	err = task.handleActorTurnCommit(t.Context(), &programv0.ActorTurnCommitRequested{
		CorrelationId: "019c10d5-a6f7-7af1-8f5f-000000000103", TargetInputSequence: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "applied header") {
		t.Fatalf("missing applied proof error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("missing applied proof outlived lease expiry: %s", time.Since(started))
	}
	if guestErr := <-guestResult; guestErr == nil {
		t.Fatal("guest stream remained open after applied-proof deadline")
	}
}

func TestHandleActorTurnCommitStopsBlockedDecisionWriteAtLeaseExpiry(t *testing.T) {
	claim := testFreshProgramClaim(t)
	claim.Lease.ExpiresAt = time.Now().Add(180 * time.Millisecond)
	target, err := workspace.EmptyResetTarget("version-1", workspace.TreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest})
	if err != nil {
		t.Fatal(err)
	}
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host, guest := net.Pipe()
	defer guest.Close()
	task := &guestRunLeaseTask{
		program: freshProgram{session: fakeGuestSession{stream: host}}, store: store,
		controlPlane: &actorTurnCommitControlPlane{
			testRunLeaseControlPlane: &testRunLeaseControlPlane{}, workspaceVersionID: "version-1",
		},
		resetTarget: target, lease: claim.Lease, authority: freshWorkspaceAuthority(&claim, "channel-1"),
		waitWorkspace: workerapi.Workspace{BaseVersionID: "version-1"},
	}
	stopGuest := make(chan struct{})
	guestResult := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(guest)
		header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
		if err == nil {
			var pause *programv0.ActorTurnCommitPauseRequest
			pause, err = wire.ReadActorTurnCommitPauseRequest(header, reader, bodyLen)
			if err == nil {
				err = wire.WriteActorTurnCommitPauseReady(guest, &programv0.ActorTurnCommitPauseReady{
					CorrelationId: pause.GetCorrelationId(), TargetInputSequence: pause.GetTargetInputSequence(),
					RunId: pause.GetRunId(), AttemptNumber: pause.GetAttemptNumber(), RunLeaseId: pause.GetRunLeaseId(),
					TreeDigest: target.Tree.Digest, TreeSizeBytes: target.Tree.SizeBytes,
					TreeEntryCount: uint32(target.Tree.EntryCount), WorkspaceChanged: false,
				})
			}
		}
		if err == nil {
			<-stopGuest
		}
		guestResult <- err
	}()
	started := time.Now()
	err = task.handleActorTurnCommit(t.Context(), &programv0.ActorTurnCommitRequested{
		CorrelationId: "019c10d5-a6f7-7af1-8f5f-000000000104", TargetInputSequence: 1,
	})
	close(stopGuest)
	if err == nil || !strings.Contains(err.Error(), "write actor turn commit decision") {
		t.Fatalf("blocked decision error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("blocked decision outlived lease expiry: %s", time.Since(started))
	}
	if guestErr := <-guestResult; guestErr != nil {
		t.Fatal(guestErr)
	}
}

func TestCommitActorTurnKeepsRenewingWhileControlPlaneCommitIsPending(t *testing.T) {
	claim := testFreshProgramClaim(t)
	claim.Lease.ExpiresAt = time.Now().Add(120 * time.Millisecond)
	controlPlane := &blockingActorTurnCommitControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		started:                  make(chan struct{}),
		release:                  make(chan struct{}),
	}
	task := &guestRunLeaseTask{
		controlPlane: controlPlane,
		mounts:       actorTurnRenewalMounts{},
		lease:        claim.Lease,
		authority:    freshWorkspaceAuthority(&claim, "channel-1"),
	}
	request := workerapi.CommitActorTurnRequest{
		CorrelationID:       "019c10d5-a6f7-7af1-8f5f-000000000101",
		TargetInputSequence: 1, BaseWorkspaceVersionID: claim.Lease.BaseWorkspaceVersionID,
		Tree: workerapi.WorkspaceTreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest},
	}
	var response workerapi.CommitActorTurnResponse
	result := make(chan error, 1)
	go func() {
		_, err := task.commitActorTurnWithRenewal(
			t.Context(), claim.Lease.BaseWorkspaceVersionID, &request, &response,
		)
		result <- err
	}()
	<-controlPlane.started
	deadline := time.Now().Add(2 * time.Second)
	for {
		controlPlane.mu.Lock()
		renewed := controlPlane.renewed
		controlPlane.mu.Unlock()
		if renewed >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("renewals while commit was pending = %d", renewed)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(controlPlane.release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if response.WorkspaceVersionID != "version-2" || !task.lease.ExpiresAt.After(claim.Lease.ExpiresAt) {
		t.Fatalf("pending commit response=%+v lease expiry=%v", response, task.lease.ExpiresAt)
	}
}

func TestCommitActorTurnAcceptsOnlyTheReplayedPendingFrontier(t *testing.T) {
	claim := testFreshProgramClaim(t)
	claim.Lease.ExpiresAt = time.Now().Add(120 * time.Millisecond)
	controlPlane := &blockingActorTurnCommitControlPlane{
		testRunLeaseControlPlane: &testRunLeaseControlPlane{},
		started:                  make(chan struct{}),
		release:                  make(chan struct{}),
		projectedBase:            "version-2",
	}
	task := &guestRunLeaseTask{
		controlPlane: controlPlane, mounts: actorTurnRenewalMounts{}, lease: claim.Lease,
		authority: freshWorkspaceAuthority(&claim, "channel-1"),
	}
	request := workerapi.CommitActorTurnRequest{
		CorrelationID: "019c10d5-a6f7-7af1-8f5f-000000000102", TargetInputSequence: 1,
		BaseWorkspaceVersionID: claim.Lease.BaseWorkspaceVersionID,
		Tree:                   workerapi.WorkspaceTreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest},
	}
	var response workerapi.CommitActorTurnResponse
	type commitResult struct {
		pending string
		err     error
	}
	result := make(chan commitResult, 1)
	go func() {
		pending, err := task.commitActorTurnWithRenewal(
			t.Context(), claim.Lease.BaseWorkspaceVersionID, &request, &response,
		)
		result <- commitResult{pending: pending, err: err}
	}()
	<-controlPlane.started
	deadline := time.Now().Add(2 * time.Second)
	for {
		controlPlane.mu.Lock()
		renewed := controlPlane.renewed
		controlPlane.mu.Unlock()
		if renewed > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending Actor frontier was not observed through renewal")
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(controlPlane.release)
	got := <-result
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.pending != response.WorkspaceVersionID || task.lease.BaseWorkspaceVersionID != claim.Lease.BaseWorkspaceVersionID ||
		task.authority.GetFence().GetBaseWorkspaceVersionId() != claim.Lease.BaseWorkspaceVersionID {
		t.Fatalf("pending=%q response=%q local lease=%q guest=%q", got.pending, response.WorkspaceVersionID,
			task.lease.BaseWorkspaceVersionID, task.authority.GetFence().GetBaseWorkspaceVersionId())
	}
}
