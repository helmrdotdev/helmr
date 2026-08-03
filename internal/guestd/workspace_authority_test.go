package guestd

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

func TestWorkspaceRunAuthorityRenewalAndReplay(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	entry := testWorkspaceAuthorityEntry()
	authority := testWorkspaceRunAuthority(now.Add(time.Minute))
	if err := entry.installWorkspaceRunAuthority(authority, now); err != nil {
		t.Fatal(err)
	}
	request := &workspacev0.RenewWorkspaceAuthorityRequest{
		Previous:             proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority),
		NewExpiresAtUnixNano: now.Add(2 * time.Minute).UnixNano(),
	}
	first, err := entry.renewWorkspaceRunAuthority(request, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.GetExpiresAtUnixNano() != request.GetNewExpiresAtUnixNano() {
		t.Fatalf("renewed expiry = %d", first.GetExpiresAtUnixNano())
	}
	replayed, err := entry.renewWorkspaceRunAuthority(request, now)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(first, replayed) {
		t.Fatalf("replay = %+v, want %+v", replayed, first)
	}
}

func TestProgramResumeGrantRequiresInstalledWorkspaceAuthority(t *testing.T) {
	entry, mounts, authority := testWorkspaceFinalizationMountUnadmitted(t)
	releaseProgram, err := mounts.admitProgram(entry, authority, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer releaseProgram()
	waits := newWaitingRunRegistry()
	pause := &runv0.CheckpointPauseRequest{
		RunId: "run-1", AttemptNumber: 2, RunLeaseId: "source-lease",
		RunWaitId: "wait-1", CorrelationId: "correlation-1", CheckpointId: "checkpoint-1",
		ResumeAttachId: "attach-1", CheckpointRequestVersion: 3,
	}
	if _, err := waits.registerProgram(pause); err != nil {
		t.Fatal(err)
	}
	request := &workspacev0.GrantProgramResumeRequest{
		Authority: authority, RunWaitId: "wait-1", CorrelationId: "correlation-1",
		CheckpointId: "checkpoint-1", ResumeAttachId: "attach-1", ResumeRequestVersion: 4,
	}
	var requestBytes bytes.Buffer
	if err := frameio.WriteProtoFrame(&requestBytes, request); err != nil {
		t.Fatal(err)
	}
	stream := &scriptedNetConn{reader: bytes.NewReader(requestBytes.Bytes())}
	if err := handleProgramResumeGrantConnection(stream, 0, mounts, waits, time.Now); err != nil {
		t.Fatal(err)
	}
	var response workspacev0.GrantProgramResumeResponse
	if err := frameio.ReadProtoFrame(bytes.NewReader(stream.written.Bytes()), &response); err != nil {
		t.Fatal(err)
	}
	attach := &runv0.ResumeAttach{
		RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1",
		RunWaitId: "wait-1", CorrelationId: "correlation-1", CheckpointId: "checkpoint-1",
		ResumeAttachId: "attach-1", ResumeRequestVersion: 4,
	}
	entry.processesMu.Lock()
	entry.authorityState = workspaceAuthorityFinalizing
	entry.processesMu.Unlock()
	if err := waits.attachResume(attach, &bytes.Buffer{}); err == nil {
		t.Fatal("finalizing Workspace authority retained its Program resume grant")
	}
	entry.processesMu.Lock()
	entry.authorityState = workspaceAuthorityLive
	entry.processesMu.Unlock()
	if err := waits.attachResume(attach, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	altered := proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority)
	altered.Fence.RunLeaseId = "substituted-lease"
	requestBytes.Reset()
	request.Authority = altered
	if err := frameio.WriteProtoFrame(&requestBytes, request); err != nil {
		t.Fatal(err)
	}
	stream = &scriptedNetConn{reader: bytes.NewReader(requestBytes.Bytes())}
	if err := handleProgramResumeGrantConnection(stream, 0, mounts, waits, time.Now); err == nil {
		t.Fatal("uninstalled restore authority was accepted")
	}
	stale := proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority)
	stale.Fence.MountFencingGeneration--
	requestBytes.Reset()
	request.Authority = stale
	if err := frameio.WriteProtoFrame(&requestBytes, request); err != nil {
		t.Fatal(err)
	}
	stream = &scriptedNetConn{reader: bytes.NewReader(requestBytes.Bytes())}
	if err := handleProgramResumeGrantConnection(stream, 0, mounts, waits, time.Now); err == nil {
		t.Fatal("stale Workspace Mount generation was accepted")
	}
}

