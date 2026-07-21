package guestd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
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
		Event: &runv0.RunEvent_TaskResult{
			TaskResult: &runv0.TaskResult{
				ExitCode:   0,
				OutputJson: stringPointer(`{"ok":true}`),
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
	taskResult := event.GetTaskResult()
	if taskResult == nil ||
		taskResult.GetExitCode() != 0 ||
		taskResult.GetOutputJson() != `{"ok":true}` {
		t.Fatalf("Task result = %#v", event.GetEvent())
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
		waitDone: make(chan struct{}),
	}
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	var outputDone sync.WaitGroup
	err := relayProgram(
		context.Background(),
		guest,
		process,
		&programEventStream{conn: guest},
		make(chan error, 2),
		&outputDone,
	)
	if err == nil || !strings.Contains(err.Error(), "control event") {
		t.Fatalf("relayProgram() error = %v", err)
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
		channelToken:      "channel-1",
		fencingGeneration: 1,
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
		&workspacev0.WorkspaceOperationEnvelope{
			WorkspaceMountId:  "mount-1",
			WorkspaceId:       "workspace-1",
			ChannelToken:      "channel-1",
			FencingGeneration: 1,
			WriteLeaseId:      "write-lease-1",
			FencingToken:      "fence-1",
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
		waitDone:      make(chan struct{}),
	}
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

func stringPointer(value string) *string {
	return &value
}
