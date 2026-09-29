package executor

import (
	"bufio"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

type restoredProgramHarness struct {
	mountedMachine
	RunLeaseControlPlane
	SessionExecutionControlPlane
	actor        bool
	installed    *computerv0.ComputerRunAuthority
	mu           sync.Mutex
	acked, polls int
	err          chan error
}

func (h *restoredProgramHarness) ReadSessionControl(_ context.Context, r workerapi.SessionControlRequest) (workerapi.SessionControlResponse, error) {
	return workerapi.SessionControlResponse{CorrelationID: r.CorrelationID}, nil
}
func (h *restoredProgramHarness) CreateRunWait(context.Context, workerapi.CreateRunWaitRequest) (workerapi.CreateRunWaitResponse, error) {
	return workerapi.CreateRunWaitResponse{}, errors.New("restored wait must not be registered again")
}
func (h *restoredProgramHarness) AcknowledgeRunWaitResume(_ context.Context, r workerapi.RunWaitResumeAckRequest) (workerapi.RunWaitResumeAckResponse, error) {
	h.mu.Lock()
	h.acked++
	h.mu.Unlock()
	return workerapi.RunWaitResumeAckResponse{RunID: "run", RunWaitID: r.RunWaitID, CheckpointID: r.CheckpointID}, nil
}
func (h *restoredProgramHarness) PollRunWait(_ context.Context, r workerapi.RunWaitPollRequest) (workerapi.RunWaitPollResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.polls++
	if h.acked != 1 {
		return workerapi.RunWaitPollResponse{}, errors.New("poll before attachment acknowledgement")
	}
	response := workerapi.RunWaitPollResponse{RunID: "run", RunWaitID: r.RunWaitID, Status: workerapi.RunWaitPollStatusWaiting}
	if h.polls > 1 {
		response.Status = workerapi.RunWaitPollStatusResumeRequested
		response.ResumeKind = "completed"
		response.ResumePayload = []byte("null")
	}
	return response, nil
}
func (h *restoredProgramHarness) OpenStream(context.Context) (vm.Stream, error) {
	host, guest := net.Pipe()
	go func() { defer guest.Close(); h.err <- h.serve(guest) }()
	return host, nil
}
func (h *restoredProgramHarness) serve(stream net.Conn) error {
	reader := bufio.NewReader(stream)
	prefix, err := reader.Peek(4)
	if err != nil {
		return err
	}
	if frameio.IsStreamFramePrefix(prefix) {
		header, _, err := wire.ReadStreamFrameHeader(reader)
		if err != nil {
			return err
		}
		if header.Type != wire.StreamTypeProgramResumeGrant {
			return errors.New("restore attempted a new Program admission")
		}
		var q computerv0.GrantProgramResumeRequest
		if err := frameio.ReadProtoFrame(reader, &q); err != nil {
			return err
		}
		if !proto.Equal(q.Authority, h.installed) {
			return errors.New("resume grant differs from materializer installation")
		}
		a := &programv0.ResumeAttach{RunId: q.Authority.Fence.RunId, AttemptNumber: q.Authority.Fence.AttemptNumber, RunLeaseId: q.Authority.Fence.RunLeaseId, RunWaitId: q.RunWaitId, CheckpointId: q.CheckpointId, ResumeRequestVersion: 7, ResumeAttachId: "attach", CorrelationId: "correlation"}
		if h.actor {
			a.Execution = &programv0.SessionExecution{SessionId: "01950000-0000-7000-8000-000000000001", RunId: a.RunId, AttemptNumber: a.AttemptNumber, RunGeneration: 3}
		}
		return frameio.WriteProtoFrame(stream, &computerv0.GrantProgramResumeResponse{Fence: q.Authority.Fence, Attach: a})
	}
	var a programv0.ResumeAttach
	if err := frameio.ReadProtoFrame(reader, &a); err != nil {
		return err
	}
	var decision programv0.ResumeDecision
	if err := frameio.ReadProtoFrame(reader, &decision); err != nil {
		return err
	}
	if decision.Kind != "waiting" || !decision.RequireConsumedAck || decision.RunLeaseId != a.RunLeaseId {
		return errors.New("physical attachment resolved a pending wait")
	}
	if err := frameio.WriteProtoFrame(stream, &programv0.ResumeAck{RunWaitId: a.RunWaitId, CheckpointId: a.CheckpointId, ResumeAttachId: a.ResumeAttachId, ResumeRequestVersion: a.ResumeRequestVersion, RunLeaseId: a.RunLeaseId, CorrelationId: a.CorrelationId}); err != nil {
		return err
	}
	header, size, err := wire.ReadStreamFrameHeader(reader)
	if err != nil {
		return err
	}
	resolved, err := wire.ReadResumeDecision(header, reader, size)
	if err != nil {
		return err
	}
	if resolved.Kind != "completed" || resolved.RequireConsumedAck {
		return errors.New("invalid hot wait completion")
	}
	outcome := &programv0.RunEvent{Event: &programv0.RunEvent_TaskOutcome{TaskOutcome: &programv0.TaskOutcome{Outcome: &programv0.TaskOutcome_Succeeded{Succeeded: &programv0.TaskSucceeded{OutputJson: `{"ok":true}`}}}}}
	if h.actor {
		outcome = &programv0.RunEvent{Event: &programv0.RunEvent_ActorOutcome{ActorOutcome: &programv0.ActorOutcome{RunGeneration: 3, Outcome: &programv0.ActorOutcome_Succeeded{Succeeded: &programv0.ActorSucceeded{}}}}}
	}
	if err := frameio.WriteProtoFrame(stream, outcome); err != nil {
		return err
	}

	return frameio.WriteProtoFrame(stream, &programv0.RunEvent{Event: &programv0.RunEvent_ProgramQuiesced{ProgramQuiesced: &programv0.ProgramQuiesced{RunId: a.RunId, AttemptNumber: a.AttemptNumber, RunLeaseId: a.RunLeaseId}}})
}
func TestRestoredProgramReattachesAndContinuesExistingWait(t *testing.T) {
	for _, kind := range []string{"task", "actor"} {
		t.Run(kind, func(t *testing.T) { testRestoredProgram(t, kind) })
	}
}
func testRestoredProgram(t *testing.T, kind string) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	h := &restoredProgramHarness{actor: kind == "actor", err: make(chan error, 2)}
	mounts := NewMounts()
	mount := workerapi.ComputerInstanceAssignment{ComputerInstanceID: "instance", ComputerID: "computer", WriterGeneration: 2, RestoreCheckpointID: "checkpoint", DesiredVersion: 4, RuntimeEpoch: 1, VMPlatformID: "platform", GuestdChannelToken: "channel", Target: workerapi.ComputerMountTarget{BaseComputerDiskVersionID: "private-disk"}}
	unregister := mounts.Register(mount, newInstanceMount(h), "channel")
	defer unregister()
	claim := &workerapi.RunLeaseClaimResponse{Lease: workerapi.RunLeaseAssignment{ID: "lease", RunID: "run", AttemptNumber: 1, LeaseSequence: 2, ComputerInstanceID: "instance", ComputerID: "computer", WriterGeneration: 2, WorkerEpoch: 1, WorkerHostID: "worker", VMPlatformID: "platform", BaseComputerDiskVersionID: "disk", ExpiresAt: time.Now().Add(time.Minute)}, Computer: workerapi.ComputerAttachment{WriteCapability: "capability", Target: workerapi.ComputerMountTarget{BaseComputerDiskVersionID: "disk"}}, ProgramResume: &workerapi.ProgramResume{CheckpointID: "checkpoint", RunWaitID: "wait", EntrypointKind: kind}}
	activation := &restorePlanHarness{restoreActivationHarness: &restoreActivationHarness{}, planCalls: 1, plan: &workerapi.ComputerRestorePlan{
		ComputerInstanceID: mount.ComputerInstanceID, ComputerID: mount.ComputerID, CheckpointID: mount.RestoreCheckpointID, WriterGeneration: mount.WriterGeneration, WorkerEpoch: claim.Lease.WorkerEpoch, WorkerHostID: claim.Lease.WorkerHostID, VMPlatformID: claim.Lease.VMPlatformID, DesiredVersion: mount.DesiredVersion, WriteCapability: claim.Computer.WriteCapability,
		Members: []workerapi.ComputerRestoreMember{{RunID: claim.Lease.RunID, AttemptNumber: claim.Lease.AttemptNumber, Lease: claim.Lease.Fence(), BaseComputerDiskVersionID: claim.Lease.BaseComputerDiskVersionID, ExpiresAt: claim.Lease.ExpiresAt}},
	}}
	// Exercise the actual materializer projection before the runner rebuilds the
	// same grant. The private physical restore source differs from the Run's base.
	if err := (ComputerMaterializer{RestoreControl: activation}).activateRestore(ctx, activation, mount); err != nil {
		t.Fatal(err)
	}
	activation.mu.Lock()
	h.installed = proto.Clone(activation.installation.Grants[0]).(*computerv0.ComputerRunAuthority)
	activation.mu.Unlock()
	task, err := (ProgramRunner{ControlPlane: testControlPlane(t, h), Mounts: mounts, CAS: &checkpointCAS{}, ComputerCaptures: &ComputerCaptureRuns{}}).StartRunLeaseTask(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	defer task.Close()
	result, err := task.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.ProgramQuiesced.RunLeaseID != "lease" || (kind == "task" && (result.Outcome.Succeeded == nil || string(result.Outcome.Succeeded.Output) != `{"ok":true}`)) || (kind == "actor" && (result.ActorOutcome == nil || result.ActorOutcome.Succeeded == nil || result.ActorOutcome.RunGeneration != 3)) {
		t.Fatal("restored task did not finish under current authority")
	}
	for range 2 {
		if err := <-h.err; err != nil {
			t.Fatal(err)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.acked != 1 || h.polls != 2 {
		t.Fatalf("ack=%d polls=%d", h.acked, h.polls)
	}
}
