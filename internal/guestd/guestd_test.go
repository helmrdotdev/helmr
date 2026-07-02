package guestd

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/safepath"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

func TestMain(m *testing.M) {
	if os.Getenv("HELMR_GUESTD_HELPER") != "" {
		os.Exit(runGuestAdapterHelperProcess())
	}
	root, err := os.MkdirTemp("/tmp", "helmr-guestd-test-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("HELMR_GUESTD_TMPDIR", root); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

func TestRunAdapterForwardsOutputAndCompletion(t *testing.T) {
	tempDir, runner := guestAdapterHelperRunner(t, "stdout-stderr")
	var stream bytes.Buffer
	err := runAdapter(context.Background(), &stream, Config{
		AdapterRuntimePath: runner,
		AdapterPath:        "adapter.js",
	}, tempDir, tempDir, tempDir, tempDir, ociRuntimeConfig{}, false, testRunTaskRequest(), newWaitingRunRegistry())
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr string
	var completed bool
	for !completed {
		event, err := wire.ReadRunEvent(&stream)
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StdoutChunk:
			stdout += string(value.StdoutChunk)
		case *runv0.RunEvent_StderrChunk:
			stderr += string(value.StderrChunk)
		case *runv0.RunEvent_TaskResult:
			completed = true
			if value.TaskResult.ExitCode != 0 {
				t.Fatalf("exit code = %d", value.TaskResult.ExitCode)
			}
		}
	}
	if !strings.Contains(stdout, "stdout-line") {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr, "stderr-line") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunAdapterDrainsOutputTailAfterProcessExit(t *testing.T) {
	tempDir, runner := guestAdapterHelperRunner(t, "stdout-stderr-tail")
	var stream bytes.Buffer
	err := runAdapter(context.Background(), &stream, Config{
		AdapterRuntimePath: runner,
		AdapterPath:        "adapter.js",
	}, tempDir, tempDir, tempDir, tempDir, ociRuntimeConfig{}, false, testRunTaskRequest(), newWaitingRunRegistry())
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr string
	var complete *runv0.TaskResult
	for complete == nil {
		event, err := wire.ReadRunEvent(&stream)
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StdoutChunk:
			stdout += string(value.StdoutChunk)
		case *runv0.RunEvent_StderrChunk:
			stderr += string(value.StderrChunk)
		case *runv0.RunEvent_TaskResult:
			complete = value.TaskResult
		}
	}
	if complete.GetExitCode() != 0 {
		t.Fatalf("exit code = %d message=%v", complete.GetExitCode(), complete.ErrorMessage)
	}
	if !strings.HasSuffix(stdout, "stdout-tail\n") {
		t.Fatalf("stdout tail missing, len=%d tail=%q", len(stdout), tailString(stdout, 64))
	}
	if !strings.HasSuffix(stderr, "stderr-tail\n") {
		t.Fatalf("stderr tail missing, len=%d tail=%q", len(stderr), tailString(stderr, 64))
	}
}

func TestRunAdapterDoesNotTreatStdoutAsTaskOutput(t *testing.T) {
	tempDir, runner := guestAdapterHelperRunner(t, "stdout-json")
	var stream bytes.Buffer
	err := runAdapter(context.Background(), &stream, Config{
		AdapterRuntimePath: runner,
		AdapterPath:        "adapter.js",
	}, tempDir, tempDir, tempDir, tempDir, ociRuntimeConfig{}, false, testRunTaskRequest(), newWaitingRunRegistry())
	if err != nil {
		t.Fatal(err)
	}
	var complete *runv0.TaskResult
	for complete == nil {
		event, err := wire.ReadRunEvent(&stream)
		if err != nil {
			t.Fatal(err)
		}
		complete = event.GetTaskResult()
	}
	if complete.GetExitCode() != 0 {
		t.Fatalf("exit code = %d", complete.GetExitCode())
	}
	if complete.OutputJson != nil {
		t.Fatalf("output = %q", complete.GetOutputJson())
	}
}

func TestRunAdapterDoesNotSetOutputOnNonzeroExit(t *testing.T) {
	tempDir, runner := guestAdapterHelperRunner(t, "stdout-json-exit-3")
	var stream bytes.Buffer
	err := runAdapter(context.Background(), &stream, Config{
		AdapterRuntimePath: runner,
		AdapterPath:        "adapter.js",
	}, tempDir, tempDir, tempDir, tempDir, ociRuntimeConfig{}, false, testRunTaskRequest(), newWaitingRunRegistry())
	if err != nil {
		t.Fatal(err)
	}
	var complete *runv0.TaskResult
	for complete == nil {
		event, err := wire.ReadRunEvent(&stream)
		if err != nil {
			t.Fatal(err)
		}
		complete = event.GetTaskResult()
	}
	if complete.GetExitCode() != 3 {
		t.Fatalf("exit code = %d", complete.GetExitCode())
	}
	if complete.OutputJson != nil {
		t.Fatalf("output = %q", complete.GetOutputJson())
	}
}

func TestRunAdapterForwardsTaskOutputBeforeDescendantFDEOF(t *testing.T) {
	tempDir, runner := guestAdapterHelperRunner(t, "task-output-fd-holder")
	releasePath := filepath.Join(tempDir, "release-fd-holder")
	releaseHolder := func() {
		_ = os.WriteFile(releasePath, []byte("release"), 0o644)
	}
	defer releaseHolder()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	if err := host.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- runAdapter(ctx, guest, Config{
			AdapterRuntimePath: runner,
			AdapterPath:        "adapter.js",
		}, tempDir, tempDir, tempDir, tempDir, ociRuntimeConfig{}, false, testRunTaskRequest(), newWaitingRunRegistry())
	}()

	var complete *runv0.TaskResult
	for complete == nil {
		event, err := wire.ReadRunEvent(host)
		if err != nil {
			t.Fatal(err)
		}
		complete = event.GetTaskResult()
	}
	if got := complete.GetOutputJson(); got != `{"ok":true}` {
		t.Fatalf("task output = %q", got)
	}
	if complete.ExitCode != 0 {
		t.Fatalf("exit code = %d message=%v", complete.ExitCode, complete.ErrorMessage)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runAdapter did not return after descendant fd holder exited")
	}
}

func TestRunAdapterNoResultTerminatesDescendantFDEOF(t *testing.T) {
	tempDir, runner := guestAdapterHelperRunner(t, "no-result-fd-holder")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	if err := host.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- runAdapter(ctx, guest, Config{
			AdapterRuntimePath: runner,
			AdapterPath:        "adapter.js",
		}, tempDir, tempDir, tempDir, tempDir, ociRuntimeConfig{}, false, testRunTaskRequest(), newWaitingRunRegistry())
	}()

	var complete *runv0.TaskResult
	for complete == nil {
		event, err := wire.ReadRunEvent(host)
		if err != nil {
			t.Fatal(err)
		}
		complete = event.GetTaskResult()
	}
	if complete.ExitCode != 1 {
		t.Fatalf("exit code = %d message=%v", complete.ExitCode, complete.ErrorMessage)
	}
	if complete.ErrorMessage == nil || !strings.Contains(*complete.ErrorMessage, "without reporting task_result") {
		t.Fatalf("error message = %v", complete.ErrorMessage)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runAdapter did not return after no-result adapter left inherited fds open")
	}
}

func TestRunAdapterPrefersLateTaskResultAfterWaitTimeout(t *testing.T) {
	tempDir, runner := guestAdapterHelperRunner(t, "task-result-after-blocked-control-event")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()

	errCh := make(chan error, 1)
	go func() {
		errCh <- runAdapter(ctx, guest, Config{
			AdapterRuntimePath: runner,
			AdapterPath:        "adapter.js",
		}, tempDir, tempDir, tempDir, tempDir, ociRuntimeConfig{}, false, testRunTaskRequest(), newWaitingRunRegistry())
	}()

	time.Sleep(400 * time.Millisecond)
	if err := host.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	event, err := wire.ReadRunEvent(host)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(event.GetStdoutChunk()); got != "blocked-before-result\n" {
		t.Fatalf("first event stdout = %q event=%+v", got, event)
	}

	var complete *runv0.TaskResult
	for complete == nil {
		event, err := wire.ReadRunEvent(host)
		if err != nil {
			t.Fatal(err)
		}
		complete = event.GetTaskResult()
	}
	if got := complete.GetOutputJson(); got != `{"late":true}` {
		t.Fatalf("output = %q", got)
	}
	if complete.ExitCode != 0 {
		t.Fatalf("exit code = %d message=%v", complete.ExitCode, complete.ErrorMessage)
	}
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runAdapter did not return")
	}
}

