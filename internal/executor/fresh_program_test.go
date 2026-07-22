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

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/frameio"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func TestFreshProgramOrdersAdmissionEntrypointAndTaskCompletion(t *testing.T) {
	claim := testFreshProgramClaim(t)
	claim.Lease.ExpiresAt = time.Now().Add(5 * time.Second).UTC()
	control := &testFreshProgramControl{
		lease:              claim.Lease,
		startFailures:      1,
		entrypointFailures: 1,
	}
	events := &testFreshProgramEventSink{}
	guest, host := net.Pipe()
	defer guest.Close()
	sessions := NewWorkspaceMountSessions()
	unregister := sessions.RegisterWorkspaceMountSession(
		api.WorkerWorkspaceMount{ID: claim.Lease.WorkspaceMountID},
		fakeGuestSession{stream: host},
		"channel-1",
	)
	defer unregister()
	guestResult := make(chan error, 1)
	go func() {
		guestResult <- serveFreshProgramProtocol(
			guest,
			claim.Lease,
			control,
		)
	}()
	program, err := (GuestRunner{
		WorkspaceMounts: sessions,
	}).startFreshProgram(
		context.Background(),
		&claim,
		control,
		events,
	)
	if err != nil {
		t.Fatal(err)
	}
	if program.lease != control.lease ||
		program.entrypoint.GetDeclaredId() != "deploy" ||
		program.entrypoint.GetTask() == nil ||
		program.observedEventSeq != 4 {
		t.Fatalf("fresh Program = %+v", program)
	}
	outcome, quiesced, err := program.awaitTaskCompletion(context.Background(), events, nil)
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
	if !slices.Equal(control.snapshot(), []string{
		"start", "start", "entrypoint", "entrypoint",
	}) {
		t.Fatalf("control calls = %v", control.snapshot())
	}
	if control.renewalCount() != 1 {
		t.Fatalf("Run Lease renewals = %d", control.renewalCount())
	}
	if !slices.Equal(events.snapshot(), []testFreshProgramLog{
		{stream: api.WorkerLogStreamStdout, observedSeq: 2, content: "loading"},
		{stream: api.WorkerLogStreamStderr, observedSeq: 3, content: "notice"},
		{stream: api.WorkerLogStreamStdout, observedSeq: 6, content: "done"},
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

func TestStartFreshProgramDoesNotReleaseAfterStartRejection(t *testing.T) {
	claim := testFreshProgramClaim(t)
	control := &testFreshProgramControl{
		lease: claim.Lease,
		startErr: &client.HTTPError{
			StatusCode: http.StatusConflict,
			Status:     "Conflict",
			Message:    "stale start",
		},
	}
	guest, host := net.Pipe()
	defer guest.Close()
	sessions := NewWorkspaceMountSessions()
	unregister := sessions.RegisterWorkspaceMountSession(
		api.WorkerWorkspaceMount{ID: claim.Lease.WorkspaceMountID},
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
	_, err := (GuestRunner{
		WorkspaceMounts: sessions,
	}).startFreshProgram(
		context.Background(),
		&claim,
		control,
		&testFreshProgramEventSink{},
	)
	if err == nil || !strings.Contains(err.Error(), "stale start") {
		t.Fatalf("startFreshProgram() error = %v", err)
	}
	if err := <-proofSent; err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(control.snapshot(), []string{"start"}) {
		t.Fatalf("control calls = %v", control.snapshot())
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
		api.WorkerWorkspaceMount{ID: claim.Lease.WorkspaceMountID},
		fakeGuestSession{stream: host},
		"channel-1",
	)
	defer unregister()
	started := time.Now()
	_, err := (GuestRunner{
		WorkspaceMounts: sessions,
	}).startFreshProgram(
		context.Background(),
		&claim,
		&testFreshProgramControl{lease: claim.Lease},
		&testFreshProgramEventSink{},
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startFreshProgram() error = %v", err)
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
	_, err := validateFreshProgramClaim(&claim)
	if err == nil || !strings.Contains(err.Error(), "plaintext exceeds") {
		t.Fatalf("validateFreshProgramClaim() error = %v", err)
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
				testProgramQuiescedEvent(api.WorkerRunLeaseReceipt{
					ID: "lease-1", RunID: "run-1", AttemptNumber: 2,
				}),
			},
			want: "before emitting",
		},
		{
			name: "mismatched proof",
			events: []*runv0.RunEvent{
				testTaskSucceededEvent(`null`),
				testProgramQuiescedEvent(api.WorkerRunLeaseReceipt{
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
				lease: api.WorkerRunLeaseReceipt{
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
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("awaitTaskCompletion() error = %v", err)
			}
		})
	}
}

func serveFreshProgramProtocol(
	conn net.Conn,
	lease api.WorkerRunLeaseReceipt,
	control *testFreshProgramControl,
) error {
	defer conn.Close()
	if err := readFreshProgramAdmission(conn, lease); err != nil {
		return err
	}
	if len(control.snapshot()) != 0 {
		return errors.New("Control ACK preceded process-started proof")
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
		!onlyControlCalls(control.snapshot(), "start") {
		return errors.New("Program-start release preceded start ACK")
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
		!onlyControlCalls(control.snapshot(), "start", "entrypoint") {
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

func testProgramQuiescedEvent(lease api.WorkerRunLeaseReceipt) *runv0.RunEvent {
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
	lease api.WorkerRunLeaseReceipt,
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
		return fmt.Errorf("Program header = %+v body=%d", header, bodyLength)
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
		return fmt.Errorf("Program authority = %+v", &authority)
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
		return fmt.Errorf("Program request = %+v", &request)
	}
	var command runv0.ProgramSupervisorCommand
	if err := frameio.ReadProtoFrame(conn, &command); err != nil {
		return err
	}
	first := command.GetSecretDelivery()
	if first == nil ||
		first.GetEnv() != "API_TOKEN" ||
		string(first.GetValue()) != "secret-one" {
		return fmt.Errorf("first Program Secret = %+v", first)
	}
	command.Reset()
	if err := frameio.ReadProtoFrame(conn, &command); err != nil {
		return err
	}
	second := command.GetSecretDelivery()
	if second == nil ||
		second.GetFile() != "/run/helmr-secrets/config.json" ||
		string(second.GetValue()) != "secret-two" {
		return fmt.Errorf("second Program Secret = %+v", second)
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
		return fmt.Errorf("Program Secret completion = %+v", complete)
	}
	return nil
}

func testFreshProgramClaim(
	t *testing.T,
) api.WorkerRunLeaseClaimResponse {
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
	return api.WorkerRunLeaseClaimResponse{
		Lease: api.WorkerRunLeaseReceipt{
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
		Workspace: api.WorkerWorkspaceAttachment{
			WriteCapability: "write-capability",
		},
		Secrets: []api.WorkerSecretDelivery{
			{
				Env:   &api.WorkerSecretEnv{Name: "API_TOKEN"},
				Value: []byte("secret-one"),
			},
			{
				File: &api.WorkerSecretFile{
					Path: "/run/helmr-secrets/config.json",
				},
				Value: []byte("secret-two"),
			},
		},
		Execution: api.WorkerRunLeaseExecution{
			Fresh: &api.WorkerRunLeaseFresh{
				ProgramStart: start.Bytes(),
			},
		},
	}
}

type testFreshProgramControl struct {
	mu                 sync.Mutex
	lease              api.WorkerRunLeaseReceipt
	calls              []string
	startErr           error
	entrypointErr      error
	startFailures      int
	entrypointFailures int
	renewals           int
}

type testFreshProgramLog struct {
	stream      api.WorkerLogStream
	observedSeq uint64
	content     string
}

type testFreshProgramEventSink struct {
	mu   sync.Mutex
	logs []testFreshProgramLog
}

func (s *testFreshProgramEventSink) AppendRunLog(
	_ context.Context,
	_ api.WorkerRunLeaseReceipt,
	stream api.WorkerLogStream,
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

func (s *testFreshProgramEventSink) snapshot() []testFreshProgramLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]testFreshProgramLog(nil), s.logs...)
}

func (c *testFreshProgramControl) AcknowledgeRunStart(
	_ context.Context,
	request api.WorkerRunStartRequest,
) (api.WorkerRunStartResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "start")
	if request.Fresh == nil || request.Restore != nil || request.Attach != nil ||
		!equalRunLeaseReceipt(request.Lease, c.lease) {
		return api.WorkerRunStartResponse{}, errors.New(
			"unexpected start receipt",
		)
	}
	if c.startErr != nil {
		return api.WorkerRunStartResponse{}, c.startErr
	}
	if c.startFailures > 0 {
		c.startFailures--
		return api.WorkerRunStartResponse{}, errors.New("transient start acknowledgement failure")
	}
	return api.WorkerRunStartResponse{Lease: c.lease}, nil
}

func (c *testFreshProgramControl) AcknowledgeRunEntrypoint(
	_ context.Context,
	request api.WorkerRunEntrypointRequest,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "entrypoint")
	if !equalRunLeaseReceipt(request.Lease, c.lease) ||
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

func (c *testFreshProgramControl) RenewRunLease(
	_ context.Context,
	lease api.WorkerRunLeaseReceipt,
) (api.WorkerRunLeaseRenewResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.renewals++
	if !equalRunLeaseReceipt(lease, c.lease) {
		return api.WorkerRunLeaseRenewResponse{}, errors.New("unexpected renewal receipt")
	}
	return api.WorkerRunLeaseRenewResponse{Lease: c.lease}, nil
}

func (c *testFreshProgramControl) renewalCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.renewals
}

func (c *testFreshProgramControl) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.calls...)
}