func TestProgramResumeGrantWithoutActiveClaimDoesNotAdvanceFinalizedMount(t *testing.T) {
	entry, mounts, authority := testWorkspaceFinalizationMount(t)
	runWorkspaceCapture(t, mounts, testWorkspaceCaptureRequest(
		t,
		authority,
		"11111111-1111-4111-8111-111111111111",
	))
	entry.authorityMu.Lock()
	before := proto.Clone(entry.authority).(*workspacev0.WorkspaceRunAuthority)
	entry.authorityMu.Unlock()
	next := proto.Clone(before).(*workspacev0.WorkspaceRunAuthority)
	next.Fence.WriterGeneration++
	next.Fence.MountFencingGeneration++
	next.Fence.BaseWorkspaceVersionId = "version-2"
	var requestBytes bytes.Buffer
	if err := frameio.WriteProtoFrame(&requestBytes, &workspacev0.GrantProgramResumeRequest{
		Authority: next,
	}); err != nil {
		t.Fatal(err)
	}
	stream := &scriptedNetConn{reader: bytes.NewReader(requestBytes.Bytes())}
	if err := handleProgramResumeGrantConnection(stream, 0, mounts, newWaitingRunRegistry(), time.Now); err == nil {
		t.Fatal("Program resume without an active claim succeeded")
	}
	entry.authorityMu.Lock()
	after := proto.Clone(entry.authority).(*workspacev0.WorkspaceRunAuthority)
	entry.authorityMu.Unlock()
	entry.processesMu.Lock()
	state := entry.authorityState
	entry.processesMu.Unlock()
	if !proto.Equal(after, before) ||
		entry.currentFencingGeneration() != uint64(before.GetFence().GetMountFencingGeneration()) ||
		state != workspaceAuthorityFinalizing {
		t.Fatalf("failed resume mutated finalized mount: authority=%+v generation=%d state=%d", after, entry.currentFencingGeneration(), state)
	}
}

func TestProgramRestoreVerificationRequiresExactFrozenRegistration(t *testing.T) {
	mounts := newWorkspaceOperationRegistry()
	entry := &workspaceMountEntry{}
	mounts.programClaims = []*managedProgramClaim{{
		entry: entry,
		authority: &workspacev0.WorkspaceRunAuthority{Fence: &workspacev0.WorkspaceAuthorityFence{
			RunId: "run-1", AttemptNumber: 2, RunLeaseId: "lease-1",
		}},
		released: make(chan struct{}),
	}}
	waits := newWaitingRunRegistry()
	if _, err := waits.registerProgram(&runv0.CheckpointPauseRequest{
		RunId: "run-1", AttemptNumber: 2, RunWaitId: "wait-1", CorrelationId: "correlation-1",
		CheckpointId: "checkpoint-1", ResumeAttachId: "attach-1", CheckpointRequestVersion: 3,
	}); err != nil {
		t.Fatal(err)
	}
	request := &workspacev0.VerifyProgramRestoreRequest{
		RunId: "run-1", AttemptNumber: 2, RunWaitId: "wait-1",
		CheckpointId: "checkpoint-1", CorrelationId: "correlation-1",
	}
	var body bytes.Buffer
	if err := frameio.WriteProtoFrame(&body, request); err != nil {
		t.Fatal(err)
	}
	stream := &scriptedNetConn{reader: bytes.NewReader(body.Bytes())}
	if err := handleProgramRestoreVerifyConnection(stream, 0, mounts, waits); err != nil {
		t.Fatal(err)
	}
	var response workspacev0.VerifyProgramRestoreResponse
	if err := frameio.ReadProtoFrame(bytes.NewReader(stream.written.Bytes()), &response); err != nil {
		t.Fatal(err)
	}
	if response.GetCheckpointId() != request.GetCheckpointId() || response.GetCorrelationId() != request.GetCorrelationId() {
		t.Fatalf("verification response = %+v", &response)
	}
	if mounts.childAdmission == nil || mounts.childAdmission.entry != entry {
		t.Fatal("verified frozen Program did not authorize one child admission")
	}
	request.CorrelationId = "different"
	body.Reset()
	if err := frameio.WriteProtoFrame(&body, request); err != nil {
		t.Fatal(err)
	}
	stream = &scriptedNetConn{reader: bytes.NewReader(body.Bytes())}
	if err := handleProgramRestoreVerifyConnection(stream, 0, mounts, waits); err == nil {
		t.Fatal("mismatched frozen registration was verified")
	}
}

