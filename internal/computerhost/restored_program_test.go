package computerhost_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computerhost"
	"github.com/helmrdotdev/helmr/internal/executor"
	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"google.golang.org/protobuf/proto"
)

// restoredProgramHarness is the restored Computer's guest and the Control
// Plane. The physical owner installs the restore on it before each runner
// resumes its Program over the same machine.
type restoredProgramHarness struct {
	executor.RunLeaseControlPlane
	executor.SessionExecutionControlPlane
	actor bool
	// resume lets each Run's second poll resume its wait. Otherwise every
	// restored wait stays pending.
	resume bool
	// program serves a Run's Program stream once its restored wait is
	// reattached. Nil completes the wait and then the Run.
	program func(stream net.Conn, reader *bufio.Reader, attach *programv0.ResumeAttach) error
	// closeErr is the physical machine's stop result.
	closeErr error
	plan     *workerapi.ComputerRestorePlan
	runs     map[string]string

	mu           sync.Mutex
	installed    map[string]*computerv0.ComputerRunAuthority
	restoreSteps []string
	acked, polls map[string]int
	polled       map[string]chan struct{}
	closes       int
	err          chan error
}

// newRestoredProgramHarness restores claims, all Runs of mount's Instance, as
// the members of one committed restore plan.
func newRestoredProgramHarness(mount workerapi.ComputerInstanceAssignment, claims ...*workerapi.RunLeaseClaimResponse) *restoredProgramHarness {
	h := &restoredProgramHarness{runs: map[string]string{}, installed: map[string]*computerv0.ComputerRunAuthority{}, acked: map[string]int{}, polls: map[string]int{}, polled: map[string]chan struct{}{}, err: make(chan error, 16)}
	first := claims[0]
	h.plan = &workerapi.ComputerRestorePlan{
		ComputerInstanceID: mount.ComputerInstanceID, ComputerID: mount.ComputerID, CheckpointID: mount.RestoreCheckpointID, WriterGeneration: mount.WriterGeneration, WorkerEpoch: first.Lease.WorkerEpoch, WorkerHostID: first.Lease.WorkerHostID, VMPlatformID: first.Lease.VMPlatformID, DesiredVersion: mount.DesiredVersion, WriteCapability: first.Computer.WriteCapability,
	}
	for _, claim := range claims {
		h.plan.Members = append(h.plan.Members, workerapi.ComputerRestoreMember{RunID: claim.Lease.RunID, AttemptNumber: claim.Lease.AttemptNumber, Lease: claim.Lease.Fence(), BaseComputerDiskVersionID: claim.Lease.BaseComputerDiskVersionID, ExpiresAt: claim.Lease.ExpiresAt})
		h.runs[claim.ProgramResume.RunWaitID] = claim.Lease.RunID
		h.polled[claim.Lease.RunID] = make(chan struct{})
	}
	return h
}

func (h *restoredProgramHarness) Stream() vm.Stream { return nil }
func (h *restoredProgramHarness) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
func (h *restoredProgramHarness) Close(context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closes++
	return h.closeErr
}
func (h *restoredProgramHarness) physicalCloses() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.closes
}
func (h *restoredProgramHarness) GetComputerRestorePlan(context.Context, workerapi.ComputerRestorePlanRequest) (workerapi.ComputerRestorePlanResponse, error) {
	return workerapi.ComputerRestorePlanResponse{Plan: h.plan}, nil
}
func (h *restoredProgramHarness) AcknowledgeComputerRestore(_ context.Context, q workerapi.ComputerRestoreAckRequest) (workerapi.ComputerRestoreAckResponse, error) {
	h.mu.Lock()
	h.restoreSteps = append(h.restoreSteps, "ack")
	h.mu.Unlock()
	return workerapi.ComputerRestoreAckResponse{ComputerInstanceID: q.ComputerInstanceID, CheckpointID: q.CheckpointID, DesiredVersion: q.DesiredVersion, WriterGeneration: q.WriterGeneration}, nil
}

