package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestFreshProgramOrdersAdmissionEntrypointAndTaskCompletion(t *testing.T) {
	claim := testFreshProgramClaim(t)
	claim.Lease.ExpiresAt = time.Now().Add(5 * time.Second).UTC()
	controlPlane := &testFreshProgramControlPlane{
		lease:              claim.Lease,
		startFailures:      1,
		entrypointFailures: 1,
	}
	events := &testFreshProgramEventSink{}
	guest, host := net.Pipe()
	defer guest.Close()
	sessions := NewWorkspaceMountSessions()
	unregister := sessions.RegisterWorkspaceMountSession(
		testWorkspaceMount(claim.Lease),
		fakeGuestSession{stream: host},
		"channel-1",
	)
	defer unregister()
	guestResult := make(chan error, 1)
	go func() {
		guestResult <- serveFreshProgramProtocol(
			guest,
			claim.Lease,
			controlPlane,
		)
	}()
	program, err := (ProgramRunner{
		WorkspaceMounts: sessions,
	}).startNewProgram(
		context.Background(),
		&claim,
		controlPlane,
		events,
	)
	if err != nil {
		t.Fatal(err)
	}
	if program.lease != controlPlane.lease ||
		program.entrypoint.GetDeclaredId() != "deploy" ||
		program.entrypoint.GetTask() == nil ||
		program.observedEventSeq != 4 {
		t.Fatalf("fresh Program = %+v", program)
	}
	outcome, quiesced, err := program.awaitTaskCompletion(context.Background(), events, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.GetSucceeded().GetOutputJson() != `{"ok":true}` {
		t.Fatalf("Task outcome = %+v", outcome)
	}
	if quiesced.GetRunLeaseId() != claim.Lease.ID {
		t.Fatalf("Program quiescence proof = %+v", quiesced)
	}
	if program.observedEventSeq != 7 {
		t.Fatalf("observed event sequence = %d", program.observedEventSeq)
	}
	if err := <-guestResult; err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(controlPlane.snapshot(), []string{
		"start", "start", "entrypoint", "entrypoint",
	}) {
		t.Fatalf("Control Plane calls = %v", controlPlane.snapshot())
	}
	if controlPlane.renewalCount() != 1 {
		t.Fatalf("Run Lease renewals = %d", controlPlane.renewalCount())
	}
	if !slices.Equal(events.snapshot(), []testFreshProgramLog{
		{stream: workerapi.LogStreamStdout, observedSeq: 2, content: "loading"},
		{stream: workerapi.LogStreamStderr, observedSeq: 3, content: "notice"},
		{stream: workerapi.LogStreamStdout, observedSeq: 6, content: "done"},
	}) {
		t.Fatalf("Run logs = %+v", events.snapshot())
	}
	if claim.Execution.Fresh.ProgramStart != nil ||
		claim.Workspace.WriteCapability != "" {
		t.Fatalf("claim delivery authority was retained: %+v", claim)
	}
	for _, secret := range claim.Secrets {
		if secret.Value != nil {
			t.Fatalf("Secret value was retained: %+v", secret)
		}
	}
}

func TestChildAttachStartsNewProgramOnRetainedMount(t *testing.T) {
	claim := testChildAttachProgramClaim(t)
	controlPlane := &testFreshProgramControlPlane{
		lease:     claim.Lease,
		wantChild: true,
	}
	guest, host := net.Pipe()
	defer guest.Close()
	verifyGuest, verifyHost := net.Pipe()
	defer verifyGuest.Close()
	parent := &queuedStreamSession{
		streams: []vm.Stream{
			testVMStream(host),
			testVMStream(verifyHost),
		},
	}
	sessions := NewWorkspaceMountSessions()
	unregister := sessions.RegisterWorkspaceMountSession(
		testWorkspaceMount(claim.Lease),
		parent,
		"channel-1",
	)
	defer unregister()
	guestResult := make(chan error, 1)
	go func() {
		guestResult <- serveFreshProgramProtocol(
			guest,
			claim.Lease,
			controlPlane,
		)
	}()
	verifyResult := make(chan error, 1)
	go func() {
		verifyResult <- serveFrozenParentVerification(verifyGuest)
	}()
	program, err := (ProgramRunner{
		WorkspaceMounts: sessions,
	}).startNewProgram(
		context.Background(),
		&claim,
		controlPlane,
		&testFreshProgramEventSink{},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer program.session.Close(context.Background())
	if _, _, err := program.awaitTaskCompletion(
		context.Background(),
		&testFreshProgramEventSink{},
		nil,
		nil,
		nil,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if err := <-guestResult; err != nil {
		t.Fatal(err)
	}
	if err := <-verifyResult; err != nil {
		t.Fatal(err)
	}
	if claim.Execution.Attach.Child.ProgramStart != nil {
		t.Fatal("child Program start authority was retained")
	}
}

func TestChildAttachRejectsMismatchedFrozenParentProof(t *testing.T) {
	claim := testChildAttachProgramClaim(t)
	programGuest, programHost := net.Pipe()
	defer programGuest.Close()
	verifyGuest, verifyHost := net.Pipe()
	defer verifyGuest.Close()
	parent := &queuedStreamSession{
		streams: []vm.Stream{
			testVMStream(programHost),
			testVMStream(verifyHost),
		},
	}
	sessions := NewWorkspaceMountSessions()
	unregister := sessions.RegisterWorkspaceMountSession(
		testWorkspaceMount(claim.Lease),
		parent,
		"channel-1",
	)
	defer unregister()
	verifyResult := make(chan error, 1)
	go func() {
		defer verifyGuest.Close()
		if _, _, err := wire.ReadStreamFrameHeader(verifyGuest); err != nil {
			verifyResult <- err
			return
		}
		var request workspacev0.VerifyProgramRestoreRequest
		if err := frameio.ReadProtoFrame(verifyGuest, &request); err != nil {
			verifyResult <- err
			return
		}
		request.CorrelationId = "wrong-correlation"
		verifyResult <- frameio.WriteProtoFrame(
			verifyGuest,
			&workspacev0.VerifyProgramRestoreResponse{
				RunId:         request.GetRunId(),
				AttemptNumber: request.GetAttemptNumber(),
				RunWaitId:     request.GetRunWaitId(),
				CheckpointId:  request.GetCheckpointId(),
				CorrelationId: request.GetCorrelationId(),
			},
		)
	}()
	controlPlane := &testFreshProgramControlPlane{
		lease:     claim.Lease,
		wantChild: true,
	}
	_, err := (ProgramRunner{
		WorkspaceMounts: sessions,
	}).startNewProgram(
		context.Background(),
		&claim,
		controlPlane,
		&testFreshProgramEventSink{},
	)
	if err == nil || !strings.Contains(err.Error(), "verification response changed") {
		t.Fatalf("startNewProgram() error = %v", err)
	}
	if err := <-verifyResult; err != nil {
		t.Fatal(err)
	}
	if len(controlPlane.snapshot()) != 0 {
		t.Fatalf("Control Plane calls = %v", controlPlane.snapshot())
	}
}

func serveFrozenParentVerification(conn net.Conn) error {
	defer conn.Close()
	header, bodyLen, err := wire.ReadStreamFrameHeader(conn)
	if err != nil {
		return err
	}
	if header.Type != wire.StreamTypeProgramRestoreVerify ||
		header.RunID != "parent-run-1" ||
		header.RunWaitID != "wait-1" ||
		header.CheckpointID != "checkpoint-1" ||
		bodyLen != 0 {
		return fmt.Errorf("parent verification header = %+v body=%d", header, bodyLen)
	}
	var request workspacev0.VerifyProgramRestoreRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return err
	}
	if request.GetRunId() != "parent-run-1" ||
		request.GetAttemptNumber() != 3 ||
		request.GetRunWaitId() != "wait-1" ||
		request.GetCheckpointId() != "checkpoint-1" ||
		request.GetCorrelationId() != "correlation-1" {
		return fmt.Errorf("parent verification request = %+v", &request)
	}
	return frameio.WriteProtoFrame(
		conn,
		&workspacev0.VerifyProgramRestoreResponse{
			RunId:         request.GetRunId(),
			AttemptNumber: request.GetAttemptNumber(),
			RunWaitId:     request.GetRunWaitId(),
			CheckpointId:  request.GetCheckpointId(),
			CorrelationId: request.GetCorrelationId(),
		},
	)
}

func TestStartFreshProgramDoesNotReleaseAfterStartRejection(t *testing.T) {
	claim := testFreshProgramClaim(t)
	controlPlane := &testFreshProgramControlPlane{
		lease: claim.Lease,
		startErr: &httpclient.Error{
			StatusCode: http.StatusConflict,
			Status:     "Conflict",
			Message:    "stale start",
		},
	}
	guest, host := net.Pipe()
	defer guest.Close()
	sessions := NewWorkspaceMountSessions()
	unregister := sessions.RegisterWorkspaceMountSession(
		testWorkspaceMount(claim.Lease),
		fakeGuestSession{stream: host},
		"channel-1",
	)
	defer unregister()
	proofSent := make(chan error, 1)
	go func() {
		if err := readFreshProgramAdmission(guest, claim.Lease); err != nil {
			proofSent <- err
			return
		}
		proofSent <- frameio.WriteProtoFrame(guest, &runv0.RunEvent{
			Event: &runv0.RunEvent_ProgramProcessStarted{
				ProgramProcessStarted: &runv0.ProgramProcessStarted{
					RunId:         claim.Lease.RunID,
					AttemptNumber: uint32(claim.Lease.AttemptNumber),
					RunLeaseId:    claim.Lease.ID,
				},
			},
		})
	}()
	_, err := (ProgramRunner{
		WorkspaceMounts: sessions,
	}).startNewProgram(
		context.Background(),
		&claim,
		controlPlane,
		&testFreshProgramEventSink{},
	)
	if err == nil || !strings.Contains(err.Error(), "stale start") {
		t.Fatalf("startNewProgram() error = %v", err)
	}
	if err := <-proofSent; err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(controlPlane.snapshot(), []string{"start"}) {
		t.Fatalf("Control Plane calls = %v", controlPlane.snapshot())
	}
	if claim.Execution.Fresh.ProgramStart != nil ||
		claim.Workspace.WriteCapability != "" {
		t.Fatal("rejected claim delivery authority was retained")
	}
}

func TestStartFreshProgramStopsBlockedAdmissionAtStartDeadline(t *testing.T) {
	claim := testFreshProgramClaim(t)
	claim.Lease.StartDeadlineAt = time.Now().Add(50 * time.Millisecond).UTC()
	guest, host := net.Pipe()
	defer guest.Close()
	sessions := NewWorkspaceMountSessions()
	unregister := sessions.RegisterWorkspaceMountSession(
		testWorkspaceMount(claim.Lease),
		fakeGuestSession{stream: host},
		"channel-1",
	)
	defer unregister()
	started := time.Now()
	_, err := (ProgramRunner{
		WorkspaceMounts: sessions,
	}).startNewProgram(
		context.Background(),
		&claim,
		&testFreshProgramControlPlane{lease: claim.Lease},
		&testFreshProgramEventSink{},
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startNewProgram() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked admission stopped after %s", elapsed)
	}
	if claim.Execution.Fresh.ProgramStart != nil ||
		claim.Workspace.WriteCapability != "" {
		t.Fatal("timed-out claim delivery authority was retained")
	}
}

func TestValidateFreshProgramClaimRejectsSecretAggregateAboveBound(t *testing.T) {
	claim := testFreshProgramClaim(t)
	shared := make([]byte, maxFreshProgramSecretBytes/2+1)
	claim.Secrets[0].Value = shared
	claim.Secrets[1].Value = shared
	_, err := validateNewProgramClaim(&claim)
	if err == nil || !strings.Contains(err.Error(), "plaintext exceeds") {
		t.Fatalf("validateNewProgramClaim() error = %v", err)
	}
}

func TestAwaitTaskCompletionRequiresFinalMatchingQuiescenceProof(t *testing.T) {
	tests := []struct {
		name   string
		events []*runv0.RunEvent
		want   string
	}{
		{
			name: "missing proof",
			events: []*runv0.RunEvent{
				testTaskSucceededEvent(`null`),
			},
			want: "EOF",
		},
		{
			name: "proof before outcome",
			events: []*runv0.RunEvent{
				testProgramQuiescedEvent(workerapi.RunLeaseAssignment{
					ID: "lease-1", RunID: "run-1", AttemptNumber: 2,
				}),
			},
			want: "before emitting",
		},
		{
			name: "mismatched proof",
			events: []*runv0.RunEvent{
				testTaskSucceededEvent(`null`),
				testProgramQuiescedEvent(workerapi.RunLeaseAssignment{
					ID: "other", RunID: "run-1", AttemptNumber: 2,
				}),
			},
			want: "does not match",
		},
		{
			name: "duplicate outcome",
			events: []*runv0.RunEvent{
				testTaskSucceededEvent(`null`),
				testTaskSucceededEvent(`null`),
			},
			want: "more than one",
		},
		{
			name: "malformed output",
			events: []*runv0.RunEvent{
				testTaskSucceededEvent(`{`),
			},
			want: "not unambiguous JSON",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			guest, host := net.Pipe()
			go func() {
				defer guest.Close()
				for _, event := range test.events {
					if err := frameio.WriteProtoFrame(guest, event); err != nil {
						return
					}
				}
			}()
			program := freshProgram{
				session: fakeGuestSession{stream: host},
				lease: workerapi.RunLeaseAssignment{
					ID: "lease-1", RunID: "run-1", AttemptNumber: 2,
				},
				entrypoint: &runv0.EntrypointIdentity{
					DeclaredId: "deploy",
					Kind: &runv0.EntrypointIdentity_Task{
						Task: &runv0.TaskEntrypoint{},
					},
				},
			}
			_, _, err := program.awaitTaskCompletion(
				context.Background(),
				&testFreshProgramEventSink{},
				nil,
				nil,
				nil,
				nil,
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("awaitTaskCompletion() error = %v", err)
			}
		})
	}
}

func TestFreshProgramDispatchesActorInputSendForTaskAndActor(t *testing.T) {
	lease := workerapi.RunLeaseAssignment{
		ID: "lease-1", RunID: "run-1", AttemptNumber: 2,
	}
	send := &runv0.ActorInputSendRequested{
		CorrelationId: "019c10d5-a6f7-7af1-8f5f-000000000111",
		DeclaredId:    "mailbox",
		Address: &runv0.ActorInputSendRequested_ActorKey{
			ActorKey: "primary",
		},
		DataJson: `{"message":"hello"}`,
	}
	tests := []struct {
		name       string
		entrypoint *runv0.EntrypointIdentity
		outcome    *runv0.RunEvent
		await      func(*freshProgram, freshProgramEventSink, func(context.Context, *runv0.ActorInputSendRequested) error) error
	}{
		{
			name: "Task",
			entrypoint: &runv0.EntrypointIdentity{
				Kind: &runv0.EntrypointIdentity_Task{Task: &runv0.TaskEntrypoint{}},
			},
			outcome: testTaskSucceededEvent(`null`),
			await: func(program *freshProgram, events freshProgramEventSink, callback func(context.Context, *runv0.ActorInputSendRequested) error) error {
				_, _, err := program.awaitTaskCompletion(t.Context(), events, nil, callback, nil, nil)
				return err
			},
		},
		{
			name: "Actor",
			entrypoint: &runv0.EntrypointIdentity{
				Kind: &runv0.EntrypointIdentity_Actor{Actor: &runv0.ActorEntrypoint{}},
			},
			outcome: &runv0.RunEvent{
				Event: &runv0.RunEvent_ActorOutcome{
					ActorOutcome: &runv0.ActorOutcome{
						TerminalInputSequence: new(int64(0)),
						Outcome: &runv0.ActorOutcome_Succeeded{
							Succeeded: &runv0.ActorSucceeded{},
						},
					},
				},
			},
			await: func(program *freshProgram, events freshProgramEventSink, callback func(context.Context, *runv0.ActorInputSendRequested) error) error {
				_, _, err := program.awaitActorCompletion(t.Context(), events, nil, nil, callback, nil, nil, nil)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			guest, host := net.Pipe()
			go func() {
				defer guest.Close()
				_ = frameio.WriteProtoFrame(guest, &runv0.RunEvent{
					Event: &runv0.RunEvent_ActorInputSendRequested{
						ActorInputSendRequested: send,
					},
				})
				_ = frameio.WriteProtoFrame(guest, test.outcome)
				_ = frameio.WriteProtoFrame(guest, testProgramQuiescedEvent(lease))
			}()
			program := &freshProgram{
				session:    fakeGuestSession{stream: host},
				lease:      lease,
				entrypoint: test.entrypoint,
			}
			var observed *runv0.ActorInputSendRequested
			err := test.await(
				program,
				&testFreshProgramEventSink{},
				func(_ context.Context, requested *runv0.ActorInputSendRequested) error {
					observed = requested
					return nil
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			if observed.GetCorrelationId() != send.GetCorrelationId() ||
				observed.GetActorKey() != send.GetActorKey() ||
				observed.GetDataJson() != send.GetDataJson() {
				t.Fatalf("Actor input send = %+v", observed)
			}
		})
	}
}

func TestFreshProgramDispatchesActorOutputAppend(t *testing.T) {
	lease := workerapi.RunLeaseAssignment{
		ID: "lease-1", RunID: "run-1", AttemptNumber: 2,
	}
	requested := &runv0.ActorOutputAppendRequested{
		CorrelationId: "019c10d5-a6f7-7af1-8f5f-000000000112",
		DataJson:      `{"status":"working"}`,
		ContentType:   "application/json",
	}
	guest, host := net.Pipe()
	go func() {
		defer guest.Close()
		_ = frameio.WriteProtoFrame(guest, &runv0.RunEvent{
			Event: &runv0.RunEvent_ActorOutputAppendRequested{
				ActorOutputAppendRequested: requested,
			},
		})
		_ = frameio.WriteProtoFrame(guest, &runv0.RunEvent{
			Event: &runv0.RunEvent_ActorOutcome{
				ActorOutcome: &runv0.ActorOutcome{
					TerminalInputSequence: new(int64(0)),
					Outcome: &runv0.ActorOutcome_Succeeded{
						Succeeded: &runv0.ActorSucceeded{},
					},
				},
			},
		})
		_ = frameio.WriteProtoFrame(guest, testProgramQuiescedEvent(lease))
	}()
	program := &freshProgram{
		session: fakeGuestSession{stream: host},
		lease:   lease,
		entrypoint: &runv0.EntrypointIdentity{
			Kind: &runv0.EntrypointIdentity_Actor{Actor: &runv0.ActorEntrypoint{}},
		},
	}
	var observed *runv0.ActorOutputAppendRequested
	_, _, err := program.awaitActorCompletion(
		t.Context(),
		&testFreshProgramEventSink{},
		nil,
		nil,
		nil,
		func(_ context.Context, value *runv0.ActorOutputAppendRequested) error {
			observed = value
			return nil
		},
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if observed.GetCorrelationId() != requested.GetCorrelationId() ||
		observed.GetDataJson() != requested.GetDataJson() ||
		observed.GetContentType() != requested.GetContentType() {
		t.Fatalf("Actor output append = %+v", observed)
	}
}

func serveFreshProgramProtocol(
	conn net.Conn,
	lease workerapi.RunLeaseAssignment,
	controlPlane *testFreshProgramControlPlane,
) error {
	defer conn.Close()
	if err := readFreshProgramAdmission(conn, lease); err != nil {
		return err
	}
	if len(controlPlane.snapshot()) != 0 {
		return errors.New("control plane ACK preceded process-started proof")
	}
	if err := frameio.WriteProtoFrame(conn, &runv0.RunEvent{
		Event: &runv0.RunEvent_ProgramProcessStarted{
			ProgramProcessStarted: &runv0.ProgramProcessStarted{
				RunId:         lease.RunID,
				AttemptNumber: uint32(lease.AttemptNumber),
				RunLeaseId:    lease.ID,
			},
		},
	}); err != nil {
		return err
	}
	var command runv0.ProgramSupervisorCommand
	if err := frameio.ReadProtoFrame(conn, &command); err != nil {
		return err
	}
	release := command.GetStartRelease()
	if release == nil ||
		release.GetRunId() != lease.RunID ||
		release.GetAttemptNumber() != uint32(lease.AttemptNumber) ||
		release.GetRunLeaseId() != lease.ID ||
		!onlyControlCalls(controlPlane.snapshot(), "start") {
		return errors.New("program-start release preceded start ACK")
	}
	if err := frameio.WriteProtoFrame(conn, &runv0.RunEvent{
		Event: &runv0.RunEvent_StdoutChunk{StdoutChunk: []byte("loading")},
	}); err != nil {
		return err
	}
	if err := frameio.WriteProtoFrame(conn, &runv0.RunEvent{
		Event: &runv0.RunEvent_StderrChunk{StderrChunk: []byte("notice")},
	}); err != nil {
		return err
	}
	identity := &runv0.EntrypointIdentity{
		DeclaredId: "deploy",
		Kind: &runv0.EntrypointIdentity_Task{
			Task: &runv0.TaskEntrypoint{},
		},
	}
	if err := frameio.WriteProtoFrame(conn, &runv0.RunEvent{
		Event: &runv0.RunEvent_EntrypointReady{
			EntrypointReady: &runv0.EntrypointReady{
				RunId:         lease.RunID,
				AttemptNumber: uint32(lease.AttemptNumber),
				Entrypoint:    identity,
			},
		},
	}); err != nil {
		return err
	}
	command.Reset()
	if err := frameio.ReadProtoFrame(conn, &command); err != nil {
		return err
	}
	entrypointRelease := command.GetEntrypointRelease()
	if entrypointRelease == nil ||
		entrypointRelease.GetRunId() != lease.RunID ||
		entrypointRelease.GetAttemptNumber() != uint32(lease.AttemptNumber) ||
		entrypointRelease.GetEntrypoint().GetDeclaredId() != "deploy" ||
		entrypointRelease.GetEntrypoint().GetTask() == nil ||
		!onlyControlCalls(controlPlane.snapshot(), "start", "entrypoint") {
		return errors.New("entrypoint release preceded entrypoint ACK")
	}
	if err := frameio.WriteProtoFrame(conn, testTaskSucceededEvent(`{"ok":true}`)); err != nil {
		return err
	}
	if err := frameio.WriteProtoFrame(conn, &runv0.RunEvent{
		Event: &runv0.RunEvent_StdoutChunk{StdoutChunk: []byte("done")},
	}); err != nil {
		return err
	}
	return frameio.WriteProtoFrame(conn, testProgramQuiescedEvent(lease))
}

func onlyControlCalls(calls []string, allowed ...string) bool {
	if len(calls) == 0 || calls[len(calls)-1] != allowed[len(allowed)-1] {
		return false
	}
	for _, call := range calls {
		if !slices.Contains(allowed, call) {
			return false
		}
	}
	for _, expected := range allowed {
		if !slices.Contains(calls, expected) {
			return false
		}
	}
	return true
}

func testTaskSucceededEvent(output string) *runv0.RunEvent {
	return &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskOutcome{
			TaskOutcome: &runv0.TaskOutcome{
				Outcome: &runv0.TaskOutcome_Succeeded{
					Succeeded: &runv0.TaskSucceeded{OutputJson: output},
				},
			},
		},
	}
}

func testProgramQuiescedEvent(lease workerapi.RunLeaseAssignment) *runv0.RunEvent {
	return &runv0.RunEvent{
		Event: &runv0.RunEvent_ProgramQuiesced{
			ProgramQuiesced: &runv0.ProgramQuiesced{
				RunId:         lease.RunID,
				AttemptNumber: uint32(lease.AttemptNumber),
				RunLeaseId:    lease.ID,
			},
		},
	}
}

func readFreshProgramAdmission(
	conn net.Conn,
	lease workerapi.RunLeaseAssignment,
) error {
	header, bodyLength, err := wire.ReadStreamFrameHeader(conn)
	if err != nil {
		return err
	}
	if header.Type != wire.StreamTypeProgramRun ||
		header.RunID != lease.RunID ||
		header.WorkspaceID != lease.WorkspaceID ||
		header.WorkspaceMountID != lease.WorkspaceMountID ||
		bodyLength != 0 {
		return fmt.Errorf("program header = %+v body=%d", header, bodyLength)
	}
	var authority workspacev0.WorkspaceRunAuthority
	if err := frameio.ReadProtoFrame(conn, &authority); err != nil {
		return err
	}
	fence := authority.GetFence()
	if authority.GetChannelToken() != "channel-1" ||
		authority.GetWriteCapability() != "write-capability" ||
		fence.GetWorkerInstanceId() != lease.WorkerInstanceID ||
		fence.GetWorkerEpoch() != lease.WorkerEpoch ||
		fence.GetRuntimeInstanceId() != lease.RuntimeInstanceID ||
		fence.GetRuntimeIdentityId() != lease.RuntimeIdentityID ||
		fence.GetWorkspaceMountId() != lease.WorkspaceMountID ||
		fence.GetWorkspaceId() != lease.WorkspaceID ||
		fence.GetRunId() != lease.RunID ||
		fence.GetAttemptNumber() != uint32(lease.AttemptNumber) ||
		fence.GetRunLeaseId() != lease.ID ||
		fence.GetLeaseSequence() != lease.LeaseSequence ||
		fence.GetWorkspaceLeaseId() != lease.WorkspaceLeaseID ||
		fence.GetOwnershipGeneration() != lease.OwnershipGeneration ||
		fence.GetWriterGeneration() != lease.WriterGeneration ||
		fence.GetMountFencingGeneration() != lease.MountFencingGeneration ||
		fence.GetExpiresAtUnixNano() != lease.ExpiresAt.UnixNano() ||
		fence.GetBaseWorkspaceVersionId() != lease.BaseWorkspaceVersionID {
		return fmt.Errorf("program authority = %+v", &authority)
	}
	var request runv0.ProgramRunRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return err
	}
	if request.GetRunId() != lease.RunID ||
		request.GetAttemptNumber() != uint32(lease.AttemptNumber) ||
		request.GetRunLeaseId() != lease.ID ||
		request.GetSecretCount() != 2 ||
		len(request.GetProgramStartFrame()) == 0 ||
		request.GetStartDeadlineUnixMs() != lease.StartDeadlineAt.UnixMilli() {
		return fmt.Errorf("program request = %+v", &request)
	}
	var command runv0.ProgramSupervisorCommand
	if err := frameio.ReadProtoFrame(conn, &command); err != nil {
		return err
	}
	first := command.GetSecretDelivery()
	if first == nil ||
		first.GetEnv() != "API_TOKEN" ||
		string(first.GetValue()) != "secret-one" {
		return fmt.Errorf("first program secret = %+v", first)
	}
	command.Reset()
	if err := frameio.ReadProtoFrame(conn, &command); err != nil {
		return err
	}
	second := command.GetSecretDelivery()
	if second == nil ||
		second.GetFile() != "/run/helmr-secrets/config.json" ||
		string(second.GetValue()) != "secret-two" {
		return fmt.Errorf("second program secret = %+v", second)
	}
	command.Reset()
	if err := frameio.ReadProtoFrame(conn, &command); err != nil {
		return err
	}
	complete := command.GetSecretsComplete()
	if complete == nil ||
		complete.GetRunId() != lease.RunID ||
		complete.GetAttemptNumber() != uint32(lease.AttemptNumber) ||
		complete.GetRunLeaseId() != lease.ID ||
		complete.GetSecretCount() != 2 {
		return fmt.Errorf("program secret completion = %+v", complete)
	}
	return nil
}

func testFreshProgramClaim(
	t *testing.T,
) workerapi.RunLeaseClaimResponse {
	t.Helper()
	var start bytes.Buffer
	if err := frameio.WriteProtoFrame(&start, &runv0.ProgramStart{
		RunId:                "run-1",
		AttemptNumber:        2,
		EntrypointDeclaredId: "deploy",
		Entrypoint: &runv0.ProgramStart_Task{
			Task: &runv0.TaskStart{
				Payload: &runv0.TaskStart_NoPayload{
					NoPayload: &runv0.NoPayload{},
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return workerapi.RunLeaseClaimResponse{
		Lease: workerapi.RunLeaseAssignment{
			ID:                     "lease-1",
			RunID:                  "run-1",
			AttemptNumber:          2,
			LeaseSequence:          3,
			WorkerInstanceID:       "worker-1",
			WorkerEpoch:            7,
			RuntimeInstanceID:      "runtime-1",
			RuntimeIdentityID:      "runtime-identity-1",
			WorkspaceID:            "workspace-1",
			WorkspaceMountID:       "mount-1",
			WorkspaceLeaseID:       "workspace-lease-1",
			BaseWorkspaceVersionID: "version-1",
			OwnershipGeneration:    2,
			WriterGeneration:       3,
			MountFencingGeneration: 4,
			StartDeadlineAt:        time.Now().Add(time.Minute).UTC(),
			ExpiresAt:              time.Now().Add(5 * time.Minute).UTC(),
		},
		Workspace: workerapi.WorkspaceAttachment{
			WriteCapability: "write-capability",
		},
		Secrets: []workerapi.SecretDelivery{
			{
				Env:   &workerapi.SecretEnv{Name: "API_TOKEN"},
				Value: []byte("secret-one"),
			},
			{
				File: &workerapi.SecretFile{
					Path: "/run/helmr-secrets/config.json",
				},
				Value: []byte("secret-two"),
			},
		},
		Execution: workerapi.RunLeaseExecution{
			Fresh: &workerapi.RunLeaseFresh{
				ProgramStart: start.Bytes(),
			},
		},
	}
}

func testChildAttachProgramClaim(
	t *testing.T,
) workerapi.RunLeaseClaimResponse {
	t.Helper()
	claim := testFreshProgramClaim(t)
	start := claim.Execution.Fresh.ProgramStart
	claim.Execution = workerapi.RunLeaseExecution{
		Attach: &workerapi.RunLeaseAttach{
			Child: &workerapi.RunLeaseChildAttach{
				ParentRunID:         "parent-run-1",
				ParentAttemptNumber: 3,
				RunWaitID:           "wait-1",
				CheckpointID:        "checkpoint-1",
				ResumeAttachID:      "attach-1",
				CorrelationID:       "correlation-1",
				ProgramStart:        start,
			},
		},
	}
	return claim
}

func testWorkspaceMount(
	lease workerapi.RunLeaseAssignment,
) workerapi.WorkspaceMount {
	return workerapi.WorkspaceMount{
		ID:                lease.WorkspaceMountID,
		WorkspaceID:       lease.WorkspaceID,
		RuntimeInstanceID: lease.RuntimeInstanceID,
		BaseVersionID:     lease.BaseWorkspaceVersionID,
		FencingGeneration: lease.MountFencingGeneration,
	}
}

type testFreshProgramControlPlane struct {
	mu                 sync.Mutex
	lease              workerapi.RunLeaseAssignment
	calls              []string
	startErr           error
	entrypointErr      error
	startFailures      int
	entrypointFailures int
	renewals           int
	wantChild          bool
}

type testFreshProgramLog struct {
	stream      workerapi.LogStream
	observedSeq uint64
	content     string
}

type testFreshProgramEventSink struct {
	mu   sync.Mutex
	logs []testFreshProgramLog
}

func (s *testFreshProgramEventSink) AppendRunLog(
	_ context.Context,
	_ workerapi.RunLeaseAssignment,
	stream workerapi.LogStream,
	observedSeq uint64,
	content []byte,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logs = append(s.logs, testFreshProgramLog{
		stream:      stream,
		observedSeq: observedSeq,
		content:     string(content),
	})
	return nil
}

func (s *testFreshProgramEventSink) ApplyRunMetadata(
	context.Context,
	workerapi.RunLeaseAssignment,
	*runv0.MetadataUpdated,
) error {
	return nil
}

func (s *testFreshProgramEventSink) RecordStructuredRunLog(
	context.Context,
	workerapi.RunLeaseAssignment,
	uint64,
	*runv0.StructuredLogRequested,
) error {
	return nil
}

func (s *testFreshProgramEventSink) snapshot() []testFreshProgramLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]testFreshProgramLog(nil), s.logs...)
}

func (c *testFreshProgramControlPlane) AcknowledgeRunStart(
	_ context.Context,
	request workerapi.RunStartRequest,
) (workerapi.RunStartResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "start")
	validArm := request.Fresh != nil &&
		request.Restore == nil &&
		request.Attach == nil
	if c.wantChild {
		validArm = request.Fresh == nil &&
			request.Restore == nil &&
			request.Attach != nil &&
			request.Attach.Child != nil &&
			request.Attach.Parent == nil &&
			request.Attach.Child.RunWaitID == "wait-1" &&
			request.Attach.Child.CheckpointID == "checkpoint-1" &&
			request.Attach.Child.ResumeAttachID == "attach-1"
	}
	if !validArm || request.Lease != c.lease.Fence() {
		return workerapi.RunStartResponse{}, errors.New(
			"unexpected start receipt",
		)
	}
	if c.startErr != nil {
		return workerapi.RunStartResponse{}, c.startErr
	}
	if c.startFailures > 0 {
		c.startFailures--
		return workerapi.RunStartResponse{}, errors.New("transient start acknowledgement failure")
	}
	return workerapi.RunStartResponse{Lease: c.lease.Fence()}, nil
}

func (c *testFreshProgramControlPlane) AcknowledgeRunEntrypoint(
	_ context.Context,
	request workerapi.RunEntrypointRequest,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "entrypoint")
	if request.Lease != c.lease.Fence() ||
		request.EntrypointKind != "task" ||
		request.EntrypointDeclaredID != "deploy" {
		return errors.New("unexpected entrypoint acknowledgement")
	}
	if c.entrypointErr != nil {
		return c.entrypointErr
	}
	if c.entrypointFailures > 0 {
		c.entrypointFailures--
		return errors.New("transient entrypoint acknowledgement failure")
	}
	return nil
}

func (c *testFreshProgramControlPlane) RenewRunLease(
	_ context.Context,
	lease workerapi.RunLeaseAssignment,
) (workerapi.RunLeaseRenewResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.renewals++
	if !equalRunLeaseAssignment(lease, c.lease) {
		return workerapi.RunLeaseRenewResponse{}, errors.New("unexpected renewal receipt")
	}
	return workerapi.RunLeaseRenewResponse{
		Lease: c.lease.Fence(), ExpiresAt: c.lease.ExpiresAt,
		BaseWorkspaceVersionID: c.lease.BaseWorkspaceVersionID,
	}, nil
}

func (c *testFreshProgramControlPlane) renewalCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.renewals
}

func (c *testFreshProgramControlPlane) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}