func TestWorkspaceRunAuthorityRenewalRejectsNonCurrentReceipt(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	entry := testWorkspaceAuthorityEntry()
	authority := testWorkspaceRunAuthority(now.Add(time.Minute))
	if err := entry.installWorkspaceRunAuthority(authority, now); err != nil {
		t.Fatal(err)
	}
	altered := proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority)
	altered.GetFence().WorkerEpoch++
	if _, err := entry.renewWorkspaceRunAuthority(&workspacev0.RenewWorkspaceAuthorityRequest{
		Previous:             altered,
		NewExpiresAtUnixNano: now.Add(2 * time.Minute).UnixNano(),
	}, now); err == nil {
		t.Fatal("altered receipt was renewed")
	}
	if _, err := entry.renewWorkspaceRunAuthority(&workspacev0.RenewWorkspaceAuthorityRequest{
		Previous:             authority,
		NewExpiresAtUnixNano: authority.GetFence().GetExpiresAtUnixNano(),
	}, now); err == nil {
		t.Fatal("non-advancing expiry was renewed")
	}
}

func TestWorkspaceRunAuthorityRenewalRejectsExpiredCurrentAuthority(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	entry := testWorkspaceAuthorityEntry()
	authority := testWorkspaceRunAuthority(now.Add(time.Minute))
	if err := entry.installWorkspaceRunAuthority(authority, now); err != nil {
		t.Fatal(err)
	}
	if _, err := entry.renewWorkspaceRunAuthority(&workspacev0.RenewWorkspaceAuthorityRequest{
		Previous:             authority,
		NewExpiresAtUnixNano: now.Add(2 * time.Minute).UnixNano(),
	}, now.Add(time.Minute)); err == nil {
		t.Fatal("expired authority was renewed")
	}
}

