package guestd

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
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
	if err := handleWorkspaceFinalizationBegin(context.Background(), &stream, registry, func() time.Time {
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
		t.Fatalf("Begin response = %+v", response)
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