func (h *restoredProgramHarness) ReadSessionControl(_ context.Context, r workerapi.SessionControlRequest) (workerapi.SessionControlResponse, error) {
	return workerapi.SessionControlResponse{CorrelationID: r.CorrelationID}, nil
}
func (h *restoredProgramHarness) CreateRunWait(context.Context, workerapi.CreateRunWaitRequest) (workerapi.CreateRunWaitResponse, error) {
	return workerapi.CreateRunWaitResponse{}, errors.New("restored wait must not be registered again")
}
func (h *restoredProgramHarness) AcknowledgeRunWaitResume(_ context.Context, r workerapi.RunWaitResumeAckRequest) (workerapi.RunWaitResumeAckResponse, error) {
	h.mu.Lock()
	h.acked[r.RunWaitID]++
	h.mu.Unlock()
	return workerapi.RunWaitResumeAckResponse{RunID: h.runs[r.RunWaitID], RunWaitID: r.RunWaitID, CheckpointID: r.CheckpointID}, nil
}
func (h *restoredProgramHarness) PollRunWait(_ context.Context, r workerapi.RunWaitPollRequest) (workerapi.RunWaitPollResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.polls[r.RunWaitID]++
	if h.acked[r.RunWaitID] != 1 {
		return workerapi.RunWaitPollResponse{}, errors.New("poll before attachment acknowledgement")
	}
	run := h.runs[r.RunWaitID]
	if h.polls[r.RunWaitID] == 1 {
		close(h.polled[run])
	}
	response := workerapi.RunWaitPollResponse{RunID: run, RunWaitID: r.RunWaitID, Status: workerapi.RunWaitPollStatusWaiting}
	if h.resume && h.polls[r.RunWaitID] > 1 {
		response.Status = workerapi.RunWaitPollStatusResumeRequested
		response.ResumeKind = "completed"
		response.ResumePayload = []byte("null")
	}
	return response, nil
}

