package guestd

import (
	"bytes"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
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