func TestServeHealthReportsStartingUntilReady(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	var ready atomic.Bool
	go serveHealth(listener, ready.Load, nil)

	client := http.Client{Timeout: time.Second}
	url := "http://" + listener.Addr().String()
	assertHealthStatus := func(want string) {
		t.Helper()
		resp, err := client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"status":"`+want+`"`) {
			t.Fatalf("health body = %s, want status %q", body, want)
		}
	}

	assertHealthStatus("starting")
	ready.Store(true)
	assertHealthStatus("ok")
}

func TestServeHealthLogsResponseTiming(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	var logBuffer lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logBuffer, nil))
	var ready atomic.Bool
	ready.Store(true)
	go serveHealth(listener, ready.Load, logger)

	client := http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	deadline := time.Now().Add(time.Second)
	for {
		logs := logBuffer.String()
		requestIndex := strings.Index(logs, "guestd health request received")
		responseIndex := strings.Index(logs, "guestd health response written")
		if requestIndex >= 0 &&
			responseIndex > requestIndex &&
			strings.Contains(logs, "duration_ms=") &&
			strings.Contains(logs, "flush_duration_ms=") &&
			strings.Contains(logs, "bytes=36") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("health logs = %q, want response timing", logs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunAdapterReportsPrelaunchFailure(t *testing.T) {
	tempDir := t.TempDir()
	writeTestTaskProjectPackage(t, tempDir)
	var stream bytes.Buffer
	err := runAdapter(context.Background(), &stream, Config{
		AdapterRuntimePath: filepath.Join(tempDir, "missing-runner"),
		AdapterPath:        "adapter.js",
	}, tempDir, tempDir, tempDir, tempDir, ociRuntimeConfig{}, false, testRunTaskRequest(), newWaitingRunRegistry())
	if err != nil {
		t.Fatal(err)
	}
	stderr, complete := readGuestdFailureEvents(t, &stream)
	if !strings.Contains(stderr, "missing-runner") {
		t.Fatalf("stderr = %q", stderr)
	}
	if complete.ExitCode != 1 {
		t.Fatalf("exit code = %d", complete.ExitCode)
	}
	if complete.ErrorMessage == nil || !strings.Contains(*complete.ErrorMessage, "missing-runner") {
		t.Fatalf("error message = %v", complete.ErrorMessage)
	}
}

func TestRunAdapterReportsMalformedControlEvent(t *testing.T) {
	for _, helper := range []string{"malformed-control", "malformed-control-exit-42"} {
		t.Run(helper, func(t *testing.T) {
			tempDir, runner := guestAdapterHelperRunner(t, helper)
			var stream bytes.Buffer
			err := runAdapter(context.Background(), &stream, Config{
				AdapterRuntimePath: runner,
				AdapterPath:        "-test.run=TestGuestAdapterHelperProcess",
			}, tempDir, tempDir, tempDir, tempDir, ociRuntimeConfig{}, false, testRunTaskRequest(), newWaitingRunRegistry())
			if err != nil {
				t.Fatal(err)
			}
			stderr, complete := readGuestdFailureEvents(t, &stream)
			if complete.ExitCode != 1 {
				t.Fatalf("exit code = %d", complete.ExitCode)
			}
			if complete.ErrorMessage == nil || !strings.Contains(*complete.ErrorMessage, "read adapter control event") {
				t.Fatalf("complete = %+v stderr=%q", complete, stderr)
			}
		})
	}
}

func TestRunAdapterReportsWaitHandoffControlFailure(t *testing.T) {
	tempDir, runner := guestAdapterHelperRunner(t, "wait-control-only")
	stream := &runSetupStream{read: bytes.NewReader(nil)}
	err := runAdapter(context.Background(), stream, Config{
		AdapterRuntimePath: runner,
		AdapterPath:        "-test.run=TestGuestAdapterHelperProcess",
	}, tempDir, tempDir, tempDir, tempDir, ociRuntimeConfig{}, false, testRunTaskRequest(), newWaitingRunRegistry())
	if err != nil {
		t.Fatal(err)
	}
	var sawWait bool
	for {
		event, err := wire.ReadRunEvent(&stream.written)
		if err != nil {
			t.Fatal(err)
		}
		if event.GetRunWaitRequested() != nil {
			sawWait = true
			continue
		}
		if event.GetLogEntry() != "" {
			continue
		}
		complete := event.GetTaskResult()
		if complete == nil {
			t.Fatalf("unexpected event = %+v", event)
		}
		if !sawWait {
			t.Fatal("wait request was not forwarded before failure")
		}
		if complete.ExitCode != 1 || complete.ErrorMessage == nil || !strings.Contains(*complete.ErrorMessage, "read checkpoint suspend request") {
			t.Fatalf("complete = %+v", complete)
		}
		return
	}
}

func TestRunAdapterBridgesActiveStreamReadResultToAdapterStdin(t *testing.T) {
	t.Setenv("HELMR_GUESTD_HELPER", "active-stream-read-control")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	tempDir, runner := guestAdapterHelperRunner(t, "active-stream-read-control")
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	errCh := make(chan error, 1)
	go func() {
		errCh <- runAdapter(ctx, guest, Config{
			AdapterRuntimePath: runner,
			AdapterPath:        "-test.run=TestGuestAdapterHelperProcess",
		}, tempDir, tempDir, tempDir, tempDir, ociRuntimeConfig{}, false, testRunTaskRequest(), newWaitingRunRegistry())
	}()

	event, err := wire.ReadRunEvent(host)
	if err != nil {
		t.Fatal(err)
	}
	read := event.GetActiveStreamReadRequested()
	if read == nil {
		t.Fatalf("event = %+v, want active stream read request", event)
	}
	if read.CorrelationId != "active-read-1" || read.Stream != "inbox" || read.Block {
		t.Fatalf("active read request = %+v", read)
	}
	if err := frameio.WriteProtoFrame(host, &runv0.ActiveStreamReadResult{
		CorrelationId: "active-read-1",
		TimedOut:      true,
	}); err != nil {
		t.Fatal(err)
	}

	var complete *runv0.TaskResult
	for complete == nil {
		event, err := wire.ReadRunEvent(host)
		if err != nil {
			t.Fatal(err)
		}
		complete = event.GetTaskResult()
	}
	if complete.ExitCode != 0 || complete.OutputJson == nil || *complete.OutputJson != `{"timedOut":true}` {
		t.Fatalf("complete = %+v", complete)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestHandleRunRejectsSourceOnlyRun(t *testing.T) {
	var stream bytes.Buffer
	err := handleRunConnection(context.Background(), &stream, Config{
		AdapterRuntimePath: "/bin/false",
		AdapterPath:        "adapter.js",
	}, slogDiscard(), newWaitingRunRegistry(), wire.StreamHeader{Type: wire.StreamTypeWorkspaceArtifact, RunID: "run"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	stderr, complete := readGuestdFailureEvents(t, &stream)
	if !strings.Contains(stderr, `unsupported runtime input type "workspace-artifact"`) {
		t.Fatalf("stderr = %q", stderr)
	}
	if complete.ExitCode != 1 {
		t.Fatalf("exit code = %d", complete.ExitCode)
	}
}

func TestHandleRunRejectsMismatchedRunIDs(t *testing.T) {
	tests := []struct {
		name         string
		imageRunID   string
		sourceRunID  string
		requestRunID string
		want         string
	}{
		{
			name:         "deployment source stream",
			imageRunID:   "run-1",
			sourceRunID:  "run-2",
			requestRunID: "run-1",
			want:         `deployment source run_id "run-2" does not match run image run_id "run-1"`,
		},
		{
			name:         "run request",
			imageRunID:   "run-1",
			sourceRunID:  "run-1",
			requestRunID: "run-2",
			want:         `run request run_id "run-2" does not match runtime input run_id "run-1"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var input bytes.Buffer
			image := ociTar(t, []ociTestLayer{{mediaType: "application/vnd.oci.image.layer.v1.tar", body: tarBytes(t, nil)}}, []byte(`{"Config":{}}`))
			source := tarBytes(t, nil)
			if _, err := input.Write(image); err != nil {
				t.Fatal(err)
			}
			if err := wire.WriteStreamFrameHeader(&input, wire.StreamHeader{Type: wire.StreamTypeDeploymentSource, RunID: tt.sourceRunID}, uint64(len(source))); err != nil {
				t.Fatal(err)
			}
			if _, err := input.Write(source); err != nil {
				t.Fatal(err)
			}
			if err := frameio.WriteProtoFrame(&input, &runv0.RunTaskRequest{
				RunId:       tt.requestRunID,
				TaskId:      "task",
				PayloadJson: "{}",
			}); err != nil {
				t.Fatal(err)
			}
			if tt.sourceRunID == tt.imageRunID {
				if err := wire.WriteStreamFrameHeader(&input, wire.StreamHeader{Type: wire.StreamTypeWorkspaceArtifact, RunID: tt.sourceRunID}, uint64(len(source))); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := input.Write(source); err != nil {
				t.Fatal(err)
			}
			stream := &runSetupStream{read: bytes.NewReader(input.Bytes())}

			err := handleRunConnection(context.Background(), stream, Config{
				AdapterRuntimePath: "/bin/false",
				AdapterPath:        "adapter.js",
			}, slogDiscard(), newWaitingRunRegistry(), wire.StreamHeader{Type: wire.StreamTypeRunImage, RunID: tt.imageRunID}, uint64(len(image)))
			if err != nil {
				t.Fatal(err)
			}
			stderr, complete := readGuestdFailureEvents(t, &stream.written)
			if !strings.Contains(stderr, tt.want) {
				t.Fatalf("stderr = %q, want %q", stderr, tt.want)
			}
			if complete.ExitCode != 1 || complete.ErrorMessage == nil || !strings.Contains(*complete.ErrorMessage, tt.want) {
				t.Fatalf("complete = %+v", complete)
			}
		})
	}
}

func TestHandleRunConnectionDrainsRequestAfterSourceExtractionError(t *testing.T) {
	var input bytes.Buffer
	image := ociTar(t, []ociTestLayer{{mediaType: "application/vnd.oci.image.layer.v1.tar", body: tarBytes(t, nil)}}, []byte(`{"Config":{}}`))
	source := testTar(t, nil, &tar.Header{Name: "../escape.txt", Mode: 0o644, Size: 0})
	if _, err := input.Write(image); err != nil {
		t.Fatal(err)
	}
	deploymentSource := tarBytes(t, nil)
	if err := wire.WriteStreamFrameHeader(&input, wire.StreamHeader{Type: wire.StreamTypeDeploymentSource, RunID: "run-1"}, uint64(len(deploymentSource))); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write(deploymentSource); err != nil {
		t.Fatal(err)
	}
	request := testRunTaskRequest()
	request.RunId = "run-1"
	request.Workspace.Artifact.SizeBytes = uint64(len(source))
	request.Workspace.Artifact.EntryCount = 1
	if err := frameio.WriteProtoFrame(&input, request); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteStreamFrameHeader(&input, wire.StreamHeader{Type: wire.StreamTypeWorkspaceArtifact, RunID: "run-1"}, uint64(len(source))); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write(source); err != nil {
		t.Fatal(err)
	}
	stream := &runSetupStream{read: bytes.NewReader(input.Bytes())}
	err := handleRunConnection(context.Background(), stream, Config{}, slogDiscard(), newWaitingRunRegistry(), wire.StreamHeader{Type: wire.StreamTypeRunImage, RunID: "run-1"}, uint64(len(image)))
	if err != nil {
		t.Fatal(err)
	}
	if stream.read.Len() != 0 {
		t.Fatalf("unread bytes = %d", stream.read.Len())
	}
	stderr, complete := readGuestdFailureEvents(t, &stream.written)
	if !strings.Contains(stderr, "extract workspace artifact") || complete.ExitCode != 1 {
		t.Fatalf("stderr = %q complete = %+v", stderr, complete)
	}
}