func TestWorkspaceAuthorityRenewalSamplesTimeAfterReadingRequest(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	entry := testWorkspaceAuthorityEntry()
	authority := testWorkspaceRunAuthority(now.Add(time.Minute))
	if err := entry.installWorkspaceRunAuthority(authority, now); err != nil {
		t.Fatal(err)
	}
	registry := newWorkspaceOperationRegistry()
	registry.register("mount-1", entry)
	var stream bytes.Buffer
	if err := frameio.WriteProtoFrame(&stream, &workspacev0.RenewWorkspaceAuthorityRequest{
		Previous:             proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority),
		NewExpiresAtUnixNano: now.Add(2 * time.Minute).UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	clockCalledAfterRead := false
	if err := handleWorkspaceAuthorityRenew(&stream, registry, func() time.Time {
		clockCalledAfterRead = stream.Len() == 0
		return now
	}); err != nil {
		t.Fatal(err)
	}
	if !clockCalledAfterRead {
		t.Fatal("renewal sampled time before consuming the request")
	}
}

func TestWorkspaceAuthorityRenewalRebindsActiveProgramClaim(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	entry := testWorkspaceAuthorityEntry()
	authority := testWorkspaceRunAuthority(now.Add(time.Minute))
	registry := newWorkspaceOperationRegistry()
	registry.register("mount-1", entry)
	releaseProgram, err := registry.admitProgram(entry, authority, now)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseProgram()
	var stream bytes.Buffer
	newExpiry := now.Add(2 * time.Minute)
	if err := frameio.WriteProtoFrame(&stream, &workspacev0.RenewWorkspaceAuthorityRequest{
		Previous:             proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority),
		NewExpiresAtUnixNano: newExpiry.UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := handleWorkspaceAuthorityRenew(&stream, registry, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	registry.mu.Lock()
	claimExpiry := registry.programClaims[0].authority.GetFence().GetExpiresAtUnixNano()
	registry.mu.Unlock()
	if claimExpiry != newExpiry.UnixNano() {
		t.Fatalf("active Program claim expiry = %d, want %d", claimExpiry, newExpiry.UnixNano())
	}
}

func TestWorkspaceAuthorityRenewalRejectsMismatchedActiveProgramClaimWithoutMutation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	entry := testWorkspaceAuthorityEntry()
	authority := testWorkspaceRunAuthority(now.Add(time.Minute))
	registry := newWorkspaceOperationRegistry()
	registry.register("mount-1", entry)
	releaseProgram, err := registry.admitProgram(entry, authority, now)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseProgram()
	registry.mu.Lock()
	registry.programClaims[0].authority.GetFence().RunLeaseId = "newer-run-lease"
	registry.mu.Unlock()
	if _, err := registry.renewCurrentWorkspaceRunAuthority(entry, &workspacev0.RenewWorkspaceAuthorityRequest{
		Previous:             proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority),
		NewExpiresAtUnixNano: now.Add(2 * time.Minute).UnixNano(),
	}, now); err == nil {
		t.Fatal("renewal accepted a mismatched active Program claim")
	}
	entry.authorityMu.Lock()
	expiry := entry.authority.GetFence().GetExpiresAtUnixNano()
	entry.authorityMu.Unlock()
	if expiry != authority.GetFence().GetExpiresAtUnixNano() {
		t.Fatalf("failed renewal mutated Workspace authority expiry = %d", expiry)
	}
}

func TestWorkspaceFinalizationBeginFreezesAuthorityAndReplays(t *testing.T) {
	entry, registry, authority := testWorkspaceFinalizationMount(t)
	now := time.Now()
	request := testWorkspaceFinalizationBeginRequest(authority, "11111111-1111-4111-8111-111111111111", workspace.FinalizationCaptureKind)

	first, err := beginTestWorkspaceFinalizationRequest(context.Background(), registry, request, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.GetFence().GetExpiresAtUnixNano() != request.GetFinalizationExpiresAtUnixNano() ||
		first.GetOperationId() != request.GetOperationId() || first.GetKind() != request.GetKind() {
		t.Fatalf("Begin response = %+v", first)
	}
	entry.processesMu.Lock()
	state, operationID, kind := entry.authorityState, entry.finalizationID, entry.finalizationKind
	entry.processesMu.Unlock()
	if state != workspaceAuthorityFinalizing || operationID != request.GetOperationId() || kind != request.GetKind() {
		t.Fatalf("finalization state = %d %q %q", state, operationID, kind)
	}
	journal, found, err := entry.readWorkspaceFinalizationJournal()
	if err != nil || !found {
		t.Fatalf("Begin journal = %+v found=%t err=%v", journal, found, err)
	}
	if journal.Phase != "begun" || journal.OperationID != request.GetOperationId() ||
		journal.Fence.ExpiresAtUnixNano != request.GetFinalizationExpiresAtUnixNano() {
		t.Fatalf("Begin journal = %+v", journal)
	}

	replayed, err := beginTestWorkspaceFinalizationRequest(context.Background(), registry, request, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(first, replayed) {
		t.Fatalf("replay = %+v, want %+v", replayed, first)
	}

	changed := proto.Clone(request).(*workspacev0.BeginWorkspaceFinalizationRequest)
	changed.Kind = workspace.FinalizationResetKind
	if _, err := beginTestWorkspaceFinalizationRequest(context.Background(), registry, changed, now.Add(time.Second)); err == nil {
		t.Fatal("changed Begin replay was accepted")
	}
	if _, err := entry.renewWorkspaceRunAuthority(&workspacev0.RenewWorkspaceAuthorityRequest{
		Previous:             proto.Clone(entry.authority).(*workspacev0.WorkspaceRunAuthority),
		NewExpiresAtUnixNano: request.GetFinalizationExpiresAtUnixNano() + int64(time.Minute),
	}, now.Add(time.Second)); err == nil {
		t.Fatal("finalizing authority was renewed")
	}
}

func TestWorkspaceFinalizationBeginRecoversDurableBeginBeforeAck(t *testing.T) {
	entry, registry, authority := testWorkspaceFinalizationMount(t)
	request := testWorkspaceFinalizationBeginRequest(authority, "11111111-1111-4111-8111-111111111111", workspace.FinalizationResetKind)
	frozen := proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority)
	frozen.GetFence().ExpiresAtUnixNano = request.GetFinalizationExpiresAtUnixNano()
	if err := entry.writeWorkspaceFinalizationJournal(workspaceFinalizationJournal{
		Version: workspaceFinalizationJournalVersion, Kind: request.GetKind(),
		OperationID: request.GetOperationId(), Fence: workspaceFinalizationFence(frozen.GetFence()), Phase: "begun",
	}); err != nil {
		t.Fatal(err)
	}

	retryAt := time.Unix(0, authority.GetFence().GetExpiresAtUnixNano()).Add(time.Nanosecond)
	response, err := beginTestWorkspaceFinalizationRequest(context.Background(), registry, request, retryAt)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetFence().GetExpiresAtUnixNano() != request.GetFinalizationExpiresAtUnixNano() ||
		entry.authorityState != workspaceAuthorityFinalizing {
		t.Fatalf("recovered Begin = %+v state=%d", response, entry.authorityState)
	}
}

func TestWorkspaceFinalizationBeginSamplesTimeAfterReadingRequest(t *testing.T) {
	_, registry, authority := testWorkspaceFinalizationMount(t)
	now := time.Now()
	request := testWorkspaceFinalizationBeginRequest(authority, "11111111-1111-4111-8111-111111111111", workspace.FinalizationCaptureKind)
	var stream bytes.Buffer
	if err := frameio.WriteProtoFrame(&stream, request); err != nil {
		t.Fatal(err)
	}
	clockCalledAfterRead := false
	if err := handleWorkspaceFinalizationBegin(&stream, registry, func() time.Time {
		clockCalledAfterRead = stream.Len() == 0
		return now
	}); err != nil {
		t.Fatal(err)
	}
	if !clockCalledAfterRead {
		t.Fatal("Begin sampled time before consuming the request")
	}
	var response workspacev0.BeginWorkspaceFinalizationResponse
	if err := frameio.ReadProtoFrame(&stream, &response); err != nil {
		t.Fatal(err)
	}
	if response.GetError() != "" || response.GetOperationId() != request.GetOperationId() {
		t.Fatalf("Begin response = %+v", &response)
	}
}

func beginTestWorkspaceFinalizationRequest(
	_ context.Context,
	registry *workspaceOperationRegistry,
	request *workspacev0.BeginWorkspaceFinalizationRequest,
	now time.Time,
) (*workspacev0.BeginWorkspaceFinalizationResponse, error) {
	previous := request.GetPrevious()
	fence := previous.GetFence()
	entry, release, ok := registry.acquireExact(
		fence.GetWorkspaceMountId(), fence.GetWorkspaceId(), previous.GetChannelToken(),
		uint64(fence.GetMountFencingGeneration()),
	)
	if !ok {
		return nil, errors.New("test authority does not match mounted Workspace")
	}
	defer release()
	return registry.beginCurrentWorkspaceFinalization(entry, request, func() time.Time { return now })
}

func testWorkspaceFinalizationBeginRequest(
	authority *workspacev0.WorkspaceRunAuthority,
	operationID string,
	kind string,
) *workspacev0.BeginWorkspaceFinalizationRequest {
	return &workspacev0.BeginWorkspaceFinalizationRequest{
		Previous:                      proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority),
		FinalizationExpiresAtUnixNano: authority.GetFence().GetExpiresAtUnixNano() + int64(time.Minute),
		OperationId:                   operationID,
		Kind:                          kind,
	}
}

func testWorkspaceAuthorityEntry() *workspaceMountEntry {
	return &workspaceMountEntry{
		workspaceMountID:  "mount-1",
		workspaceID:       "workspace-1",
		baseVersionID:     "version-1",
		channelToken:      "channel-1",
		fencingGeneration: 4,
		runtimeInstanceID: "runtime-1",
	}
}

func testWorkspaceRunAuthority(expiresAt time.Time) *workspacev0.WorkspaceRunAuthority {
	return &workspacev0.WorkspaceRunAuthority{
		Fence: &workspacev0.WorkspaceAuthorityFence{
			WorkerInstanceId:       "worker-1",
			WorkerEpoch:            7,
			RuntimeInstanceId:      "runtime-1",
			RuntimeIdentityId:      "runtime-identity-1",
			WorkspaceId:            "workspace-1",
			WorkspaceMountId:       "mount-1",
			RunId:                  "run-1",
			AttemptNumber:          2,
			RunLeaseId:             "run-lease-1",
			LeaseSequence:          5,
			WorkspaceLeaseId:       "workspace-lease-1",
			OwnershipGeneration:    2,
			WriterGeneration:       3,
			MountFencingGeneration: 4,
			ExpiresAtUnixNano:      expiresAt.UnixNano(),
			BaseWorkspaceVersionId: "version-1",
		},
		ChannelToken:    "channel-1",
		WriteCapability: "write-capability",
	}
}
