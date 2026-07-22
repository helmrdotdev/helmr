package guestd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

func TestProgramRuntimeHelper(t *testing.T) {
	if os.Getenv("HELMR_PROGRAM_HELPER") != "1" {
		return
	}
	control := os.NewFile(3, "program-control")
	if control == nil {
		os.Exit(2)
	}
	body, err := frameio.ReadMessageFrame(os.Stdin)
	if err != nil {
		os.Exit(3)
	}
	var framed bytes.Buffer
	if err := frameio.WriteMessageFrame(&framed, body); err != nil {
		os.Exit(4)
	}
	digest := sha256.Sum256(framed.Bytes())
	if hex.EncodeToString(digest[:]) != os.Getenv("HELMR_EXPECTED_FRAME_DIGEST") {
		os.Exit(5)
	}
	if err := frameio.WriteProtoFrame(control, &runv0.RunEvent{
		Event: &runv0.RunEvent_EntrypointReady{
			EntrypointReady: &runv0.EntrypointReady{
				RunId:         "run-1",
				AttemptNumber: 2,
				Entrypoint: &runv0.EntrypointIdentity{
					DeclaredId: "deploy",
					Kind: &runv0.EntrypointIdentity_Task{
						Task: &runv0.TaskEntrypoint{},
					},
				},
			},
		},
	}); err != nil {
		os.Exit(6)
	}
	var release runv0.EntrypointRelease
	if err := frameio.ReadProtoFrame(os.Stdin, &release); err != nil {
		os.Exit(7)
	}
	if release.GetRunId() != "run-1" ||
		release.GetAttemptNumber() != 2 ||
		release.GetEntrypoint().GetDeclaredId() != "deploy" ||
		release.GetEntrypoint().GetTask() == nil {
		os.Exit(8)
	}
	if err := frameio.WriteProtoFrame(control, &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskOutcome{
			TaskOutcome: &runv0.TaskOutcome{
				Outcome: &runv0.TaskOutcome_Succeeded{
					Succeeded: &runv0.TaskSucceeded{
						OutputJson: `{"ok":true}`,
					},
				},
			},
		},
	}); err != nil {
		os.Exit(9)
	}
	os.Exit(0)
}

func TestSuperviseProgramOrdersFreshEntrypointGates(t *testing.T) {
	frame := testProgramStartFrame(t)
	process := testProgramProcess(t, frame)
	defer process.close()
	request := testProgramRunRequest(frame)
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	result := make(chan error, 1)
	go func() {
		result <- superviseProgram(
			context.Background(),
			guest,
			request,
			process,
			newWaitingRunRegistry(),
			nil,
		)
	}()

	var event runv0.RunEvent
	if err := frameio.ReadProtoFrame(host, &event); err != nil {
		t.Fatal(err)
	}
	started := event.GetProgramProcessStarted()
	if started == nil ||
		started.GetRunId() != request.GetRunId() ||
		started.GetAttemptNumber() != request.GetAttemptNumber() ||
		started.GetRunLeaseId() != request.GetRunLeaseId() {
		t.Fatalf("process-started event = %#v", event.GetEvent())
	}
	if err := frameio.WriteProtoFrame(
		host,
		programStartReleaseCommand(request, request.GetRunLeaseId()),
	); err != nil {
		t.Fatal(err)
	}
	if err := frameio.ReadProtoFrame(host, &event); err != nil {
		t.Fatal(err)
	}
	ready := event.GetEntrypointReady()
	if ready == nil || ready.GetEntrypoint().GetDeclaredId() != "deploy" {
		t.Fatalf("entrypoint-ready event = %#v", event.GetEvent())
	}
	if err := frameio.WriteProtoFrame(
		host,
		entrypointReleaseCommand(request, ready.GetEntrypoint()),
	); err != nil {
		t.Fatal(err)
	}
	if err := frameio.ReadProtoFrame(host, &event); err != nil {
		t.Fatal(err)
	}
	outcome := event.GetTaskOutcome().GetSucceeded()
	if outcome == nil || outcome.GetOutputJson() != `{"ok":true}` {
		t.Fatalf("Task outcome = %#v", event.GetEvent())
	}
	if err := frameio.ReadProtoFrame(host, &event); err != nil {
		t.Fatal(err)
	}
	quiesced := event.GetProgramQuiesced()
	if quiesced == nil ||
		quiesced.GetRunId() != request.GetRunId() ||
		quiesced.GetAttemptNumber() != request.GetAttemptNumber() ||
		quiesced.GetRunLeaseId() != request.GetRunLeaseId() {
		t.Fatalf("Program quiescence proof = %#v", event.GetEvent())
	}
	cgroup := process.cgroup.(*testProgramCgroup)
	if cgroup.killCount() == 0 || cgroup.waitCount() == 0 {
		t.Fatalf("quiescence proof preceded cgroup cleanup: %+v", cgroup)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Program supervisor did not finish")
	}
}