func TestHandleRunConnectionRejectsWorkspaceArtifactBodyDigestMismatch(t *testing.T) {
	var input bytes.Buffer
	image := ociTar(t, []ociTestLayer{{mediaType: "application/vnd.oci.image.layer.v1.tar", body: tarBytes(t, nil)}}, []byte(`{"Config":{}}`))
	source := tarBytes(t, map[string]string{"workspace.txt": "workspace"})
	if _, err := input.Write(image); err != nil {
		t.Fatal(err)
	}
	deploymentSource := tarBytes(t, nil)
	if err := wire.WriteStreamFrameHeader(&input, wire.StreamHeader{Type: wire.StreamTypeDeploymentSource, RunID: "run-1"}, uint64(len(deploymentSource))); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write(deploymentSource); err != nil {
		t.Fatal(err)
	}
	request := testRunTaskRequest()
	request.RunId = "run-1"
	request.Workspace.Artifact.Digest = sha256sum.DigestBytes([]byte("not the tar body"))
	request.Workspace.Artifact.SizeBytes = uint64(len(source))
	request.Workspace.Artifact.EntryCount = 1
	if err := frameio.WriteProtoFrame(&input, request); err != nil {
		t.Fatal(err)
	}
	declaredDigest := request.Workspace.Artifact.Digest
	if err := wire.WriteStreamFrameHeader(&input, wire.StreamHeader{Type: wire.StreamTypeWorkspaceArtifact, RunID: "run-1", BodyDigest: &declaredDigest}, uint64(len(source))); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write(source); err != nil {
		t.Fatal(err)
	}
	stream := &runSetupStream{read: bytes.NewReader(input.Bytes())}

	err := handleRunConnection(context.Background(), stream, Config{}, slogDiscard(), newWaitingRunRegistry(), wire.StreamHeader{Type: wire.StreamTypeRunImage, RunID: "run-1"}, uint64(len(image)))
	if err != nil {
		t.Fatal(err)
	}
	stderr, complete := readGuestdFailureEvents(t, &stream.written)
	if !strings.Contains(stderr, "workspace artifact body digest") || complete.ExitCode != 1 {
		t.Fatalf("stderr = %q complete = %+v", stderr, complete)
	}
}

func TestHandleRunConnectionAcceptsEmptyWorkspaceArtifact(t *testing.T) {
	var input bytes.Buffer
	image := ociTar(t, []ociTestLayer{{mediaType: "application/vnd.oci.image.layer.v1.tar", body: tarBytes(t, nil)}}, []byte(`{"Config":{}}`))
	deploymentSource := tarBytes(t, nil)
	workspaceArtifact := tarBytes(t, nil)
	if _, err := input.Write(image); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteStreamFrameHeader(&input, wire.StreamHeader{Type: wire.StreamTypeDeploymentSource, RunID: "run-1"}, uint64(len(deploymentSource))); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write(deploymentSource); err != nil {
		t.Fatal(err)
	}
	request := testRunTaskRequest()
	request.RunId = "run-1"
	request.ModulePath = "task.ts"
	request.Workspace.Artifact.Digest = sha256sum.DigestBytes(workspaceArtifact)
	request.Workspace.Artifact.SizeBytes = uint64(len(workspaceArtifact))
	request.Workspace.Artifact.EntryCount = 0
	if err := frameio.WriteProtoFrame(&input, request); err != nil {
		t.Fatal(err)
	}
	declaredDigest := request.Workspace.Artifact.Digest
	if err := wire.WriteStreamFrameHeader(&input, wire.StreamHeader{Type: wire.StreamTypeWorkspaceArtifact, RunID: "run-1", BodyDigest: &declaredDigest}, uint64(len(workspaceArtifact))); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write(workspaceArtifact); err != nil {
		t.Fatal(err)
	}
	stream := &runSetupStream{read: bytes.NewReader(input.Bytes())}

	err := handleRunConnection(context.Background(), stream, Config{
		AdapterRuntimePath: "/bin/false",
		AdapterPath:        "adapter.js",
	}, slogDiscard(), newWaitingRunRegistry(), wire.StreamHeader{Type: wire.StreamTypeRunImage, RunID: "run-1"}, uint64(len(image)))
	if err != nil {
		t.Fatal(err)
	}
	stderr, complete := readGuestdFailureEvents(t, &stream.written)
	if strings.Contains(stderr, "workspace artifact entry_count") {
		t.Fatalf("empty workspace artifact was rejected: stderr = %q complete = %+v", stderr, complete)
	}
	if complete.ExitCode != 1 {
		t.Fatalf("complete = %+v", complete)
	}
}

