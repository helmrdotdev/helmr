package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

type actorTurnCommitControl struct {
	*testRunLeaseControl
	request api.WorkerCommitActorTurnRequest
}

func (control *actorTurnCommitControl) CommitActorTurn(
	_ context.Context,
	request api.WorkerCommitActorTurnRequest,
) (api.WorkerCommitActorTurnResponse, error) {
	control.request = request
	lease := request.Lease
	lease.BaseWorkspaceVersionID = "version-2"
	return api.WorkerCommitActorTurnResponse{
		Lease: lease, RunID: request.Lease.RunID, AttemptNumber: request.Lease.AttemptNumber,
		RunLeaseID: request.Lease.ID, CorrelationID: request.CorrelationID,
		CommittedInputSequence: request.TargetInputSequence, WorkspaceVersionID: "version-2",
		Tree: request.Tree,
	}, nil
}

func TestHandleActorTurnCommitAdvancesAllLocalWorkspaceFrontiers(t *testing.T) {
	claim := testFreshProgramClaim(t)
	claim.Lease.WorkerGroupID = "workers"
	claim.Lease.WorkerProtocolVersion = api.CurrentWorkerProtocolVersion
	claim.Lease.NetworkSlotID = "slot-1"
	claim.Lease.NetworkSlotGeneration = 1
	claim.Lease.RequestedCPUMillis = 1
	claim.Lease.RequestedMemoryBytes = 1
	claim.Lease.RequestedWorkloadDiskBytes = 1
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
	control := &actorTurnCommitControl{testRunLeaseControl: &testRunLeaseControl{}}
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	authority := freshWorkspaceAuthority(&claim, "channel-1")
	task := &guestRunLeaseTask{
		program: freshProgram{session: fakeGuestSession{stream: host}},
		store:   store, control: control, resetTarget: target, lease: claim.Lease,
		authority: authority, waitWorkspace: api.WorkerWorkspace{BaseVersionID: "version-1"},
		checkpointer: &runtimeCheckpointer{},
	}

	artifactBody := []byte("canonical workspace archive")
	artifactDigest := sha256sum.DigestBytes(artifactBody)
	tree := workspace.TreeIdentity{Digest: sha256sum.DigestBytes([]byte("logical tree")), SizeBytes: 12, EntryCount: 1}
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
			guestResult <- errors.New("pause request carried the wrong Workspace frontier")
			return
		}
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
		if err := wire.WriteActorTurnCommitPauseReady(guest, &runv0.ActorTurnCommitPauseReady{
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
			guestResult <- errors.New("commit decision did not carry the new Workspace frontier")
			return
		}
		guestResult <- nil
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := task.handleActorTurnCommit(ctx, &runv0.ActorTurnCommitRequested{
		CorrelationId: "00000000-0000-0000-0000-000000000099", TargetInputSequence: 1,
	}); err != nil {
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
	if task.waitWorkspace.Artifact == nil || task.waitWorkspace.Artifact.Digest != artifactDigest ||
		task.checkpointer.(*runtimeCheckpointer).workspace.ArtifactDigest != artifactDigest {
		t.Fatal("Actor turn commit did not advance Wait and checkpoint Workspace artifacts")
	}
	if control.request.Artifact == nil || control.request.Artifact.Digest != artifactDigest ||
		control.request.BaseWorkspaceVersionID != "version-1" || control.request.Tree.Digest != tree.Digest {
		t.Fatalf("Actor turn Control request = %+v", control.request)
	}
}