func TestSuperviseProgramRejectsMismatchedStartRelease(t *testing.T) {
	frame := testProgramStartFrame(t)
	process := testProgramProcess(t, frame)
	defer process.close()
	request := testProgramRunRequest(frame)
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	result := make(chan error, 1)
	go func() {
		result <- superviseProgram(
			context.Background(),
			guest,
			request,
			process,
			newWaitingRunRegistry(),
			nil,
		)
	}()
	var event runv0.RunEvent
	if err := frameio.ReadProtoFrame(host, &event); err != nil {
		t.Fatal(err)
	}
	if event.GetProgramProcessStarted() == nil {
		t.Fatalf("first event = %#v", event.GetEvent())
	}
	if err := frameio.WriteProtoFrame(
		host,
		programStartReleaseCommand(request, "wrong"),
	); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "execution fence") {
			t.Fatalf("superviseProgram() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Program supervisor did not reject release")
	}
}

func TestSuperviseProgramRejectsWrongCommandArmBeforeStart(t *testing.T) {
	commands := map[string]*runv0.ProgramSupervisorCommand{
		"Secret completion": programSecretsCompleteCommand(
			testProgramRunRequest(testProgramStartFrame(t)),
		),
		"Secret delivery": programSecretDeliveryCommand(&runv0.ProgramSecret{
			Placement: &runv0.ProgramSecret_File{
				File: "/run/helmr-secrets/token",
			},
			Value: []byte("secret"),
		}),
	}
	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			frame := testProgramStartFrame(t)
			process := testProgramProcess(t, frame)
			defer process.close()
			request := testProgramRunRequest(frame)
			guest, host := net.Pipe()
			defer guest.Close()
			defer host.Close()
			result := make(chan error, 1)
			go func() {
				result <- superviseProgram(
					context.Background(),
					guest,
					request,
					process,
					newWaitingRunRegistry(),
					nil,
				)
			}()
			var event runv0.RunEvent
			if err := frameio.ReadProtoFrame(host, &event); err != nil {
				t.Fatal(err)
			}
			if event.GetProgramProcessStarted() == nil {
				t.Fatalf("first event = %#v", event.GetEvent())
			}
			if err := frameio.WriteProtoFrame(host, command); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if err == nil || !strings.Contains(err.Error(), "release command") {
					t.Fatalf("superviseProgram() error = %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Program supervisor accepted the wrong command arm")
			}
		})
	}
}

func TestRelayProgramPropagatesControlDecodeFailure(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var control bytes.Buffer
	if err := frameio.WriteMessageFrame(&control, []byte{0xff}); err != nil {
		t.Fatal(err)
	}
	process := &programProcess{
		cmd:      cmd,
		stdin:    nopWriteCloser{Writer: io.Discard},
		control:  io.NopCloser(&control),
		cgroup:   &testProgramCgroup{},
		waitDone: make(chan struct{}),
	}
	defer process.close()
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	var outputDone sync.WaitGroup
	err := relayProgram(
		context.Background(),
		guest,
		testProgramRunRequest(testProgramStartFrame(t)),
		&runv0.EntrypointIdentity{Kind: &runv0.EntrypointIdentity_Task{Task: &runv0.TaskEntrypoint{}}},
		process,
		&programEventStream{conn: guest},
		make(chan error, 2),
		&outputDone,
		newWaitingRunRegistry(),
		&programOutputCoordinator{},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "control event") {
		t.Fatalf("relayProgram() error = %v", err)
	}
}