// awaitPolled returns once runID's reattached wait has been polled. The Run
// registers its capture wait before it starts polling.
func (h *restoredProgramHarness) awaitPolled(ctx context.Context, runID string) error {
	select {
	case <-h.polled[runID]:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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
		if header.Type == wire.StreamTypeComputerRestoreInstall || header.Type == wire.StreamTypeComputerRestoreActivate {
			var installation computerv0.ComputerRestoreInstallation
			if err := frameio.ReadProtoFrame(reader, &installation); err != nil {
				return err
			}
			if len(installation.Grants) != len(h.plan.Members) {
				return errors.New("restore installation does not carry the member grants")
			}
			h.mu.Lock()
			h.restoreSteps = append(h.restoreSteps, string(header.Type))
			for _, grant := range installation.Grants {
				h.installed[grant.GetFence().GetRunId()] = proto.Clone(grant).(*computerv0.ComputerRunAuthority)
			}
			h.mu.Unlock()
			return frameio.WriteProtoFrame(stream, &computerv0.ComputerRestoreInstallationResponse{Installation: &installation})
		}
		if header.Type != wire.StreamTypeProgramResumeGrant {
			return errors.New("restore attempted a new Program admission")
		}
		var q computerv0.GrantProgramResumeRequest
		if err := frameio.ReadProtoFrame(reader, &q); err != nil {
			return err
		}
		h.mu.Lock()
		installed := h.installed[q.GetAuthority().GetFence().GetRunId()]
		h.mu.Unlock()
		if installed == nil || !proto.Equal(q.Authority, installed) {
			return errors.New("resume grant differs from server installation")
		}
		a := &programv0.ResumeAttach{RunId: q.Authority.Fence.RunId, AttemptNumber: q.Authority.Fence.AttemptNumber, RunLeaseId: q.Authority.Fence.RunLeaseId, RunWaitId: q.RunWaitId, CheckpointId: q.CheckpointId, ResumeRequestVersion: 7, ResumeAttachId: "attach-" + q.Authority.Fence.RunId, CorrelationId: "correlation-" + q.Authority.Fence.RunId}
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
	if h.program != nil {
		return h.program(stream, reader, &a)
	}
	if err := readHotWaitCompletion(reader); err != nil {
		return err
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

// readHotWaitCompletion reads the resolution of a reattached hot wait.
func readHotWaitCompletion(reader *bufio.Reader) error {
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
	return nil
}

// restoredClaim is a restored Run's claim on mount, reattaching its wait.
func restoredClaim(mount workerapi.ComputerInstanceAssignment, lease workerapi.RunLeaseAssignment, waitID, kind string) *workerapi.RunLeaseClaimResponse {
	return &workerapi.RunLeaseClaimResponse{Lease: lease, Computer: workerapi.ComputerAttachment{WriteCapability: "capability", Target: workerapi.ComputerMountTarget{BaseComputerDiskVersionID: lease.BaseComputerDiskVersionID}}, ProgramResume: &workerapi.ProgramResume{CheckpointID: mount.RestoreCheckpointID, RunWaitID: waitID, EntrypointKind: kind}}
}

// unusedCAS satisfies a required CAS collaborator that the test never reads.
type unusedCAS struct{ cas.Store }

func TestRestoredProgramReattachesAndContinuesExistingWait(t *testing.T) {
	for _, kind := range []string{"task", "actor"} {
		t.Run(kind, func(t *testing.T) { testRestoredProgram(t, kind) })
	}
}
func testRestoredProgram(t *testing.T, kind string) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	mounts := computerhost.NewMounts()
	mount := workerapi.ComputerInstanceAssignment{ComputerInstanceID: "instance", ComputerID: "computer", WriterGeneration: 2, RestoreCheckpointID: "checkpoint", DesiredVersion: 4, RuntimeEpoch: 1, VMPlatformID: "platform", GuestdChannelToken: "channel", Target: workerapi.ComputerMountTarget{BaseComputerDiskVersionID: "private-disk"}}
	claim := restoredClaim(mount, workerapi.RunLeaseAssignment{ID: "lease", RunID: "run", AttemptNumber: 1, LeaseSequence: 2, ComputerInstanceID: "instance", ComputerID: "computer", WriterGeneration: 2, WorkerEpoch: 1, WorkerHostID: "worker", VMPlatformID: "platform", BaseComputerDiskVersionID: "disk", ExpiresAt: time.Now().Add(time.Minute)}, "wait", kind)
	h := newRestoredProgramHarness(mount, claim)
	h.actor, h.resume = kind == "actor", true
	// The physical owner installs the restore on the machine before the runner
	// rebuilds the same grant. The private physical restore source differs from
	// the Run's base.
	unregister, err := computerhost.MountComputer(ctx, mounts, h, h, mount)
	if err != nil {
		t.Fatal(err)
	}
	defer unregister()
	h.mu.Lock()
	steps := append([]string(nil), h.restoreSteps...)
	h.mu.Unlock()
	if want := []string{string(wire.StreamTypeComputerRestoreInstall), "ack", string(wire.StreamTypeComputerRestoreActivate)}; !slices.Equal(steps, want) {
		t.Fatalf("restore steps = %v, want %v", steps, want)
	}
	task, err := (executor.ProgramRunner{ControlPlane: testControlPlane(t, h), Mounts: mounts, CAS: unusedCAS{}, ComputerCaptures: &computerhost.CaptureRuns{}}).StartRunLeaseTask(ctx, claim)
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
	for range 4 {
		if err := <-h.err; err != nil {
			t.Fatal(err)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.acked["wait"] != 1 || h.polls["wait"] != 2 {
		t.Fatalf("ack=%d polls=%d", h.acked["wait"], h.polls["wait"])
	}
}
