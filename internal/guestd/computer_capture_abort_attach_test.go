package guestd

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

func TestCaptureAbortAttachmentFencesPreparationAndActivation(t *testing.T) {
	r, waits, q := captureAbortFixture(t, 1)
	if err := r.applyCaptureAbort(t.Context(), waits, q, time.Now); err != nil {
		t.Fatal(err)
	}
	q.Activate = true
	if err := r.applyCaptureAbort(t.Context(), waits, q, time.Now); err == nil {
		t.Fatal("activation without transport accepted")
	}
	q.Activate = false
	prepareAbortFixtureStreams(t, r, waits, q)
	member := q.Members[0]
	slot := waits.slots[member.Member.RunWaitId]
	first := slot.abortStream
	prepareAbortFixtureStreams(t, r, waits, q)
	if first == slot.abortStream {
		t.Fatal("preparation did not replace old connection")
	}
	a := &computerv0.ComputerCaptureAbortAttachRequest{CheckpointId: q.Capture.CheckpointId, AbortDesiredVersion: q.AbortDesiredVersion, Member: member.Member, Authority: member.Authority, AttachSequence: member.AttachSequence}
	for _, mutate := range []func(*computerv0.ComputerCaptureAbortAttachRequest){
		func(a *computerv0.ComputerCaptureAbortAttachRequest) { a.AttachSequence-- },
		func(a *computerv0.ComputerCaptureAbortAttachRequest) { a.CheckpointId = "foreign" },
		func(a *computerv0.ComputerCaptureAbortAttachRequest) { a.AbortDesiredVersion++ },
		func(a *computerv0.ComputerCaptureAbortAttachRequest) { a.Authority.Fence.RunLeaseId = "foreign" },
		func(a *computerv0.ComputerCaptureAbortAttachRequest) {
			a.Authority.Fence.ExpiresAtUnixNano = time.Now().Add(-time.Second).UnixNano()
		},
	} {
		bad := proto.Clone(a).(*computerv0.ComputerCaptureAbortAttachRequest)
		bad.AttachSequence++
		mutate(bad)
		guest, host := net.Pipe()
		before := slot.abortStream
		if err := r.parkCaptureAbortStream(waits, bad, guest, time.Now()); err == nil {
			t.Fatal("invalid attachment accepted")
		}
		guest.Close()
		host.Close()
		if slot.abortStream != before {
			t.Fatal("invalid attachment displaced prepared stream")
		}
	}
	q.Activate = true
	close(slot.abortDone)
	if err := r.applyCaptureAbort(t.Context(), waits, q, time.Now); err != nil {
		t.Fatal(err)
	}
	if err := r.applyCaptureAbort(t.Context(), waits, q, time.Now); err != nil {
		t.Fatal("lost activation reply:", err)
	}
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	a.AttachSequence++
	if err := r.parkCaptureAbortStream(waits, a, guest, time.Now()); err == nil {
		t.Fatal("released member was reattached")
	}
}

func TestCaptureAbortCleanupPrecedesHealthyRelease(t *testing.T) {
	r, waits, q := captureAbortFixture(t, 2)
	cancelled := r.programClaims[0]
	cancelled.done = make(chan struct{})
	stopped := make(chan struct{})
	cancelled.stop = func() { close(stopped) }
	q.Members[0].Cancelled = true
	q.Members[0].Authority = nil
	if err := r.applyCaptureAbort(t.Context(), waits, q, time.Now); err != nil {
		t.Fatal(err)
	}
	prepareAbortFixtureStreams(t, r, waits, q)
	q.Activate = true
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.applyCaptureAbort(ctx, waits, q, time.Now) }()
	<-stopped
	healthy := waits.slots[q.Members[1].Member.RunWaitId]
	select {
	case <-healthy.abortResume:
		t.Fatal("healthy released before cancelled cleanup")
	default:
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if r.captureAbort.activated {
		t.Fatal("timeout acknowledged activation")
	}
	close(cancelled.done)
	close(healthy.abortDone)
	if err := r.applyCaptureAbort(t.Context(), waits, q, time.Now); err != nil {
		t.Fatal(err)
	}
}