func TestRelayProgramQuiescesDescendantHeldControlBeforeEOF(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := frameio.WriteProtoFrame(controlWriter, &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskOutcome{
			TaskOutcome: &runv0.TaskOutcome{
				Outcome: &runv0.TaskOutcome_Succeeded{
					Succeeded: &runv0.TaskSucceeded{OutputJson: "null"},
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	cgroup := &testProgramCgroup{onKill: controlWriter.Close}
	process := &programProcess{
		cmd:      cmd,
		stdin:    nopWriteCloser{Writer: io.Discard},
		control:  controlReader,
		cgroup:   cgroup,
		waitDone: make(chan struct{}),
	}
	defer process.close()
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	request := testProgramRunRequest(testProgramStartFrame(t))
	result := make(chan error, 1)
	go func() {
		var outputDone sync.WaitGroup
		result <- relayProgram(
			context.Background(),
			guest,
			request,
			&runv0.EntrypointIdentity{Kind: &runv0.EntrypointIdentity_Task{Task: &runv0.TaskEntrypoint{}}},
			process,
			&programEventStream{conn: guest},
			make(chan error, 2),
			&outputDone,
			newWaitingRunRegistry(),
			&programOutputCoordinator{},
			nil,
		)
	}()
	var event runv0.RunEvent
	if err := frameio.ReadProtoFrame(host, &event); err != nil {
		t.Fatal(err)
	}
	if event.GetTaskOutcome() == nil {
		t.Fatalf("first completion event = %#v", event.GetEvent())
	}
	if err := frameio.ReadProtoFrame(host, &event); err != nil {
		t.Fatal(err)
	}
	if event.GetProgramQuiesced() == nil {
		t.Fatalf("final completion event = %#v", event.GetEvent())
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if cgroup.killCount() == 0 || cgroup.waitCount() == 0 {
		t.Fatal("Program proof preceded cgroup quiescence")
	}
}

func TestPauseAndResumeProgramUsesExactFrozenAuthority(t *testing.T) {
	workspaceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspaceRoot, "state.txt"), []byte("durable"), 0o600); err != nil {
		t.Fatal(err)
	}
	cgroup := &testProgramCgroup{}
	var programInput bytes.Buffer
	process := &programProcess{
		stdin:         nopWriteCloser{Writer: &programInput},
		cgroup:        cgroup,
		workspaceRoot: workspaceRoot,
	}
	run := &runv0.ProgramRunRequest{RunId: "run-1", AttemptNumber: 2, RunLeaseId: "lease-1"}
	wait := &runv0.RunWaitRequested{CorrelationId: "wait-1", Kind: "timer"}
	pause := &runv0.CheckpointPauseRequest{
		RunId: "run-1", AttemptNumber: 2, RunLeaseId: "lease-1",
		RunWaitId: "durable-wait-1", CorrelationId: "wait-1", CheckpointId: "checkpoint-1",
		ResumeAttachId: "attach-1", CheckpointRequestVersion: 3,
		CaptureWorkspace: true,
	}
	registry := newWaitingRunRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	result := make(chan error, 1)
	events := make(chan *runv0.RunEvent, 1)
	controlErrors := make(chan error, 1)
	go func() {
		_, err := pauseAndResumeProgram(
			ctx, run, wait, pause, process,
			&programEventStream{conn: guest, changed: make(chan struct{}), done: make(chan struct{}), rebind: make(chan programConnection, 1)}, registry,
			&programOutputCoordinator{}, events, controlErrors,
		)
		result <- err
	}()
	reader := bufio.NewReader(host)
	header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
	if err != nil {
		t.Fatal(err)
	}
	if header.Type != wire.StreamTypeWorkspaceArtifact || bodyLen == 0 {
		t.Fatalf("workspace checkpoint frame = %+v body=%d", header, bodyLen)
	}
	if _, err := io.CopyN(io.Discard, reader, int64(bodyLen)); err != nil {
		t.Fatal(err)
	}
	header, bodyLen, err = wire.ReadStreamFrameHeader(reader)
	if err != nil {
		t.Fatal(err)
	}
	if header.Type != wire.StreamTypeCheckpointPauseReady || bodyLen == 0 {
		t.Fatalf("pause proof frame = %+v body=%d", header, bodyLen)
	}
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(reader, body); err != nil {
		t.Fatal(err)
	}
	var ready runv0.CheckpointPauseReady
	if err := proto.Unmarshal(body, &ready); err != nil {
		t.Fatal(err)
	}
	if ready.GetResumeAttachId() != "attach-1" || ready.GetCheckpointRequestVersion() != 3 {
		t.Fatalf("pause proof = %+v", ready)
	}
	if cgroup.freezeCount() != 1 || cgroup.thawCount() != 0 {
		t.Fatalf("cgroup before resume freeze=%d thaw=%d", cgroup.freezeCount(), cgroup.thawCount())
	}
	resumeGuest, resumeHost := net.Pipe()
	defer resumeGuest.Close()
	defer resumeHost.Close()
	attach := &runv0.ResumeAttach{
		RunId: "run-1", AttemptNumber: 2, RunLeaseId: "lease-2",
		RunWaitId: "durable-wait-1", CorrelationId: "wait-1", CheckpointId: "checkpoint-1",
		ResumeAttachId: "attach-1", ResumeRequestVersion: 4,
	}
	if err := registry.grantProgramResume(testProgramResumeGrant(attach)); err != nil {
		t.Fatal(err)
	}
	if err := registry.attachResume(attach, resumeGuest); err != nil {
		t.Fatal(err)
	}
	decision := &runv0.ResumeDecision{
		RunWaitId: "durable-wait-1", CorrelationId: "wait-1", Kind: "completed", NoResult: true, RequireConsumedAck: true,
		CheckpointId: "checkpoint-1", ResumeAttachId: "attach-1",
		ResumeRequestVersion: 4, RunLeaseId: "lease-2",
	}
	if err := frameio.WriteProtoFrame(resumeHost, decision); err != nil {
		t.Fatal(err)
	}
	events <- &runv0.RunEvent{Event: &runv0.RunEvent_ResumeConsumed{ResumeConsumed: &runv0.ResumeConsumed{
		RunWaitId: "durable-wait-1", CorrelationId: "wait-1", CheckpointId: "checkpoint-1",
		ResumeAttachId: "attach-1", ResumeRequestVersion: 4, RunLeaseId: "lease-2",
	}}}
	var ack runv0.ResumeAck
	if err := frameio.ReadProtoFrame(resumeHost, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.GetRunWaitId() != "durable-wait-1" || ack.GetCorrelationId() != "wait-1" || ack.GetResumeRequestVersion() != 4 ||
		ack.GetRunLeaseId() != "lease-2" || cgroup.thawCount() != 1 {
		t.Fatalf("resume proof=%+v thaw=%d", ack, cgroup.thawCount())
	}
	var staged runv0.ResumeDecision
	if err := frameio.ReadProtoFrame(bytes.NewReader(programInput.Bytes()), &staged); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&staged, decision) {
		t.Fatalf("staged decision = %+v", staged)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestValidateResumeDecisionAuthorityRejectsMalformedResultUnion(t *testing.T) {
	tests := []struct {
		name     string
		decision *runv0.ResumeDecision
		wantErr  bool
	}{
		{name: "completed no result", decision: &runv0.ResumeDecision{Kind: "completed", NoResult: true}},
		{name: "completed JSON null", decision: &runv0.ResumeDecision{Kind: "completed", DataJson: "null"}},
		{name: "completed both", decision: &runv0.ResumeDecision{Kind: "completed", NoResult: true, DataJson: "null"}, wantErr: true},
		{name: "completed neither", decision: &runv0.ResumeDecision{Kind: "completed"}, wantErr: true},
		{name: "failed reason", decision: &runv0.ResumeDecision{Kind: "failed", DataJson: `{"reason_code":"token_expired"}`}},
		{name: "failed no result", decision: &runv0.ResumeDecision{Kind: "failed", NoResult: true, DataJson: `{"reason_code":"token_expired"}`}, wantErr: true},
		{name: "failed missing reason", decision: &runv0.ResumeDecision{Kind: "failed", DataJson: `{}`}, wantErr: true},
		{name: "legacy fourth state", decision: &runv0.ResumeDecision{Kind: "timed_out", DataJson: "null"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateResumeDecisionAuthority(test.decision)
			if (err != nil) != test.wantErr {
				t.Fatalf("validate error = %v, want error %v", err, test.wantErr)
			}
		})
	}
}

func TestProgramOutputPauseTreatsClosedPipesAsAlreadyDrained(t *testing.T) {
	pump := &programOutputPump{
		pause: make(chan programOutputPause),
		done:  make(chan struct{}),
	}
	close(pump.done)
	resume, err := (&programOutputCoordinator{pumps: []*programOutputPump{pump}}).pause(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resume()
}

func TestProgramResumeReplayReturnsRecordedProofForExactReconnect(t *testing.T) {
	registry := newWaitingRunRegistry()
	pause := &runv0.CheckpointPauseRequest{
		RunId: "run-1", AttemptNumber: 2, RunLeaseId: "lease-1",
		RunWaitId: "durable-wait-1", CorrelationId: "correlation-1",
		CheckpointId: "checkpoint-1", ResumeAttachId: "attach-1", CheckpointRequestVersion: 3,
	}
	registration, err := registry.registerProgram(pause)
	if err != nil {
		t.Fatal(err)
	}
	attach := &runv0.ResumeAttach{
		RunId: "run-1", AttemptNumber: 2, RunLeaseId: "lease-2",
		RunWaitId: "durable-wait-1", CorrelationId: "correlation-1",
		CheckpointId: "checkpoint-1", ResumeAttachId: "attach-1", ResumeRequestVersion: 4,
	}
	if err := registry.grantProgramResume(testProgramResumeGrant(attach)); err != nil {
		t.Fatal(err)
	}
	initialGuest, initialHost := net.Pipe()
	if err := registry.attachResume(attach, initialGuest); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registration.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = initialGuest.Close()
	_ = initialHost.Close()
	decision := &runv0.ResumeDecision{
		RunWaitId: "durable-wait-1", CorrelationId: "correlation-1",
		CheckpointId: "checkpoint-1", ResumeAttachId: "attach-1", ResumeRequestVersion: 4,
		RunLeaseId: "lease-2", Kind: "completed", DataJson: "null", RequireConsumedAck: true,
	}
	ack := &runv0.ResumeAck{
		RunWaitId: "durable-wait-1", CorrelationId: "correlation-1",
		CheckpointId: "checkpoint-1", ResumeAttachId: "attach-1", ResumeRequestVersion: 4,
		RunLeaseId: "lease-2",
	}
	registration.markApplied(decision, ack)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldGuest, oldHost := net.Pipe()
	defer oldHost.Close()
	stream := &programEventStream{
		conn: oldGuest, changed: make(chan struct{}), done: make(chan struct{}),
		rebind: make(chan programConnection, 1),
	}
	stream.retainResumeReplay(ctx, registration, decision, ack)
	replayGuest, replayHost := net.Pipe()
	defer replayHost.Close()
	if err := registry.attachResume(proto.Clone(attach).(*runv0.ResumeAttach), replayGuest); err != nil {
		t.Fatal(err)
	}
	if err := frameio.WriteProtoFrame(replayHost, decision); err != nil {
		t.Fatal(err)
	}
	var replayed runv0.ResumeAck
	if err := frameio.ReadProtoFrame(replayHost, &replayed); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&replayed, ack) {
		t.Fatalf("replayed ack = %+v", &replayed)
	}
	select {
	case rebound := <-stream.rebind:
		if rebound != replayGuest {
			t.Fatal("replay connection was not adopted")
		}
	case <-time.After(time.Second):
		t.Fatal("replay connection was not rebound")
	}
	stream.closeCurrentConn()
}

func testProgramResumeGrant(attach *runv0.ResumeAttach) *programResumeGrant {
	return &programResumeGrant{
		attach: attach,
		lock:   func() {},
		unlock: func() {},
		valid:  func(time.Time) bool { return true },
	}
}

func TestValidateProgramCheckpointPauseRejectsMismatchedFence(t *testing.T) {
	run := &runv0.ProgramRunRequest{RunId: "run-1", AttemptNumber: 2, RunLeaseId: "lease-1"}
	wait := &runv0.RunWaitRequested{CorrelationId: "wait-1", Kind: "timer"}
	pause := &runv0.CheckpointPauseRequest{
		RunId: "run-1", AttemptNumber: 2, RunLeaseId: "lease-2",
		RunWaitId: "durable-wait-1", CorrelationId: "wait-1", CheckpointId: "checkpoint-1",
		ResumeAttachId: "attach-1", CheckpointRequestVersion: 3,
	}
	if err := validateProgramCheckpointPause(run, wait, pause); err == nil {
		t.Fatal("mismatched Run Lease fence was accepted")
	}
}

func TestValidateTaskOutcomeRejectsMalformedClosedShapes(t *testing.T) {
	oversizedMessage := strings.Repeat("x", maxTaskErrorMessageBytes+1)
	invalidDetails := "{"
	tests := []struct {
		name    string
		outcome *runv0.TaskOutcome
	}{
		{name: "missing", outcome: &runv0.TaskOutcome{}},
		{
			name: "missing output",
			outcome: &runv0.TaskOutcome{Outcome: &runv0.TaskOutcome_Succeeded{
				Succeeded: &runv0.TaskSucceeded{},
			}},
		},
		{
			name: "invalid output",
			outcome: &runv0.TaskOutcome{Outcome: &runv0.TaskOutcome_Succeeded{
				Succeeded: &runv0.TaskSucceeded{OutputJson: "{"},
			}},
		},
		{
			name: "ambiguous output",
			outcome: &runv0.TaskOutcome{Outcome: &runv0.TaskOutcome_Succeeded{
				Succeeded: &runv0.TaskSucceeded{OutputJson: `{"a":1,"a":2}`},
			}},
		},
		{
			name: "oversized message",
			outcome: &runv0.TaskOutcome{Outcome: &runv0.TaskOutcome_Failed{
				Failed: &runv0.TaskFailed{Message: oversizedMessage},
			}},
		},
		{
			name: "invalid details",
			outcome: &runv0.TaskOutcome{Outcome: &runv0.TaskOutcome_PayloadInvalid{
				PayloadInvalid: &runv0.TaskPayloadInvalid{
					Message:     "invalid",
					DetailsJson: &invalidDetails,
				},
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateTaskOutcome(test.outcome); err == nil {
				t.Fatal("validateTaskOutcome() error = nil")
			}
		})
	}
	if err := validateTaskOutcome(&runv0.TaskOutcome{
		Outcome: &runv0.TaskOutcome_Succeeded{
			Succeeded: &runv0.TaskSucceeded{OutputJson: "null"},
		},
	}); err != nil {
		t.Fatalf("valid JSON null rejected: %v", err)
	}
}

func TestValidateActorOutcomeRequiresCursorAndClosedVariant(t *testing.T) {
	zero := int64(0)
	negative := int64(-1)
	for _, outcome := range []*runv0.ActorOutcome{
		nil,
		{Outcome: &runv0.ActorOutcome_Succeeded{Succeeded: &runv0.ActorSucceeded{}}},
		{TerminalInputSequence: &negative, Outcome: &runv0.ActorOutcome_Succeeded{Succeeded: &runv0.ActorSucceeded{}}},
		{TerminalInputSequence: &zero},
		{TerminalInputSequence: &zero, Outcome: &runv0.ActorOutcome_Failed{Failed: &runv0.ActorFailed{}}},
	} {
		if err := validateActorOutcome(outcome); err == nil {
			t.Fatalf("validateActorOutcome(%v) error = nil", outcome)
		}
	}
	if err := validateActorOutcome(&runv0.ActorOutcome{
		TerminalInputSequence: &zero,
		Outcome:               &runv0.ActorOutcome_Succeeded{Succeeded: &runv0.ActorSucceeded{}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProgramEventStreamWriteDeadline(t *testing.T) {
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	stream := &programEventStream{
		conn:         guest,
		writeTimeout: 20 * time.Millisecond,
	}
	started := time.Now()
	result := make(chan error, 1)
	go func() {
		result <- stream.write(&runv0.RunEvent{
			Event: &runv0.RunEvent_StdoutChunk{
				StdoutChunk: []byte("blocked"),
			},
		})
	}()
	if err := guest.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("Program event write without a reader succeeded")
		}
		if time.Since(started) > time.Second {
			t.Fatalf("Program event write exceeded bounded deadline: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read deadline reset removed Program event write deadline")
	}
}

func TestProgramAdmissionDoesNotClaimBeforeSecretSequence(t *testing.T) {
	registry := newWorkspaceOperationRegistry()
	registry.register("mount-1", &workspaceMountEntry{
		workspaceID:       "workspace-1",
		baseVersionID:     "version-1",
		channelToken:      "channel-1",
		fencingGeneration: 1,
		runtimeInstanceID: "runtime-1",
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	result := make(chan error, 1)
	go func() {
		result <- handleProgramRunConnection(
			ctx,
			guest,
			nil,
			newWaitingRunRegistry(),
			registry,
			wire.StreamHeader{
				Type:             wire.StreamTypeProgramRun,
				RunID:            "run-1",
				WorkspaceID:      "workspace-1",
				WorkspaceMountID: "mount-1",
			},
			0,
		)
	}()
	if err := frameio.WriteProtoFrame(
		host,
		&workspacev0.WorkspaceRunAuthority{
			Fence: &workspacev0.WorkspaceAuthorityFence{
				WorkerInstanceId:       "worker-1",
				WorkerEpoch:            1,
				RuntimeInstanceId:      "runtime-1",
				RuntimeIdentityId:      "runtime-identity-1",
				WorkspaceId:            "workspace-1",
				WorkspaceMountId:       "mount-1",
				RunId:                  "run-1",
				AttemptNumber:          2,
				RunLeaseId:             "lease-1",
				LeaseSequence:          1,
				WorkspaceLeaseId:       "workspace-lease-1",
				OwnershipGeneration:    1,
				WriterGeneration:       1,
				MountFencingGeneration: 1,
				ExpiresAtUnixNano:      time.Now().Add(time.Minute).UnixNano(),
				BaseWorkspaceVersionId: "version-1",
			},
			ChannelToken:    "channel-1",
			WriteCapability: "write-capability",
		},
	); err != nil {
		t.Fatal(err)
	}
	request := testProgramRunRequest(testProgramStartFrame(t))
	request.SecretCount = 1
	request.StartDeadlineUnixMs = time.Now().Add(time.Minute).UnixMilli()
	if err := frameio.WriteProtoFrame(host, request); err != nil {
		t.Fatal(err)
	}
	if _, err := host.Write(frameHeader(8)); err != nil {
		t.Fatal(err)
	}
	registry.mu.RLock()
	active := registry.programActive
	registry.mu.RUnlock()
	if active {
		t.Fatal("partial Secret sequence claimed the Program slot")
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("partial Secret sequence succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("partial Secret sequence did not unblock on cancellation")
	}
}

func TestValidateProgramRunRequestRequiresOneBoundedFrame(t *testing.T) {
	valid := testProgramRunRequest(testProgramStartFrame(t))
	if err := validateProgramRunRequest(valid); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		frame []byte
	}{
		{name: "truncated", frame: []byte{0, 0, 0}},
		{name: "trailing", frame: append(testProgramStartFrame(t), 0)},
		{name: "oversized", frame: frameHeader(frameio.MaxFrameBytes + 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := testProgramRunRequest(test.frame)
			if err := validateProgramRunRequest(request); err == nil {
				t.Fatal("validateProgramRunRequest() error = nil")
			}
		})
	}
	valid.StartDeadlineUnixMs = time.Now().Add(-time.Second).UnixMilli()
	if err := validateProgramRunRequest(valid); err == nil {
		t.Fatal("expired start deadline was accepted")
	}
}

func TestValidateProgramRunRequestRejectsExcessSecretPlacements(t *testing.T) {
	request := testProgramRunRequest(testProgramStartFrame(t))
	request.SecretCount = maxProgramSecretPlacements + 1
	if err := validateProgramRunRequest(request); err == nil ||
		!strings.Contains(err.Error(), "secret_count") {
		t.Fatalf("validate Program request error = %v", err)
	}
}

func TestValidateProgramSecretsRequiresCanonicalNonConflictingPlacements(t *testing.T) {
	valid := []*runv0.ProgramSecret{
		{
			Placement: &runv0.ProgramSecret_Env{Env: "API_TOKEN"},
			Value:     []byte("value"),
		},
		{
			Placement: &runv0.ProgramSecret_File{
				File: "/run/helmr-secrets/config.json",
			},
			Value: []byte("{}"),
		},
	}
	if err := validateProgramSecrets(valid); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		secrets []*runv0.ProgramSecret
	}{
		{
			name: "noncanonical",
			secrets: []*runv0.ProgramSecret{
				valid[1],
				valid[0],
			},
		},
		{
			name: "duplicate env",
			secrets: []*runv0.ProgramSecret{
				valid[0],
				{
					Placement: &runv0.ProgramSecret_Env{Env: "API_TOKEN"},
					Value:     []byte("other"),
				},
			},
		},
		{
			name: "durable file",
			secrets: []*runv0.ProgramSecret{
				{
					Placement: &runv0.ProgramSecret_File{
						File: "/workspace/token",
					},
					Value: []byte("value"),
				},
			},
		},
		{
			name: "nul value",
			secrets: []*runv0.ProgramSecret{
				{
					Placement: &runv0.ProgramSecret_Env{Env: "TOKEN"},
					Value:     []byte("sensitive\x00value"),
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateProgramSecrets(test.secrets)
			if err == nil {
				t.Fatal("validateProgramSecrets() error = nil")
			}
			if strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("Secret value leaked in error: %v", err)
			}
		})
	}
}

func TestReadProgramSecretsRequiresExactFencedSequence(t *testing.T) {
	request := testProgramRunRequest(testProgramStartFrame(t))
	request.SecretCount = 2
	secrets := []*runv0.ProgramSecret{
		{
			Placement: &runv0.ProgramSecret_Env{Env: "API_TOKEN"},
			Value:     []byte("secret-one"),
		},
		{
			Placement: &runv0.ProgramSecret_File{
				File: "/run/helmr-secrets/config.json",
			},
			Value: []byte("secret-two"),
		},
	}
	var input bytes.Buffer
	for _, secret := range secrets {
		if err := frameio.WriteProtoFrame(
			&input,
			programSecretDeliveryCommand(secret),
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := frameio.WriteProtoFrame(
		&input,
		programSecretsCompleteCommand(request),
	); err != nil {
		t.Fatal(err)
	}
	got, err := readProgramSecrets(&input, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(secrets) ||
		!bytes.Equal(got[0].GetValue(), secrets[0].GetValue()) ||
		!bytes.Equal(got[1].GetValue(), secrets[1].GetValue()) {
		t.Fatalf("Secret delivery = %#v", got)
	}

	var trailing bytes.Buffer
	for _, secret := range append(secrets, secrets[1]) {
		if err := frameio.WriteProtoFrame(
			&trailing,
			programSecretDeliveryCommand(secret),
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := frameio.WriteProtoFrame(
		&trailing,
		&runv0.ProgramSupervisorCommand{},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := readProgramSecrets(&trailing, request); err == nil {
		t.Fatal("additional Secret delivery was accepted")
	} else if strings.Contains(err.Error(), "secret-two") {
		t.Fatalf("Secret value leaked in error: %v", err)
	}
}

func TestReadProgramSecretsRejectsOversizedDeliveryBeforeAllocation(t *testing.T) {
	request := testProgramRunRequest(testProgramStartFrame(t))
	request.SecretCount = 1
	var input bytes.Buffer
	if err := binary.Write(
		&input,
		binary.BigEndian,
		uint32(maxProgramSecretFrameBytes+1),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := readProgramSecrets(&input, request); err == nil ||
		!strings.Contains(err.Error(), "exceeds max") {
		t.Fatalf("read Program Secrets error = %v", err)
	}
}

func programSecretDeliveryCommand(
	secret *runv0.ProgramSecret,
) *runv0.ProgramSupervisorCommand {
	return &runv0.ProgramSupervisorCommand{
		Command: &runv0.ProgramSupervisorCommand_SecretDelivery{
			SecretDelivery: secret,
		},
	}
}

func programSecretsCompleteCommand(
	request *runv0.ProgramRunRequest,
) *runv0.ProgramSupervisorCommand {
	return &runv0.ProgramSupervisorCommand{
		Command: &runv0.ProgramSupervisorCommand_SecretsComplete{
			SecretsComplete: &runv0.ProgramSecretsComplete{
				RunId:         request.GetRunId(),
				AttemptNumber: request.GetAttemptNumber(),
				RunLeaseId:    request.GetRunLeaseId(),
				SecretCount:   request.GetSecretCount(),
			},
		},
	}
}

func programStartReleaseCommand(
	request *runv0.ProgramRunRequest,
	leaseID string,
) *runv0.ProgramSupervisorCommand {
	return &runv0.ProgramSupervisorCommand{
		Command: &runv0.ProgramSupervisorCommand_StartRelease{
			StartRelease: &runv0.ProgramStartRelease{
				RunId:         request.GetRunId(),
				AttemptNumber: request.GetAttemptNumber(),
				RunLeaseId:    leaseID,
			},
		},
	}
}

func entrypointReleaseCommand(
	request *runv0.ProgramRunRequest,
	identity *runv0.EntrypointIdentity,
) *runv0.ProgramSupervisorCommand {
	return &runv0.ProgramSupervisorCommand{
		Command: &runv0.ProgramSupervisorCommand_EntrypointRelease{
			EntrypointRelease: &runv0.EntrypointRelease{
				RunId:         request.GetRunId(),
				AttemptNumber: request.GetAttemptNumber(),
				Entrypoint:    identity,
			},
		},
	}
}

func TestPrepareProgramSecretTargetCleansOnlyCreatedImagePaths(t *testing.T) {
	imageRoot := t.TempDir()
	cleanup, err := prepareProgramSecretTarget(
		imageRoot,
		"/run/helmr-secrets/token",
	)
	if err != nil {
		t.Fatal(err)
	}
	target := imageRoot + "/run/helmr-secrets/token"
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("created target survived cleanup: %v", err)
	}

	existing := imageRoot + "/etc/config"
	if err := os.MkdirAll(imageRoot+"/etc", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(existing, []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanup, err = prepareProgramSecretTarget(imageRoot, "/etc/config")
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	body, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "image" {
		t.Fatalf("existing target body = %q", body)
	}
}

func TestPrepareProgramSecretTargetRejectsSymlinkParent(t *testing.T) {
	imageRoot := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, imageRoot+"/etc"); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareProgramSecretTarget(
		imageRoot,
		"/etc/token",
	); err == nil {
		t.Fatal("symlink parent was accepted")
	}
	if _, err := os.Stat(outside + "/token"); !os.IsNotExist(err) {
		t.Fatalf("Secret target escaped image root: %v", err)
	}
}

func testProgramProcess(t *testing.T, frame []byte) *programProcess {
	t.Helper()
	digest := sha256.Sum256(frame)
	cmd := exec.Command(os.Args[0], "-test.run=TestProgramRuntimeHelper")
	cmd.Env = append(
		os.Environ(),
		"HELMR_PROGRAM_HELPER=1",
		"HELMR_EXPECTED_FRAME_DIGEST="+hex.EncodeToString(digest[:]),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.ExtraFiles = []*os.File{controlWriter}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	return &programProcess{
		cmd:           cmd,
		stdin:         stdin,
		stdout:        stdout,
		stderr:        stderr,
		control:       controlReader,
		controlWriter: controlWriter,
		cgroup:        &testProgramCgroup{},
		waitDone:      make(chan struct{}),
	}
}

type testProgramCgroup struct {
	mu      sync.Mutex
	freezes int
	thaws   int
	kills   int
	waits   int
	onKill  func() error
}

func (*testProgramCgroup) attach(*exec.Cmd) error { return nil }
func (c *testProgramCgroup) freeze(context.Context) error {
	c.mu.Lock()
	c.freezes++
	c.mu.Unlock()
	return nil
}
func (c *testProgramCgroup) thaw(context.Context) error {
	c.mu.Lock()
	c.thaws++
	c.mu.Unlock()
	return nil
}
func (c *testProgramCgroup) kill() error {
	c.mu.Lock()
	c.kills++
	onKill := c.onKill
	c.onKill = nil
	c.mu.Unlock()
	if onKill != nil {
		return onKill()
	}
	return nil
}
func (c *testProgramCgroup) waitEmpty() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits++
	return nil
}
func (*testProgramCgroup) close() error { return nil }

func (c *testProgramCgroup) killCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.kills
}

func (c *testProgramCgroup) waitCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waits
}

func (c *testProgramCgroup) freezeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.freezes
}

func (c *testProgramCgroup) thawCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.thaws
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error {
	return nil
}

func testProgramRunRequest(frame []byte) *runv0.ProgramRunRequest {
	return &runv0.ProgramRunRequest{
		RunId:               "run-1",
		AttemptNumber:       2,
		RunLeaseId:          "lease-1",
		ProgramStartFrame:   append([]byte(nil), frame...),
		StartDeadlineUnixMs: time.Now().Add(5 * time.Second).UnixMilli(),
	}
}

func testProgramStartFrame(t *testing.T) []byte {
	t.Helper()
	var frame bytes.Buffer
	if err := frameio.WriteProtoFrame(&frame, &runv0.ProgramStart{
		RunId:                  "run-1",
		AttemptNumber:          2,
		EntrypointDeclaredId:   "deploy",
		DeploymentId:           "deployment-1",
		DeploymentVersion:      "v0",
		WorkspaceId:            "workspace-1",
		BaseWorkspaceVersionId: "version-1",
		Cause:                  &runv0.RunCause{Kind: &runv0.RunCause_Api{Api: &runv0.ApiCause{}}},
		Entrypoint:             &runv0.ProgramStart_Task{Task: &runv0.TaskStart{Payload: &runv0.TaskStart_NoPayload{NoPayload: &runv0.NoPayload{}}}},
	}); err != nil {
		t.Fatal(err)
	}
	return frame.Bytes()
}

func frameHeader(size uint32) []byte {
	frame := make([]byte, 4)
	binary.BigEndian.PutUint32(frame, size)
	return frame
}