func TestHandleWorkspaceRunConnectionUsesRegisteredWorkspaceMount(t *testing.T) {
	imageRoot := t.TempDir()
	workspaceRoot := filepath.Join(imageRoot, "workspace")
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	registry := newWorkspaceOperationRegistry()
	registry.register("mat-1", &workspaceMountEntry{
		channelToken:      "channel-token",
		workspaceID:       "workspace-1",
		fencingGeneration: 2,
		imageRoot:         imageRoot,
		imageConfig:       ociRuntimeConfig{},
		workspaceMount:    "/workspace",
		workspaceRoot:     workspaceRoot,
		processes:         map[string]*workspaceProcess{},
		events:            make(chan *workspacev0.WorkspaceOperationEvent, 1),
		eventsDone:        make(chan struct{}),
		cleanup:           func() {},
	})
	var input bytes.Buffer
	if err := frameio.WriteProtoFrame(&input, &workspacev0.WorkspaceOperationEnvelope{
		WorkspaceMountId:  "mat-1",
		WorkspaceId:       "workspace-1",
		ChannelToken:      "channel-token",
		FencingGeneration: 2,
		WriteLeaseId:      "write-lease-1",
		FencingToken:      "write-token-1",
	}); err != nil {
		t.Fatal(err)
	}
	deploymentSource := tarBytes(t, nil)
	if err := wire.WriteStreamFrameHeader(&input, wire.StreamHeader{Type: wire.StreamTypeDeploymentSource, RunID: "run-1"}, uint64(len(deploymentSource))); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write(deploymentSource); err != nil {
		t.Fatal(err)
	}
	request := testRunTaskRequest()
	request.RunId = "run-1"
	request.Cwd = "/workspace"
	if err := frameio.WriteProtoFrame(&input, request); err != nil {
		t.Fatal(err)
	}
	stream := &runSetupStream{read: bytes.NewReader(input.Bytes())}

	err := handleWorkspaceRunConnection(context.Background(), stream, Config{}, slogDiscard(), newWaitingRunRegistry(), registry, wire.StreamHeader{
		Type:             wire.StreamTypeWorkspaceRun,
		RunID:            "run-1",
		WorkspaceID:      "workspace-1",
		WorkspaceMountID: "mat-1",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if stream.read.Len() != 0 {
		t.Fatalf("unread bytes = %d", stream.read.Len())
	}
	stderr, complete := readGuestdFailureEvents(t, &stream.written)
	if !strings.Contains(stderr, "adapter bundle path is required") || complete.ExitCode != 1 {
		t.Fatalf("stderr = %q complete = %+v", stderr, complete)
	}
}

func TestHandleWorkspaceRunConnectionRejectsInvalidChannelToken(t *testing.T) {
	registry := newWorkspaceOperationRegistry()
	registry.register("mat-1", &workspaceMountEntry{
		channelToken:      "channel-token",
		workspaceID:       "workspace-1",
		fencingGeneration: 2,
		cleanup:           func() {},
	})
	var input bytes.Buffer
	if err := frameio.WriteProtoFrame(&input, &workspacev0.WorkspaceOperationEnvelope{
		WorkspaceMountId:  "mat-1",
		WorkspaceId:       "workspace-1",
		ChannelToken:      "wrong-token",
		FencingGeneration: 2,
		WriteLeaseId:      "write-lease-1",
		FencingToken:      "write-token-1",
	}); err != nil {
		t.Fatal(err)
	}
	stream := &runSetupStream{read: bytes.NewReader(input.Bytes())}

	err := handleWorkspaceRunConnection(context.Background(), stream, Config{}, slogDiscard(), newWaitingRunRegistry(), registry, wire.StreamHeader{
		Type:             wire.StreamTypeWorkspaceRun,
		RunID:            "run-1",
		WorkspaceID:      "workspace-1",
		WorkspaceMountID: "mat-1",
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	stderr, complete := readGuestdFailureEvents(t, &stream.written)
	if !strings.Contains(stderr, "workspace run channel token or fencing generation is invalid") || complete.ExitCode != 1 {
		t.Fatalf("stderr = %q complete = %+v", stderr, complete)
	}
}

func TestRestoreRunWorkspaceArtifactReplacesImageWorkspace(t *testing.T) {
	imageRoot := t.TempDir()
	workspaceRoot := filepath.Join(imageRoot, "workspace")
	if err := os.MkdirAll(filepath.Join(workspaceRoot, "node_modules", "dep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "node_modules", "dep", "package.json"), []byte(`{"name":"dep"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	artifact := testTar(t, []byte("marker"), &tar.Header{Name: "marker.txt", Mode: 0o644, Size: int64(len("marker"))})
	request := testRunTaskRequest()
	request.Workspace.Artifact.Digest = sha256sum.DigestBytes(artifact)
	request.Workspace.Artifact.SizeBytes = uint64(len(artifact))
	request.Workspace.Artifact.EntryCount = 1

	if err := restoreRunWorkspaceArtifact(bytes.NewReader(artifact), request, workspaceRoot, uint64(len(artifact))); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, filepath.Join(workspaceRoot, "marker.txt")); got != "marker" {
		t.Fatalf("marker = %q", got)
	}
	if _, err := os.Stat(filepath.Join(workspaceRoot, "node_modules", "dep", "package.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("image workspace content leaked into restored workspace: %v", err)
	}
}

func TestReplaceWorkspaceRootRollsBackWhenStagingInstallFails(t *testing.T) {
	parent := t.TempDir()
	workspaceRoot := filepath.Join(parent, "workspace")
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "old.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	missingStaging := filepath.Join(parent, "missing-staging")

	if err := replaceWorkspaceRoot(workspaceRoot, missingStaging); err == nil {
		t.Fatal("expected install failure")
	}
	if got := readText(t, filepath.Join(workspaceRoot, "old.txt")); got != "old" {
		t.Fatalf("workspace was not rolled back, old.txt = %q", got)
	}
}

type runSetupStream struct {
	read    *bytes.Reader
	written bytes.Buffer
}

func (s *runSetupStream) Read(p []byte) (int, error) {
	return s.read.Read(p)
}

func (s *runSetupStream) Write(p []byte) (int, error) {
	return s.written.Write(p)
}

func TestImageModeEnvUsesImageEnvAndRuntimeDefaults(t *testing.T) {
	env := imageRuntimeEnv(ociRuntimeConfig{Env: []string{"FOO=bar"}}, &resolvedRuntimeUser{Name: "helmr", UID: 1000, GID: 1000, Home: "/home/helmr"}, "/workspace")
	if got := envValue(env, "PATH"); got != defaultRuntimePath {
		t.Fatalf("PATH = %q", got)
	}
	if got := envValue(env, "HOME"); got != "/home/helmr" {
		t.Fatalf("HOME = %q", got)
	}
	if got := envValue(env, "FOO"); got != "bar" {
		t.Fatalf("FOO = %q", got)
	}
	if got := envValue(env, "PWD"); got != "/workspace" {
		t.Fatalf("PWD = %q", got)
	}
}

func TestImageModeEnvPreservesImagePathAndHome(t *testing.T) {
	env := imageRuntimeEnv(ociRuntimeConfig{Env: []string{"PATH=/custom/bin", "HOME=/custom/home"}}, &resolvedRuntimeUser{Name: "helmr", UID: 1000, GID: 1000, Home: "/home/helmr"}, "/workspace")
	if got := envValue(env, "PATH"); got != "/custom/bin" {
		t.Fatalf("PATH = %q", got)
	}
	if got := envValue(env, "HOME"); got != "/custom/home" {
		t.Fatalf("HOME = %q", got)
	}
}

func TestImageModeEnvDropsDynamicLoaderEnv(t *testing.T) {
	env := imageRuntimeEnv(ociRuntimeConfig{Env: []string{
		"LD_PRELOAD=/image/lib/libhook.so",
		"LD_LIBRARY_PATH=/image/lib",
		"LD_CUSTOM=value",
		"FOO=bar",
	}}, &resolvedRuntimeUser{Name: "helmr", UID: 1000, GID: 1000, Home: "/home/helmr"}, "/workspace")
	for _, key := range []string{"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_CUSTOM"} {
		if got := envValue(env, key); got != "" {
			t.Fatalf("%s = %q", key, got)
		}
	}
	if got := envValue(env, "FOO"); got != "bar" {
		t.Fatalf("FOO = %q", got)
	}
}

func TestAdapterDependenciesInstalledInImageFindsAncestorNodeModules(t *testing.T) {
	imageRoot := t.TempDir()
	sourceRoot := filepath.Join(imageRoot, "workspace", ".helmr", "deployment-source")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "package.json"), []byte(`{"dependencies":{"@helmr/sdk":"latest","zod":"latest"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, root := range map[string]string{
		"@helmr/sdk": sourceRoot,
		"zod":        filepath.Join(imageRoot, "workspace"),
	} {
		modulePath := filepath.Join(root, "node_modules", filepath.FromSlash(name))
		if err := os.MkdirAll(modulePath, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(modulePath, "package.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateAdapterDependenciesInstalledInImage(sourceRoot, imageRoot); err != nil {
		t.Fatal(err)
	}
}

func TestAdapterDependenciesInstalledInImageRequiresAllDependencies(t *testing.T) {
	imageRoot := t.TempDir()
	sourceRoot := filepath.Join(imageRoot, "workspace", ".helmr", "deployment-source")
	if err := os.MkdirAll(sourceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "package.json"), []byte(`{"dependencies":{"@helmr/sdk":"latest","zod":"latest"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	modulePath := filepath.Join(imageRoot, "workspace", "node_modules", "@helmr", "sdk")
	if err := os.MkdirAll(modulePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modulePath, "package.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := validateAdapterDependenciesInstalledInImage(sourceRoot, imageRoot)
	if err == nil {
		t.Fatal("expected incomplete dependency error")
	}
	if !strings.Contains(err.Error(), "task project dependencies are not installed in the sandbox image") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "zod") || strings.Contains(err.Error(), "@helmr/sdk") {
		t.Fatalf("err = %v", err)
	}
}

func TestImageNodeRuntimeCommandUsesNodeFromImagePath(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "custom/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "custom/bin/node"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path, err := imageNodeRuntimeCommand(root, ociRuntimeConfig{Env: []string{"PATH=/custom/bin:/usr/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/custom/bin/node" {
		t.Fatalf("path = %q", path)
	}
}

func TestImageNodeRuntimeCommandRequiresNodeInImagePath(t *testing.T) {
	_, err := imageNodeRuntimeCommand(t.TempDir(), ociRuntimeConfig{})
	if err == nil || !strings.Contains(err.Error(), "task image must provide an executable node in PATH") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadConnectionStartAcceptsResumeAttach(t *testing.T) {
	var stream bytes.Buffer
	if err := frameio.WriteProtoFrame(&stream, &runv0.ResumeAttach{
		CheckpointId: "checkpoint-1",
		RunWaitId:    "run-wait-id-1",
		RunLeaseId:   "execution-1",
	}); err != nil {
		t.Fatal(err)
	}
	start, err := readConnectionStart(&stream)
	if err != nil {
		t.Fatal(err)
	}
	if start.attach == nil || start.attach.CheckpointId != "checkpoint-1" || start.attach.RunWaitId != "run-wait-id-1" {
		t.Fatalf("start = %+v", start)
	}
}

func TestReadConnectionStartAcceptsStreamHeader(t *testing.T) {
	var stream bytes.Buffer
	if err := wire.WriteStreamFrameHeader(&stream, wire.StreamHeader{Type: wire.StreamTypeRunImage, RunID: "run-1"}, 5); err != nil {
		t.Fatal(err)
	}
	stream.WriteString("hello")
	start, err := readConnectionStart(&stream)
	if err != nil {
		t.Fatal(err)
	}
	if start.streamHeader.Type != wire.StreamTypeRunImage || start.streamHeader.RunID != "run-1" || start.bodyLen != 5 {
		t.Fatalf("start = %+v", start)
	}
	body := make([]byte, start.bodyLen)
	if _, err := io.ReadFull(&stream, body); err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q", body)
	}
}

func TestReadConnectionStartAcceptsLargeStreamBody(t *testing.T) {
	var stream bytes.Buffer
	if err := wire.WriteStreamFrameHeader(&stream, wire.StreamHeader{Type: wire.StreamTypeRunImage, RunID: "run-1"}, frameio.MaxFrameBytes+1); err != nil {
		t.Fatal(err)
	}
	start, err := readConnectionStart(&stream)
	if err != nil {
		t.Fatal(err)
	}
	if start.streamHeader.Type != wire.StreamTypeRunImage || start.streamHeader.RunID != "run-1" {
		t.Fatalf("header = %+v", start.streamHeader)
	}
	if start.bodyLen != uint64(frameio.MaxFrameBytes+1) {
		t.Fatalf("bodyLen = %d", start.bodyLen)
	}
}

func TestExtractTarRejectsUnsafeEntries(t *testing.T) {
	tests := []struct {
		name    string
		headers []*tar.Header
		body    []byte
		want    string
	}{
		{
			name: "absolute path",
			headers: []*tar.Header{{
				Name: "/escape.txt",
				Mode: 0o644,
				Size: 1,
			}},
			body: []byte("x"),
			want: "unsafe tar path",
		},
		{
			name: "parent path",
			headers: []*tar.Header{{
				Name: "../escape.txt",
				Mode: 0o644,
				Size: 1,
			}},
			body: []byte("x"),
			want: "unsafe tar path",
		},
		{
			name: "parent component",
			headers: []*tar.Header{{
				Name: "dir/../escape.txt",
				Mode: 0o644,
				Size: 1,
			}},
			body: []byte("x"),
			want: "unsafe tar path",
		},
		{
			name: "hardlink",
			headers: []*tar.Header{{
				Name:     "link",
				Linkname: "target",
				Typeflag: tar.TypeLink,
			}},
			want: "hardlink",
		},
		{
			name: "device",
			headers: []*tar.Header{{
				Name:     "device",
				Typeflag: tar.TypeChar,
			}},
			want: "device",
		},
		{
			name: "absolute symlink",
			headers: []*tar.Header{{
				Name:     "link",
				Linkname: "/etc/passwd",
				Typeflag: tar.TypeSymlink,
			}},
			want: "unsafe symlink target",
		},
		{
			name: "escaping symlink",
			headers: []*tar.Header{{
				Name:     "dir/link",
				Linkname: "../../escape",
				Typeflag: tar.TypeSymlink,
			}},
			want: "unsafe symlink target",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := archive.ExtractTar(bytes.NewReader(testTar(t, tt.body, tt.headers...)), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestExtractTarRejectsParentSymlinkTraversal(t *testing.T) {
	dst := t.TempDir()
	body := testTar(t, []byte("x"),
		&tar.Header{Name: "link", Linkname: "safe", Typeflag: tar.TypeSymlink},
		&tar.Header{Name: "link/file.txt", Mode: 0o644, Size: 1},
	)
	err := archive.ExtractTar(bytes.NewReader(body), dst)
	if err == nil || !strings.Contains(err.Error(), "unsafe tar parent") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "safe", "file.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file escaped through symlink, stat err = %v", err)
	}
}

func TestExtractTarIgnoresRootDirectoryEntry(t *testing.T) {
	dst := t.TempDir()
	body := testTar(t, []byte("inside"),
		&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "./file.txt", Mode: 0o644, Size: 6},
	)
	if err := archive.ExtractTar(bytes.NewReader(body), dst); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dst, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "inside" {
		t.Fatalf("file content = %q", content)
	}
}

func TestExtractTarReplacesSymlinkWithRegularFileWithoutFollowing(t *testing.T) {
	dst := t.TempDir()
	target := filepath.Join(dst, "outside.txt")
	if err := os.WriteFile(target, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("outside.txt", filepath.Join(dst, "file.txt")); err != nil {
		t.Fatal(err)
	}
	err := archive.ExtractTar(bytes.NewReader(testTar(t, []byte("inside"), &tar.Header{
		Name: "file.txt",
		Mode: 0o644,
		Size: 6,
	})), dst)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "outside" {
		t.Fatalf("followed existing symlink, target = %q", content)
	}
	content, err = os.ReadFile(filepath.Join(dst, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "inside" {
		t.Fatalf("file content = %q", content)
	}
}

func TestHandleCompileTaskBundleReportsTarExtractionError(t *testing.T) {
	body := testTar(t, nil, &tar.Header{
		Name:     "link",
		Linkname: "/etc/passwd",
		Typeflag: tar.TypeSymlink,
	})
	stream := &bufferConn{reader: bytes.NewReader(body)}
	err := handleCompileTaskBundle(context.Background(), stream, Config{}, wire.StreamHeader{
		Type:   wire.StreamTypeCompileTaskBundle,
		RunID:  "run-1",
		TaskID: "task-1",
	}, uint64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	frame, err := frameio.ReadMessageFrame(&stream.Buffer)
	if err != nil {
		t.Fatal(err)
	}
	parseErr, ok, err := frameio.DecodeParseErrorFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || parseErr.Kind != "bad_request" || !strings.Contains(parseErr.Message, "unsafe symlink target") {
		t.Fatalf("parse error = %+v ok=%v", parseErr, ok)
	}
}

func TestHandleCatalogDeploymentReportsPackageValidationError(t *testing.T) {
	body := testTar(t, []byte("export default { dirs: ['tasks'] }\n"), &tar.Header{
		Name: "helmr.config.ts",
		Mode: 0o644,
		Size: int64(len("export default { dirs: ['tasks'] }\n")),
	})
	stream := &bufferConn{reader: bytes.NewReader(body)}
	err := handleCatalogDeployment(context.Background(), stream, Config{}, wire.StreamHeader{
		Type:  wire.StreamTypeCatalogDeployment,
		RunID: "deployment-1",
	}, uint64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	frame, err := frameio.ReadMessageFrame(&stream.Buffer)
	if err != nil {
		t.Fatal(err)
	}
	parseErr, ok, err := frameio.DecodeParseErrorFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || parseErr.Kind != "bad_request" || !strings.Contains(parseErr.Message, "package.json is required for Helmr task projects") {
		t.Fatalf("parse error = %+v ok=%v", parseErr, ok)
	}
}

func TestHandleRunConnectionReportsImageExtractionError(t *testing.T) {
	body := []byte("not an oci image")
	stream := &bufferConn{reader: bytes.NewReader(body)}
	err := handleRunConnection(context.Background(), stream, Config{}, slogDiscard(), newWaitingRunRegistry(), wire.StreamHeader{
		Type:  wire.StreamTypeRunImage,
		RunID: "run-1",
	}, uint64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	stderr, complete := readGuestdFailureEvents(t, &stream.Buffer)
	if !strings.Contains(stderr, "unpack run image") {
		t.Fatalf("stderr = %q", stderr)
	}
	if complete.ExitCode != 1 || complete.ErrorMessage == nil || !strings.Contains(*complete.ErrorMessage, "unpack run image") {
		t.Fatalf("complete = %+v", complete)
	}
}

type bufferConn struct {
	reader *bytes.Reader
	bytes.Buffer
}

func (c *bufferConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

func TestRunAdapterResumesOnAttachedStream(t *testing.T) {
	t.Setenv("HELMR_GUESTD_HELPER", "resume-handoff")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	originalGuest, originalHost := net.Pipe()
	defer originalGuest.Close()
	defer originalHost.Close()
	attachedGuest, attachedHost := net.Pipe()
	defer attachedGuest.Close()
	defer attachedHost.Close()

	deadline := time.Now().Add(15 * time.Second)
	if err := originalHost.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := attachedHost.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}

	registry := newWaitingRunRegistry()
	tempDir, runner := guestAdapterHelperRunner(t, "resume-handoff")
	errCh := make(chan error, 1)
	go func() {
		errCh <- runAdapter(ctx, originalGuest, Config{
			AdapterRuntimePath: runner,
			AdapterPath:        "-test.run=TestGuestAdapterHelperProcess",
		}, tempDir, tempDir, tempDir, tempDir, ociRuntimeConfig{}, false, testRunTaskRequest(), registry)
	}()

	event, err := wire.ReadRunEvent(originalHost)
	if err != nil {
		t.Fatal(err)
	}
	if event.GetRunWaitRequested() == nil {
		t.Fatalf("first event = %+v", event)
	}
	writeSuspendAndReadReady(t, originalHost, "run-wait-id-1", "checkpoint-1")
	if err := originalHost.Close(); err != nil {
		t.Fatal(err)
	}
	if err := registry.attach("run-wait-id-1", "checkpoint-1", attachedGuest); err != nil {
		t.Fatal(err)
	}
	if err := frameio.WriteProtoFrame(attachedHost, &runv0.ResumeDecision{
		RunWaitId:          "run-wait-id-1",
		Kind:               "completed",
		DataJson:           "{}",
		RequireConsumedAck: true,
	}); err != nil {
		t.Fatal(err)
	}
	var ack runv0.ResumeAck
	if err := frameio.ReadProtoFrame(attachedHost, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.RunWaitId != "run-wait-id-1" {
		t.Fatalf("ack = %+v", &ack)
	}

	var stdout strings.Builder
	var completed bool
	for !completed {
		event, err := wire.ReadRunEvent(attachedHost)
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StdoutChunk:
			stdout.WriteString(string(value.StdoutChunk))
		case *runv0.RunEvent_TaskResult:
			completed = true
			if value.TaskResult.ExitCode != 0 {
				t.Fatalf("exit code = %d message=%v", value.TaskResult.ExitCode, value.TaskResult.ErrorMessage)
			}
		}
	}
	if !strings.Contains(stdout.String(), "after-resume") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestRunAdapterReadsNextCheckpointSuspendFromAttachedStream(t *testing.T) {
	t.Setenv("HELMR_GUESTD_HELPER", "resume-handoff-twice")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	originalGuest, originalHost := net.Pipe()
	defer originalGuest.Close()
	defer originalHost.Close()
	firstGuest, firstHost := net.Pipe()
	defer firstGuest.Close()
	defer firstHost.Close()
	secondGuest, secondHost := net.Pipe()
	defer secondGuest.Close()
	defer secondHost.Close()
	originalReader := bufio.NewReader(originalHost)
	firstReader := bufio.NewReader(firstHost)
	secondReader := bufio.NewReader(secondHost)

	deadline := time.Now().Add(30 * time.Second)
	for _, conn := range []net.Conn{originalHost, firstHost, secondHost} {
		if err := conn.SetDeadline(deadline); err != nil {
			t.Fatal(err)
		}
	}

	registry := newWaitingRunRegistry()
	tempDir, runner := guestAdapterHelperRunner(t, "resume-handoff-twice")
	errCh := make(chan error, 1)
	go func() {
		errCh <- runAdapter(ctx, originalGuest, Config{
			AdapterRuntimePath: runner,
			AdapterPath:        "-test.run=TestGuestAdapterHelperProcess",
		}, tempDir, tempDir, tempDir, tempDir, ociRuntimeConfig{}, false, testRunTaskRequest(), registry)
	}()

	readRunWaitRequestedFrom(t, originalReader)
	writeSuspendAndReadReadyFrom(t, originalHost, originalReader, "run-wait-id-1", "checkpoint-1")
	if err := registry.attach("run-wait-id-1", "checkpoint-1", firstGuest); err != nil {
		t.Fatal(err)
	}
	writeDecisionAndReadAckFrom(t, firstHost, firstReader, "run-wait-id-1", "completed")

	readRunWaitRequestedFrom(t, firstReader)
	writeSuspendAndReadReadyFrom(t, firstHost, firstReader, "run-wait-id-2", "checkpoint-2")
	if err := registry.attach("run-wait-id-2", "checkpoint-2", secondGuest); err != nil {
		t.Fatal(err)
	}
	writeDecisionAndReadAckFrom(t, secondHost, secondReader, "run-wait-id-2", "completed")

	var stdout strings.Builder
	var completed bool
	for !completed {
		event, err := wire.ReadRunEvent(secondReader)
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StdoutChunk:
			stdout.WriteString(string(value.StdoutChunk))
		case *runv0.RunEvent_TaskResult:
			completed = true
			if value.TaskResult.ExitCode != 0 {
				t.Fatalf("exit code = %d message=%v", value.TaskResult.ExitCode, value.TaskResult.ErrorMessage)
			}
		}
	}
	if !strings.Contains(stdout.String(), "after-second") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestReadResumeDecisionTimesOut(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := readResumeDecision(ctx, reader)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestAdapterRunStreamTimesOutWaitingForResumeConsumed(t *testing.T) {
	var out bytes.Buffer
	stream := newAdapterRunStream(&out)
	stream.resumeAckPending = true
	stream.resumeAckRunWait = "run-wait-id-1"
	stream.resumeAckDeadline = time.Now().Add(-time.Millisecond)
	err := stream.writeEvent(&runv0.RunEvent{
		Event: &runv0.RunEvent_LogEntry{LogEntry: "blocked"},
	})
	if err == nil || !strings.Contains(err.Error(), "was not acknowledged before timeout") {
		t.Fatalf("err = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("event was written despite timed out resume ack gate: %d bytes", out.Len())
	}
}

func readRunWaitRequestedFrom(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	_ = readRunWaitRequestedEvent(t, reader)
}

func readRunWaitRequestedEvent(t *testing.T, conn io.Reader) *runv0.RunWaitRequested {
	t.Helper()
	for {
		event, err := wire.ReadRunEvent(conn)
		if err != nil {
			t.Fatal(err)
		}
		if wait := event.GetRunWaitRequested(); wait != nil {
			return wait
		}
	}
}

func writeSuspendAndReadReady(t *testing.T, conn io.ReadWriter, runWaitID string, checkpointID string) {
	t.Helper()
	if err := wire.WriteCheckpointPauseRequest(conn, &runv0.CheckpointPauseRequest{
		RunWaitId:    runWaitID,
		CheckpointId: checkpointID,
	}); err != nil {
		t.Fatal(err)
	}
	readCheckpointPauseReady(t, conn, runWaitID, checkpointID)
}

func writeSuspendAndReadReadyFrom(t *testing.T, conn io.Writer, reader *bufio.Reader, runWaitID string, checkpointID string) {
	t.Helper()
	if err := wire.WriteCheckpointPauseRequest(conn, &runv0.CheckpointPauseRequest{
		RunWaitId:    runWaitID,
		CheckpointId: checkpointID,
	}); err != nil {
		t.Fatal(err)
	}
	readCheckpointPauseReadyFrom(t, reader, runWaitID, checkpointID)
}

func readCheckpointPauseReady(t *testing.T, conn io.Reader, runWaitID string, checkpointID string) {
	t.Helper()
	reader := bufio.NewReader(conn)
	readCheckpointPauseReadyFrom(t, reader, runWaitID, checkpointID)
}

func readCheckpointPauseReadyFrom(t *testing.T, reader *bufio.Reader, runWaitID string, checkpointID string) {
	t.Helper()
	for {
		prefix, err := reader.Peek(4)
		if err != nil {
			t.Fatal(err)
		}
		if frameio.IsStreamFramePrefix(prefix) {
			header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
			if err != nil {
				t.Fatal(err)
			}
			if header.Type != wire.StreamTypeCheckpointPauseReady || header.RunWaitID != runWaitID || header.CheckpointID != checkpointID || bodyLen != 0 {
				t.Fatalf("pause ready = %+v bodyLen=%d, want run wait=%s checkpoint=%s", header, bodyLen, runWaitID, checkpointID)
			}
			return
		}
		body, err := frameio.ReadMessageFrame(reader)
		if err != nil {
			t.Fatal(err)
		}
		var event runv0.RunEvent
		if err := proto.Unmarshal(body, &event); err != nil {
			t.Fatalf("unmarshal interleaved run event: %v", err)
		}
	}
}

func writeDecisionAndReadAckFrom(t *testing.T, writer io.Writer, reader *bufio.Reader, runWaitID string, kind string) {
	t.Helper()
	if err := frameio.WriteProtoFrame(writer, &runv0.ResumeDecision{
		RunWaitId:          runWaitID,
		Kind:               kind,
		DataJson:           "{}",
		RequireConsumedAck: true,
	}); err != nil {
		t.Fatal(err)
	}
	var ack runv0.ResumeAck
	if err := frameio.ReadProtoFrame(reader, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.RunWaitId != runWaitID {
		t.Fatalf("ack = %+v, want run wait=%s", &ack, runWaitID)
	}
}

func runGuestAdapterHelperProcess() int {
	switch os.Getenv("HELMR_GUESTD_HELPER") {
	case "resume-handoff":
	case "resume-handoff-twice":
	case "stdout-stderr":
		control, err := helperControlWriter()
		if err != nil {
			return 2
		}
		fmt.Println("stdout-line")
		fmt.Fprintln(os.Stderr, "stderr-line")
		time.Sleep(50 * time.Millisecond)
		return writeHelperTaskResult(control, 0, "")
	case "stdout-stderr-tail":
		control, err := helperControlWriter()
		if err != nil {
			return 2
		}
		fmt.Print(strings.Repeat("stdout-block\n", 8192))
		fmt.Println("stdout-tail")
		fmt.Fprint(os.Stderr, strings.Repeat("stderr-block\n", 8192))
		fmt.Fprintln(os.Stderr, "stderr-tail")
		return writeHelperTaskResult(control, 0, "")
	case "stdout-json":
		control, err := helperControlWriter()
		if err != nil {
			return 2
		}
		fmt.Print(`{"result":{"ok":true,"count":2}}`)
		return writeHelperTaskResult(control, 0, "")
	case "stdout-json-exit-3":
		control, err := helperControlWriter()
		if err != nil {
			return 2
		}
		fmt.Print(`{"result":{"ok":false}}`)
		return writeHelperTaskResult(control, 3, "")
	case "task-output-fd-holder":
		control, err := helperControlWriter()
		if err != nil {
			return 2
		}
		if err := startFDHolderChild(control); err != nil {
			fmt.Fprintln(os.Stderr, err)
			_ = control.Close()
			return 2
		}
		outputJSON := `{"ok":true}`
		if err := frameio.WriteProtoFrame(control, &runv0.RunEvent{
			Event: &runv0.RunEvent_TaskResult{TaskResult: &runv0.TaskResult{
				ExitCode:   0,
				OutputJson: &outputJSON,
			}},
		}); err != nil {
			_ = control.Close()
			return 2
		}
		fmt.Println("supervisor-exiting")
		fmt.Fprintln(os.Stderr, "supervisor-stderr")
		_ = control.Close()
		return 0
	case "no-result-fd-holder":
		control, err := helperControlWriter()
		if err != nil {
			return 2
		}
		if err := startFDHolderChild(control); err != nil {
			fmt.Fprintln(os.Stderr, err)
			_ = control.Close()
			return 2
		}
		fmt.Println("supervisor-exiting-without-result")
		_ = control.Close()
		return 0
	case "task-result-after-blocked-control-event":
		control, err := helperControlWriter()
		if err != nil {
			return 2
		}
		if err := frameio.WriteProtoFrame(control, &runv0.RunEvent{
			Event: &runv0.RunEvent_StdoutChunk{StdoutChunk: []byte("blocked-before-result\n")},
		}); err != nil {
			_ = control.Close()
			return 2
		}
		outputJSON := `{"late":true}`
		if err := frameio.WriteProtoFrame(control, &runv0.RunEvent{
			Event: &runv0.RunEvent_TaskResult{TaskResult: &runv0.TaskResult{
				ExitCode:   0,
				OutputJson: &outputJSON,
			}},
		}); err != nil {
			_ = control.Close()
			return 2
		}
		_ = control.Close()
		return 0
	case "hold-fds-child":
		return holdFDsUntilReleased()
	case "malformed-control":
		control, err := helperControlWriter()
		if err != nil {
			return 2
		}
		_, _ = control.Write([]byte{0, 0, 0, 1, 0xff})
		_ = control.Close()
		return 0
	case "malformed-control-exit-42":
		control, err := helperControlWriter()
		if err != nil {
			return 2
		}
		_, _ = control.Write([]byte{0, 0, 0, 1, 0xff})
		_ = control.Close()
		return 42
	case "wait-control-only":
		control, err := helperControlWriter()
		if err != nil {
			return 2
		}
		if err := frameio.WriteProtoFrame(control, &runv0.RunEvent{
			Event: &runv0.RunEvent_RunWaitRequested{RunWaitRequested: &runv0.RunWaitRequested{
				CorrelationId: "approval-1",
				Kind:          "token",
				ParamsJson:    `{}`,
			}},
		}); err != nil {
			return 2
		}
		_ = control.Close()
		return 0
	case "active-stream-read-control":
		control, err := helperControlWriter()
		if err != nil {
			return 2
		}
		if err := frameio.WriteProtoFrame(control, &runv0.RunEvent{
			Event: &runv0.RunEvent_ActiveStreamReadRequested{ActiveStreamReadRequested: &runv0.ActiveStreamReadRequested{
				CorrelationId: "active-read-1",
				Stream:        "inbox",
				AfterSequence: 2,
				Block:         false,
			}},
		}); err != nil {
			_ = control.Close()
			return 2
		}
		var result runv0.ActiveStreamReadResult
		if err := frameio.ReadProtoFrame(os.Stdin, &result); err != nil {
			fmt.Fprintln(os.Stderr, err)
			_ = control.Close()
			return 2
		}
		outputJSON := fmt.Sprintf(`{"timedOut":%t}`, result.GetTimedOut())
		return writeHelperTaskResult(control, 0, outputJSON)
	case "wait-all-control":
		control, err := helperControlWriter()
		if err != nil {
			return 2
		}
		for index, kind := range []string{"timer", "channel"} {
			if index == 1 {
				time.Sleep(200 * time.Millisecond)
			}
			if err := frameio.WriteProtoFrame(control, &runv0.RunEvent{
				Event: &runv0.RunEvent_RunWaitRequested{RunWaitRequested: &runv0.RunWaitRequested{
					CorrelationId: "run-wait-1",
					Kind:          kind,
					ParamsJson:    `{}`,
				}},
			}); err != nil {
				_ = control.Close()
				return 2
			}
		}
		var decision runv0.ResumeDecision
		if err := frameio.ReadProtoFrame(os.Stdin, &decision); err != nil {
			_ = control.Close()
			return 2
		}
		if err := writeHelperResumeConsumed(control, decision.RunWaitId); err != nil {
			_ = control.Close()
			return 2
		}
		return writeHelperTaskResult(control, 0, "")
	default:
		return 2
	}
	control, err := helperControlWriter()
	if err != nil {
		return 2
	}
	if err := frameio.WriteProtoFrame(control, &runv0.RunEvent{
		Event: &runv0.RunEvent_RunWaitRequested{RunWaitRequested: &runv0.RunWaitRequested{
			CorrelationId: "approval-1",
			Kind:          "token",
			ParamsJson:    `{}`,
		}},
	}); err != nil {
		return 2
	}
	var decision runv0.ResumeDecision
	if err := frameio.ReadProtoFrame(os.Stdin, &decision); err != nil {
		return 2
	}
	if err := writeHelperResumeConsumed(control, decision.RunWaitId); err != nil {
		return 2
	}
	fmt.Println("after-first")
	if os.Getenv("HELMR_GUESTD_HELPER") == "resume-handoff-twice" {
		if err := frameio.WriteProtoFrame(control, &runv0.RunEvent{
			Event: &runv0.RunEvent_RunWaitRequested{RunWaitRequested: &runv0.RunWaitRequested{
				CorrelationId: "message-1",
				Kind:          "token",
				ParamsJson:    `{}`,
			}},
		}); err != nil {
			return 2
		}
		if err := frameio.ReadProtoFrame(os.Stdin, &decision); err != nil {
			return 2
		}
		if err := writeHelperResumeConsumed(control, decision.RunWaitId); err != nil {
			return 2
		}
		fmt.Println("after-second")
		return writeHelperTaskResult(control, 0, "")
	}
	fmt.Println("after-resume")
	return writeHelperTaskResult(control, 0, "")
}

func writeHelperResumeConsumed(control io.Writer, runWaitID string) error {
	return frameio.WriteProtoFrame(control, &runv0.RunEvent{
		Event: &runv0.RunEvent_ResumeConsumed{ResumeConsumed: &runv0.ResumeConsumed{
			RunWaitId: runWaitID,
		}},
	})
}

func writeHelperTaskResult(control io.WriteCloser, exitCode int32, outputJSON string) int {
	defer control.Close()
	result := &runv0.TaskResult{ExitCode: exitCode}
	if outputJSON != "" {
		result.OutputJson = &outputJSON
	}
	if err := frameio.WriteProtoFrame(control, &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskResult{TaskResult: result},
	}); err != nil {
		return 2
	}
	return int(exitCode)
}

func startFDHolderChild(control io.WriteCloser) error {
	controlConn, ok := control.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("control writer is %T, want *net.UnixConn", control)
	}
	controlFile, err := controlConn.File()
	if err != nil {
		return fmt.Errorf("duplicate control fd: %w", err)
	}
	defer controlFile.Close()

	releasePath := filepath.Join(os.Getenv("HELMR_GUESTD_HELPER_DIR"), "release-fd-holder")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		"HELMR_GUESTD_HELPER=hold-fds-child",
		"HELMR_GUESTD_FD_HOLDER_RELEASE="+releasePath,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{controlFile}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start fd holder child: %w", err)
	}
	return nil
}

func holdFDsUntilReleased() int {
	releasePath := os.Getenv("HELMR_GUESTD_FD_HOLDER_RELEASE")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if releasePath != "" {
			if _, err := os.Stat(releasePath); err == nil {
				return 0
			}
		}
		if time.Now().After(deadline) {
			return 0
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func helperControlWriter() (io.WriteCloser, error) {
	socketPath := strings.TrimSpace(os.Getenv("HELMR_CONTROL_SOCKET"))
	if socketPath == "" {
		return nil, errors.New("HELMR_CONTROL_SOCKET is required")
	}
	return net.Dial("unix", socketPath)
}

func testTar(t *testing.T, body []byte, headers ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	for _, header := range headers {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if int64(len(body)) < header.Size {
				t.Fatalf("body size %d < header size %d", len(body), header.Size)
			}
			if _, err := writer.Write(body[:header.Size]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeTestTaskProjectPackage(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"dependencies":{"@helmr/sdk":"latest"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "@helmr", "sdk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "@helmr", "sdk", "package.json"), []byte(`{"name":"@helmr/sdk"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func guestAdapterHelperRunner(t *testing.T, helper string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	writeTestTaskProjectPackage(t, dir)
	runner := filepath.Join(dir, "helper-runner.sh")
	script := fmt.Sprintf(`#!/bin/sh
HELMR_GUESTD_HELPER=%s HELMR_GUESTD_HELPER_DIR=%s exec %s "$@"
`, shellQuote(helper), shellQuote(dir), shellQuote(os.Args[0]))
	if err := os.WriteFile(runner, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, runner
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func tailString(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[len(value)-limit:]
}

func envValue(env []string, key string) string {
	for _, entry := range env {
		entryKey, value, ok := strings.Cut(entry, "=")
		if ok && entryKey == key {
			return value
		}
	}
	return ""
}

func envKeyCount(env []string, key string) int {
	count := 0
	for _, entry := range env {
		entryKey, _, ok := strings.Cut(entry, "=")
		if ok && entryKey == key {
			count++
		}
	}
	return count
}

func readGuestdFailureEvents(t *testing.T, stream io.Reader) (string, *runv0.TaskResult) {
	t.Helper()
	var stderr strings.Builder
	for {
		event, err := wire.ReadRunEvent(stream)
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StderrChunk:
			stderr.WriteString(string(value.StderrChunk))
		case *runv0.RunEvent_TaskResult:
			return stderr.String(), value.TaskResult
		default:
			t.Fatalf("unexpected event = %+v", event)
		}
	}
}

func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestParseAdapterReturnsBinaryBundle(t *testing.T) {
	tempDir := t.TempDir()
	writeTestTaskProjectPackage(t, tempDir)
	runner := filepath.Join(tempDir, "parser.sh")
	if err := os.WriteFile(runner, []byte("#!/bin/sh\nprintf '\\001\\000\\377'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := parseAdapter(context.Background(), Config{
		AdapterRuntimePath: runner,
		AdapterPath:        "adapter.js",
	}, tempDir, "task")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, []byte{1, 0, 255}) {
		t.Fatalf("body = %v", body)
	}
}

func TestParseAdapterReturnsStructuredParseError(t *testing.T) {
	tempDir := t.TempDir()
	writeTestTaskProjectPackage(t, tempDir)
	runner := filepath.Join(tempDir, "parser.sh")
	if err := os.WriteFile(runner, []byte("#!/bin/sh\nif [ \"$1\" = install ]; then exit 0; fi\nprintf '%s\\n' '{\"level\":\"error\",\"kind\":\"task_not_found\",\"message\":\"task not found: deploy\"}' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := parseAdapter(context.Background(), Config{
		AdapterRuntimePath: runner,
		AdapterPath:        "adapter.js",
	}, tempDir, "deploy")
	var parseErr adapterParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("err = %T %[1]v", err)
	}
	if parseErr.Kind != "task_not_found" || parseErr.Message != "task not found: deploy" {
		t.Fatalf("parse err = %+v", parseErr)
	}
}

func TestParseAdapterRequiresTaskProjectPackageJSON(t *testing.T) {
	tempDir := t.TempDir()
	runner := filepath.Join(tempDir, "parser.sh")
	if err := os.WriteFile(runner, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := parseAdapter(context.Background(), Config{
		AdapterRuntimePath: runner,
		AdapterPath:        "adapter.js",
	}, tempDir, "task")
	if err == nil || !strings.Contains(err.Error(), "package.json is required for Helmr task projects") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseAdapterRequiresHelmrSDKDependency(t *testing.T) {
	tempDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tempDir, "package.json"), []byte(`{"packageManager":"bun@1.3.10","dependencies":{"left-pad":"1.3.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runner := filepath.Join(tempDir, "parser.sh")
	if err := os.WriteFile(runner, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := parseAdapter(context.Background(), Config{
		AdapterRuntimePath: runner,
		AdapterPath:        "adapter.js",
	}, tempDir, "task")
	if err == nil || !strings.Contains(err.Error(), "package.json must declare @helmr/sdk in dependencies") {
		t.Fatalf("err = %v", err)
	}
}

func TestWorkspaceMountPathRejectsUnsafeValues(t *testing.T) {
	for _, value := range []string{"/", "workspace", "/../workspace", "/workspace/../other", "/opt/helmr/workspace", "/proc/self"} {
		_, err := workspaceMountPath(&runv0.RunTaskRequest{
			Workspace: &runv0.RunTaskWorkspace{Path: value},
		})
		if err == nil {
			t.Fatalf("workspaceMountPath(%q) error = nil", value)
		}
	}
}

func TestWorkspaceRootForImageRejectsSymlinkMount(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "workspace")); err != nil {
		t.Fatal(err)
	}
	_, err := workspaceRootForImage(root, "/workspace")
	if err == nil {
		t.Fatal("expected symlink workspace rejection")
	}
}

func TestTaskSourceRootIgnoresImageWorkdir(t *testing.T) {
	got, err := taskSourceRoot("/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultTaskSourceRoot {
		t.Fatalf("task source root = %q", got)
	}
}

func TestMaterializeDeploymentSourceForRuntimePlacesSourceOutsideWorkspace(t *testing.T) {
	imageRoot := t.TempDir()
	sourceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sourceRoot, "tasks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "tasks", "task.ts"), []byte("task"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sourceRoot, "node_modules", "dep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "node_modules", "dep", "package.json"), []byte(`{"name":"dep"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(imageRoot, "opt", "helmr-task", "node_modules", "dep"), 0o755); err != nil {
		t.Fatal(err)
	}

	taskCwd, err := materializeDeploymentSourceForRuntime(imageRoot, sourceRoot, defaultTaskSourceRoot, &resolvedRuntimeUser{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())})
	if err != nil {
		t.Fatal(err)
	}

	if taskCwd != "/opt/helmr-task/.helmr/deployment-source" {
		t.Fatalf("task cwd = %q", taskCwd)
	}
	if got := readText(t, filepath.Join(imageRoot, "opt", "helmr-task", ".helmr", "deployment-source", "tasks", "task.ts")); got != "task" {
		t.Fatalf("deployment source = %q", got)
	}
	if _, err := os.Stat(filepath.Join(imageRoot, "opt", "helmr-task", ".helmr", "deployment-source", "node_modules", "dep", "package.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deployment source dependencies leaked into runtime: %v", err)
	}
	if _, err := os.Stat(filepath.Join(imageRoot, "workspace", "node_modules", "dep")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace dependencies leaked into workspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(imageRoot, "opt", "helmr-task", "node_modules", "dep")); err != nil {
		t.Fatalf("runtime dependencies missing: %v", err)
	}
}

func TestApplySecretsRejectsSymlinkParent(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(workspace, "escape")); err != nil {
		t.Fatal(err)
	}
	err := applySecrets(root, workspace, &runv0.RunTaskRequest{
		Secrets: []*runv0.SecretInject{{
			Name:       "token",
			ValueBytes: []byte("secret"),
			Placement:  &runv0.Placement{Kind: &runv0.Placement_File{File: &runv0.FilePlacement{Path: "escape/token"}}},
		}},
	}, nil, new([]string))
	if err == nil {
		t.Fatal("expected symlink parent rejection")
	}
}

func TestApplySecretsEnforcesModeAndRuntimeOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires chown")
	}
	root := guestRootWithUsers(t)
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(workspace, "token")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	mode := "0400"
	err := applySecrets(root, workspace, &runv0.RunTaskRequest{
		Secrets: []*runv0.SecretInject{{
			Name:       "token",
			ValueBytes: []byte("secret"),
			Placement: &runv0.Placement{Kind: &runv0.Placement_File{File: &runv0.FilePlacement{
				Path: "token",
				Mode: &mode,
			}}},
		}},
	}, &resolvedRuntimeUser{Name: "agent", UID: 1001, GID: 1001, Home: "/home/agent"}, new([]string))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o400 {
		t.Fatalf("mode = %#o", info.Mode().Perm())
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != 1001 || stat.Gid != 1001 {
		t.Fatalf("owner = %d:%d", stat.Uid, stat.Gid)
	}
}

func TestApplySecretsHonorsExplicitOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires chown")
	}
	root := guestRootWithUsers(t)
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	owner := "agent"
	err := applySecrets(root, workspace, &runv0.RunTaskRequest{
		Secrets: []*runv0.SecretInject{{
			Name:       "token",
			ValueBytes: []byte("secret"),
			Placement: &runv0.Placement{Kind: &runv0.Placement_File{File: &runv0.FilePlacement{
				Path:  "token",
				Owner: &owner,
			}}},
		}},
	}, nil, new([]string))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(workspace, "token"))
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != 1001 || stat.Gid != 1001 {
		t.Fatalf("owner = %d:%d", stat.Uid, stat.Gid)
	}
}

func TestApplySecretsExplicitOwnerParentsRemainTraversable(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires chown")
	}
	root := guestRootWithUsers(t)
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	owner := "root"
	mode := "0644"
	err := applySecrets(root, workspace, &runv0.RunTaskRequest{
		Secrets: []*runv0.SecretInject{{
			Name:       "token",
			ValueBytes: []byte("secret"),
			Placement: &runv0.Placement{Kind: &runv0.Placement_File{File: &runv0.FilePlacement{
				Path:  "secrets/token",
				Mode:  &mode,
				Owner: &owner,
			}}},
		}},
	}, &resolvedRuntimeUser{Name: "agent", UID: 1001, GID: 1001, Home: "/home/agent"}, new([]string))
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(filepath.Join(workspace, "secrets"))
	if err != nil {
		t.Fatal(err)
	}
	dirStat := dirInfo.Sys().(*syscall.Stat_t)
	if dirStat.Uid != 1001 || dirStat.Gid != 1001 {
		t.Fatalf("dir owner = %d:%d", dirStat.Uid, dirStat.Gid)
	}
	fileInfo, err := os.Stat(filepath.Join(workspace, "secrets", "token"))
	if err != nil {
		t.Fatal(err)
	}
	fileStat := fileInfo.Sys().(*syscall.Stat_t)
	if fileStat.Uid != 0 || fileStat.Gid != 0 {
		t.Fatalf("file owner = %d:%d", fileStat.Uid, fileStat.Gid)
	}
}

func TestWorkspaceArtifactExcludesWorkspaceRelativeSecrets(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "public.txt"), []byte("public"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := []string{}
	secretPaths, err := applySecretsWithWorkspacePaths(root, workspaceRoot, &runv0.RunTaskRequest{
		Secrets: []*runv0.SecretInject{
			{
				Name:       "file-token",
				ValueBytes: []byte("file-secret"),
				Placement: &runv0.Placement{Kind: &runv0.Placement_File{File: &runv0.FilePlacement{
					Path: "secrets/token",
				}}},
			},
			{
				Name:       "dir-token",
				ValueBytes: []byte(""),
				Placement: &runv0.Placement{Kind: &runv0.Placement_Dir{Dir: &runv0.DirPlacement{
					Path: "secret-dir",
				}}},
			},
		},
	}, nil, &env)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "secret-dir", "nested.txt"), []byte("dir-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifact, cleanup, err := workspace.CreateWorkspaceArtifactFromRootWithExcludes(
		workspaceRoot,
		t.TempDir(),
		workspaceRoot,
		workspaceSecretExcludePatterns(workspaceRoot, secretPaths),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	extracted := t.TempDir()
	file, err := os.Open(artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := archive.ExtractTar(file, extracted); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, filepath.Join(extracted, "public.txt")); got != "public" {
		t.Fatalf("public file = %q", got)
	}
	if _, err := os.Stat(filepath.Join(extracted, "secrets", "token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace file secret leaked into artifact: %v", err)
	}
	if _, err := os.Stat(filepath.Join(extracted, "secret-dir")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace dir secret leaked into artifact: %v", err)
	}
}

func TestApplySecretsResolvesRootOwnerWithoutPasswd(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires chown")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	owner := "root"
	err := applySecrets(root, workspace, &runv0.RunTaskRequest{
		Secrets: []*runv0.SecretInject{{
			Name:       "token",
			ValueBytes: []byte("secret"),
			Placement:  &runv0.Placement{Kind: &runv0.Placement_File{File: &runv0.FilePlacement{Path: "token", Owner: &owner}}},
		}},
	}, nil, new([]string))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(workspace, "token"))
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if stat.Uid != 0 || stat.Gid != 0 {
		t.Fatalf("owner = %d:%d", stat.Uid, stat.Gid)
	}
}

func TestApplySecretsRejectsInvalidFileMode(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	mode := "not-octal"
	err := applySecrets(root, workspace, &runv0.RunTaskRequest{
		Secrets: []*runv0.SecretInject{{
			Name:       "token",
			ValueBytes: []byte("secret"),
			Placement: &runv0.Placement{Kind: &runv0.Placement_File{File: &runv0.FilePlacement{
				Path: "token",
				Mode: &mode,
			}}},
		}},
	}, nil, new([]string))
	if err == nil || !strings.Contains(err.Error(), "invalid secret file mode") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "token")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("secret file stat err = %v", statErr)
	}
}

func TestApplySecretsRejectsSpecialModeBits(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	mode := "1777"
	err := applySecrets(root, workspace, &runv0.RunTaskRequest{
		Secrets: []*runv0.SecretInject{{
			Name:       "token",
			ValueBytes: []byte("secret"),
			Placement: &runv0.Placement{Kind: &runv0.Placement_File{File: &runv0.FilePlacement{
				Path: "token",
				Mode: &mode,
			}}},
		}},
	}, nil, new([]string))
	if err == nil || !strings.Contains(err.Error(), "permission bits") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "token")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("secret file stat err = %v", statErr)
	}
}

func TestApplySecretsRejectsParentPathComponents(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	err := applySecrets(root, workspace, &runv0.RunTaskRequest{
		Secrets: []*runv0.SecretInject{{
			Name:       "token",
			ValueBytes: []byte("secret"),
			Placement:  &runv0.Placement{Kind: &runv0.Placement_File{File: &runv0.FilePlacement{Path: "a/../token"}}},
		}},
	}, nil, new([]string))
	if err == nil || !strings.Contains(err.Error(), "parent components") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "token")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("secret file stat err = %v", statErr)
	}
}

func TestApplySecretsRejectsPathWhitespace(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	err := applySecrets(root, workspace, &runv0.RunTaskRequest{
		Secrets: []*runv0.SecretInject{{
			Name:       "token",
			ValueBytes: []byte("secret"),
			Placement:  &runv0.Placement{Kind: &runv0.Placement_File{File: &runv0.FilePlacement{Path: " /tmp/token"}}},
		}},
	}, nil, new([]string))
	if err == nil || !strings.Contains(err.Error(), "leading or trailing whitespace") {
		t.Fatalf("err = %v", err)
	}
}

func TestApplySecretsRejectsInvalidDirMode(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	mode := ""
	err := applySecrets(root, workspace, &runv0.RunTaskRequest{
		Secrets: []*runv0.SecretInject{{
			Name:       "token",
			ValueBytes: []byte("secret"),
			Placement: &runv0.Placement{Kind: &runv0.Placement_Dir{Dir: &runv0.DirPlacement{
				Path: "secrets",
				Mode: &mode,
			}}},
		}},
	}, nil, new([]string))
	if err == nil || !strings.Contains(err.Error(), "invalid secret dir mode") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "secrets")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("secret dir stat err = %v", statErr)
	}
}

func TestEnsureRuntimeCanTraverseSecretPathRejectsBlockedAncestor(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(blocked, "secret")
	if err := os.WriteFile(target, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := ensureRuntimeCanReadSecretFile(root, filepath.Join(root, "workspace"), target, &resolvedRuntimeUser{
		Name: "agent",
		UID:  uint32(os.Getuid() + 1),
		GID:  uint32(os.Getgid() + 1),
		Home: "/home/agent",
	})
	if err == nil || !strings.Contains(err.Error(), "not traversable") {
		t.Fatalf("err = %v", err)
	}
}

func TestApplySecretsRejectsMalformedPlacements(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		secret *runv0.SecretInject
		want   string
	}{
		{
			name:   "missing env",
			secret: &runv0.SecretInject{Name: "token", Placement: &runv0.Placement{Kind: &runv0.Placement_Env{}}},
			want:   "env placement name is required",
		},
		{
			name: "empty file path",
			secret: &runv0.SecretInject{
				Name:      "token",
				Placement: &runv0.Placement{Kind: &runv0.Placement_File{File: &runv0.FilePlacement{}}},
			},
			want: "file placement path is required",
		},
		{
			name: "empty dir path",
			secret: &runv0.SecretInject{
				Name:      "token",
				Placement: &runv0.Placement{Kind: &runv0.Placement_Dir{Dir: &runv0.DirPlacement{}}},
			},
			want: "dir placement path is required",
		},
		{
			name: "workspace root dir path",
			secret: &runv0.SecretInject{
				Name:      "token",
				Placement: &runv0.Placement{Kind: &runv0.Placement_Dir{Dir: &runv0.DirPlacement{Path: "/workspace"}}},
			},
			want: "dir placement path must not target the workspace root",
		},
		{
			name:   "missing kind",
			secret: &runv0.SecretInject{Name: "token", Placement: &runv0.Placement{}},
			want:   "placement is required",
		},
		{
			name:   "missing placement",
			secret: &runv0.SecretInject{Name: "token"},
			want:   "placement is required",
		},
		{
			name:   "nil secret",
			secret: nil,
			want:   "secret injection is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := applySecrets(root, workspace, &runv0.RunTaskRequest{
				Secrets: []*runv0.SecretInject{tt.secret},
			}, nil, new([]string))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestApplySecretsRejectsReservedRuntimePlacements(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/opt/helmr/adapter/main.js", "/proc/self/environ", "/dev/null", "/sys/kernel"} {
		t.Run(path, func(t *testing.T) {
			err := applySecrets(root, workspace, &runv0.RunTaskRequest{
				Secrets: []*runv0.SecretInject{{
					Name:       "token",
					ValueBytes: []byte("secret"),
					Placement:  &runv0.Placement{Kind: &runv0.Placement_File{File: &runv0.FilePlacement{Path: path}}},
				}},
			}, nil, new([]string))
			if err == nil || !strings.Contains(err.Error(), "reserved runtime paths") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestApplySecretsRejectsDynamicLoaderEnvPlacement(t *testing.T) {
	err := applySecrets(t.TempDir(), t.TempDir(), &runv0.RunTaskRequest{
		Secrets: []*runv0.SecretInject{{
			Name:       "preload",
			ValueBytes: []byte("/image/lib/libhook.so"),
			Placement:  &runv0.Placement{Kind: &runv0.Placement_Env{Env: &runv0.EnvPlacement{Name: "LD_PRELOAD"}}},
		}},
	}, nil, new([]string))
	if err == nil || !strings.Contains(err.Error(), "reserved runtime environment") {
		t.Fatalf("err = %v", err)
	}
}

func TestApplySecretsEnvReplacesImageEnv(t *testing.T) {
	env := []string{"TOKEN=old", "FOO=bar"}
	err := applySecrets(t.TempDir(), t.TempDir(), &runv0.RunTaskRequest{
		Secrets: []*runv0.SecretInject{{
			Name:       "token",
			ValueBytes: []byte("secret"),
			Placement:  &runv0.Placement{Kind: &runv0.Placement_Env{Env: &runv0.EnvPlacement{Name: "TOKEN"}}},
		}},
	}, nil, &env)
	if err != nil {
		t.Fatal(err)
	}
	if got := envValue(env, "TOKEN"); got != "secret" {
		t.Fatalf("TOKEN = %q", got)
	}
	if count := envKeyCount(env, "TOKEN"); count != 1 {
		t.Fatalf("TOKEN count = %d in %v", count, env)
	}
}

func TestCopyTreeRejectsDestinationSymlinkParent(t *testing.T) {
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "dir", "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(destination, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := copyTreeSkipping(source, destination, nil); err == nil {
		t.Fatal("expected destination symlink parent rejection")
	}
}

func testRunTaskRequest() *runv0.RunTaskRequest {
	return &runv0.RunTaskRequest{
		TaskId:      "task",
		RunId:       "run",
		SessionId:   "session",
		PayloadJson: "{}",
		Workspace: &runv0.RunTaskWorkspace{
			Path:        "/workspace",
			ProjectPath: "/workspace",
			Artifact: &runv0.WorkspaceArtifact{
				Digest:     "sha256:workspace",
				MediaType:  "application/vnd.helmr.workspace.v0.tar",
				Encoding:   "tar",
				SizeBytes:  1024,
				EntryCount: 1,
			},
			Writable: true,
		},
	}
}

func TestInstallAdapterBundleCreatesMissingOptPath(t *testing.T) {
	adapterBundleRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(adapterBundleRoot, "adapter"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(adapterBundleRoot, "adapter", "main.js"), []byte("adapter"), 0o644); err != nil {
		t.Fatal(err)
	}
	imageRoot := t.TempDir()
	if err := installAdapterBundle(adapterBundleRoot, imageRoot); err != nil {
		t.Fatal(err)
	}
	if got := readText(t, filepath.Join(imageRoot, "opt", "helmr", "adapter", "main.js")); got != "adapter" {
		t.Fatalf("adapter bundle = %q", got)
	}
}

func TestInstallAdapterBundleRejectsSymlinkedOptParent(t *testing.T) {
	adapterBundleRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(adapterBundleRoot, "adapter"), 0o755); err != nil {
		t.Fatal(err)
	}
	imageRoot := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(imageRoot, "opt")); err != nil {
		t.Fatal(err)
	}
	err := installAdapterBundle(adapterBundleRoot, imageRoot)
	if err == nil || !strings.Contains(err.Error(), "unsafe tar parent") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "helmr")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("adapter bundle escaped through /opt symlink, stat err = %v", err)
	}
}

func TestSafeJoinStaysUnderRoot(t *testing.T) {
	root := t.TempDir()
	path, err := safepath.JoinSlash(root, "../outside")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		t.Fatalf("path %q escaped root %q", path, root)
	}
}
