package computerhost_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/computerhost"
	"github.com/helmrdotdev/helmr/internal/executor"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// rejectedTurnCommit is a Control Plane that rejects the Actor turn commit.
type rejectedTurnCommit struct {
	executor.RunLeaseControlPlane
	err error
}

func (c rejectedTurnCommit) CommitActorTurn(context.Context, workerapi.CommitActorTurnRequest) (workerapi.CommitActorTurnResponse, error) {
	return workerapi.CommitActorTurnResponse{}, c.err
}

func TestTurnSettlementFailureReleasesMountedComputerThroughBoundSource(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	commitErr, releaseErr := &httpclient.Error{StatusCode: http.StatusConflict}, errors.New("physical stop failed")
	mounts := computerhost.NewMounts()
	mount := workerapi.ComputerInstanceAssignment{ComputerInstanceID: "instance", ComputerID: "computer", WriterGeneration: 2, RestoreCheckpointID: "checkpoint", DesiredVersion: 4, WorkerEpoch: 1, VMPlatformID: "platform", GuestChannelCredential: "channel-1", Target: workerapi.ComputerMountTarget{BaseComputerDiskVersionID: "disk"}}
	claim := restoredClaim(mount, workerapi.RunLeaseAssignment{ID: "lease", RunID: "run", AttemptNumber: 1, LeaseSequence: 2, ComputerInstanceID: "instance", ComputerID: "computer", WriterGeneration: 2, WorkerEpoch: 1, WorkerHostID: "worker", VMPlatformID: "platform", BaseComputerDiskVersionID: "disk", ExpiresAt: time.Now().Add(time.Minute)}, "wait", "actor")
	h := newRestoredProgramHarness(mount, claim)
	h.actor, h.resume, h.closeErr = true, true, releaseErr
	// Once its wait resumes, the Actor settles its turn on the borrowed stream
	// and then waits for a decision that never comes.
	h.program = func(stream net.Conn, reader *bufio.Reader, attach *programv0.ResumeAttach) error {
		if err := readHotWaitCompletion(reader); err != nil {
			return err
		}
		turn := &programv0.TurnExecution{Session: attach.Execution, TurnId: "019c10d5-a6f7-7af1-8f5f-000000000112"}
		if err := frameio.WriteProtoFrame(stream, &programv0.RunEvent{Event: &programv0.RunEvent_TurnSettleRequested{TurnSettleRequested: &programv0.TurnSettleRequested{Execution: turn, CorrelationId: "019c10d5-a6f7-7af1-8f5f-000000000099", TargetInputSequence: 1, Disposition: "completed"}}}); err != nil {
			return err
		}
		_, _ = reader.ReadByte()
		return nil
	}
	unregister, err := computerhost.MountComputer(ctx, mounts, h, h, mount)
	if err != nil {
		t.Fatal(err)
	}
	defer unregister()
	task, err := (executor.ProgramRunner{ControlPlane: testControlPlane(t, rejectedTurnCommit{err: commitErr}, h), Mounts: mounts, CAS: unusedCAS{}, ComputerCaptures: &computerhost.CaptureRuns{}}).StartRunLeaseTask(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	defer task.Close()
	_, err = task.Wait(ctx)
	var stopErr *computerhost.SourceReleaseError
	if !errors.As(err, &stopErr) || !errors.Is(err, releaseErr) || !errors.Is(err, commitErr) {
		t.Fatalf("err=%v", err)
	}
	if closes := h.physicalCloses(); closes != 1 {
		t.Fatalf("physical closes = %d, want 1", closes)
	}
	if released, err := computerhost.MountReleaseResult(ctx, mounts, claim.Lease.ComputerInstanceID); !released || !errors.Is(err, releaseErr) {
		t.Fatalf("checkpoint release result = %t, %v", released, err)
	}
	// The mount retains its release result; a later release neither stops the
	// machine again nor reports a different outcome.
	opened, err := mounts.OpenChannel(ctx, claim.Lease.ComputerInstanceID)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Channel.Close(context.Background())
	if err := opened.ReleaseSource(ctx); !errors.Is(err, releaseErr) || h.physicalCloses() != 1 {
		t.Fatalf("retained release = %v, physical closes = %d", err, h.physicalCloses())
	}
}