func TestRelayProgramCaptureAbortReplacesResetTransport(t *testing.T) {
	r, _, q := captureAbortFixture(t, 1)
	r.captureRequest = nil
	waits := newWaitingRunRegistry()
	blocked, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	defer release.Close()
	cmd := exec.Command("cat")
	cmd.Stdin = blocked
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlWriter.Close()
	decisionReader, decisionWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer decisionReader.Close()
	defer decisionWriter.Close()
	cgroup := &testProgramCgroup{}
	process := &programProcess{cmd: cmd, stdin: decisionWriter, control: controlReader, cgroup: cgroup, waitDone: make(chan struct{})}
	defer func() { release.Close(); controlWriter.Close(); process.close() }()
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	request := testProgramRunRequest(testProgramStartFrame(t))
	m := q.Members[0].Member
	request.RunId = m.RunId
	request.RunLeaseId = m.RunLeaseId
	request.AttemptNumber = m.AttemptNumber
	original := proto.Clone(request)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		var outputDone sync.WaitGroup
		result <- relayProgram(ctx, guest, request, &programv0.EntrypointIdentity{Kind: &programv0.EntrypointIdentity_Task{Task: &programv0.TaskEntrypoint{}}}, process, &programEventStream{conn: guest}, make(chan error, 2), &outputDone, waits, &programOutputCoordinator{})
	}()
	wait := &programv0.RunWaitRequested{CorrelationId: "correlation", RunWaitId: m.RunWaitId, ResumeAttachId: "attach", Kind: "token"}
	if err := frameio.WriteProtoFrame(controlWriter, &programv0.RunEvent{Event: &programv0.RunEvent_RunWaitRequested{RunWaitRequested: wait}}); err != nil {
		t.Fatal(err)
	}
	var forwarded programv0.RunEvent
	if err := frameio.ReadProtoFrame(host, &forwarded); err != nil {
		t.Fatal(err)
	}
	pause := &programv0.CheckpointPauseRequest{RunId: m.RunId, AttemptNumber: m.AttemptNumber, RunLeaseId: m.RunLeaseId, RunWaitId: m.RunWaitId, CorrelationId: wait.CorrelationId, ResumeAttachId: wait.ResumeAttachId, CheckpointId: q.Capture.CheckpointId, CheckpointRequestVersion: q.Capture.DesiredVersion}
	if err := wire.WriteCheckpointPauseRequest(host, pause); err != nil {
		t.Fatal(err)
	}
	h, _, err := wire.ReadStreamFrameHeader(host)
	if err != nil || h.Type != wire.StreamTypeCheckpointPauseReady {
		t.Fatalf("pause: %v %v", h, err)
	}
	// SnapshotCreate resets every existing vsock connection, including the source.
	host.Close()
	guest.Close()
	if err := r.applyCaptureAbort(ctx, waits, q, time.Now); err != nil {
		t.Fatal(err)
	}
	newGuest, newHost := net.Pipe()
	defer newGuest.Close()
	defer newHost.Close()
	attach := &computerv0.ComputerCaptureAbortAttachRequest{CheckpointId: q.Capture.CheckpointId, AbortDesiredVersion: q.AbortDesiredVersion, Member: m, Authority: q.Members[0].Authority, AttachSequence: 1}
	attached := make(chan error, 1)
	go func() {
		keep, err := handleComputerCaptureAbortAttach(ctx, newGuest, 0, r, waits)
		if !keep && err == nil {
			err = io.ErrClosedPipe
		}
		attached <- err
	}()
	if err := frameio.WriteProtoFrame(newHost, attach); err != nil {
		t.Fatal(err)
	}
	var ready computerv0.ComputerCaptureAbortAttachResponse
	if err := frameio.ReadProtoFrame(newHost, &ready); err != nil {
		t.Fatal(err)
	}
	if err := <-attached; err != nil {
		t.Fatal(err)
	}
	if cgroup.thawCount() != 0 {
		t.Fatal("preparation thawed Program")
	}
	q.Members[0].AttachSequence = ready.AttachSequence
	q.Activate = true
	if err := r.applyCaptureAbort(ctx, waits, q, time.Now); err != nil {
		t.Fatal(err)
	}
	decision := &programv0.ResumeDecision{RunWaitId: m.RunWaitId, CorrelationId: wait.CorrelationId, ResumeAttachId: wait.ResumeAttachId, Kind: "completed", NoResult: true}
	if err := wire.WriteResumeDecision(newHost, decision); err != nil {
		t.Fatal(err)
	}
	var actual programv0.ResumeDecision
	if err := frameio.ReadProtoFrame(decisionReader, &actual); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(decision, &actual) {
		t.Fatalf("unexpected Program control after abort: %v", &actual)
	}
	next := proto.Clone(wait).(*programv0.RunWaitRequested)
	next.RunWaitId = "second-wait"
	next.CorrelationId = "second-correlation"
	next.ResumeAttachId = "second-attach"
	if err := frameio.WriteProtoFrame(controlWriter, &programv0.RunEvent{Event: &programv0.RunEvent_RunWaitRequested{RunWaitRequested: next}}); err != nil {
		t.Fatal(err)
	}
	if err := frameio.ReadProtoFrame(newHost, &forwarded); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(forwarded.GetRunWaitRequested(), next) || !proto.Equal(request, original) || cgroup.thawCount() != 1 {
		t.Fatal("source identity/wait changed")
	}
	release.Close()
	controlWriter.Close()
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
