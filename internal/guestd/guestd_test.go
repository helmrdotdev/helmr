package guestd

import (
	"archive/tar"
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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/cas"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/transport"
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
		event, err := transport.ReadRunEvent(&stream)
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StdoutChunk:
			stdout += string(value.StdoutChunk)
		case *runv0.RunEvent_StderrChunk:
			stderr += string(value.StderrChunk)
		case *runv0.RunEvent_TaskComplete:
			completed = true
			if value.TaskComplete.ExitCode != 0 {
				t.Fatalf("exit code = %d", value.TaskComplete.ExitCode)
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
	var complete *runv0.TaskComplete
	for complete == nil {
		event, err := transport.ReadRunEvent(&stream)
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StdoutChunk:
			stdout += string(value.StdoutChunk)
		case *runv0.RunEvent_StderrChunk:
			stderr += string(value.StderrChunk)
		case *runv0.RunEvent_TaskComplete:
			complete = value.TaskComplete
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
	var complete *runv0.TaskComplete
	for complete == nil {
		event, err := transport.ReadRunEvent(&stream)
		if err != nil {
			t.Fatal(err)
		}
		complete = event.GetTaskComplete()
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
	var complete *runv0.TaskComplete
	for complete == nil {
		event, err := transport.ReadRunEvent(&stream)
		if err != nil {
			t.Fatal(err)
		}
		complete = event.GetTaskComplete()
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

	var complete *runv0.TaskComplete
	for complete == nil {
		event, err := transport.ReadRunEvent(host)
		if err != nil {
			t.Fatal(err)
		}
		complete = event.GetTaskComplete()
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

func TestRunAdapterNoOutcomeTerminatesDescendantFDEOF(t *testing.T) {
	tempDir, runner := guestAdapterHelperRunner(t, "no-outcome-fd-holder")

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

	var complete *runv0.TaskComplete
	for complete == nil {
		event, err := transport.ReadRunEvent(host)
		if err != nil {
			t.Fatal(err)
		}
		complete = event.GetTaskComplete()
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
		t.Fatal("runAdapter did not return after no-outcome adapter left inherited fds open")
	}
}

func TestRunAdapterPrefersLateTaskOutcomeAfterWaitTimeout(t *testing.T) {
	tempDir, runner := guestAdapterHelperRunner(t, "task-outcome-after-blocked-control-event")

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
	event, err := transport.ReadRunEvent(host)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(event.GetStdoutChunk()); got != "blocked-before-outcome\n" {
		t.Fatalf("first event stdout = %q event=%+v", got, event)
	}

	var complete *runv0.TaskComplete
	for complete == nil {
		event, err := transport.ReadRunEvent(host)
		if err != nil {
			t.Fatal(err)
		}
		complete = event.GetTaskComplete()
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
	go serveHealth(listener, ready.Load)

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
		event, err := transport.ReadRunEvent(&stream.written)
		if err != nil {
			t.Fatal(err)
		}
		if event.GetWaitRequested() != nil {
			sawWait = true
			continue
		}
		if event.GetLogEntry() != "" {
			continue
		}
		complete := event.GetTaskComplete()
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

func TestHandleRunRejectsSourceOnlyRun(t *testing.T) {
	var stream bytes.Buffer
	err := handleRunConnection(context.Background(), &stream, Config{
		AdapterRuntimePath: "/bin/false",
		AdapterPath:        "adapter.js",
	}, slogDiscard(), newWaitingRunRegistry(), transport.StreamHeader{Type: transport.StreamTypeWorkspaceArtifact, RunID: "run"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	stderr, complete := readGuestdFailureEvents(t, &stream)
	if !strings.Contains(stderr, `unsupported input stream type "workspace-artifact"`) {
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
			want:         `run request run_id "run-2" does not match input stream run_id "run-1"`,
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
			if err := transport.WriteStreamFrameHeader(&input, transport.StreamHeader{Type: transport.StreamTypeDeploymentSource, RunID: tt.sourceRunID}, uint64(len(source))); err != nil {
				t.Fatal(err)
			}
			if _, err := input.Write(source); err != nil {
				t.Fatal(err)
			}
			if err := transport.WriteProtoFrame(&input, &runv0.RunTaskRequest{
				RunId:       tt.requestRunID,
				TaskId:      "task",
				PayloadJson: "{}",
			}); err != nil {
				t.Fatal(err)
			}
			if tt.sourceRunID == tt.imageRunID {
				if err := transport.WriteStreamFrameHeader(&input, transport.StreamHeader{Type: transport.StreamTypeWorkspaceArtifact, RunID: tt.sourceRunID}, uint64(len(source))); err != nil {
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
			}, slogDiscard(), newWaitingRunRegistry(), transport.StreamHeader{Type: transport.StreamTypeRunImage, RunID: tt.imageRunID}, uint64(len(image)))
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
	if err := transport.WriteStreamFrameHeader(&input, transport.StreamHeader{Type: transport.StreamTypeDeploymentSource, RunID: "run-1"}, uint64(len(deploymentSource))); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write(deploymentSource); err != nil {
		t.Fatal(err)
	}
	request := testRunTaskRequest()
	request.RunId = "run-1"
	request.Workspace.Artifact.SizeBytes = uint64(len(source))
	request.Workspace.Artifact.EntryCount = 1
	if err := transport.WriteProtoFrame(&input, request); err != nil {
		t.Fatal(err)
	}
	if err := transport.WriteStreamFrameHeader(&input, transport.StreamHeader{Type: transport.StreamTypeWorkspaceArtifact, RunID: "run-1"}, uint64(len(source))); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write(source); err != nil {
		t.Fatal(err)
	}
	stream := &runSetupStream{read: bytes.NewReader(input.Bytes())}
	err := handleRunConnection(context.Background(), stream, Config{}, slogDiscard(), newWaitingRunRegistry(), transport.StreamHeader{Type: transport.StreamTypeRunImage, RunID: "run-1"}, uint64(len(image)))
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
	if err := transport.WriteStreamFrameHeader(&input, transport.StreamHeader{Type: transport.StreamTypeDeploymentSource, RunID: "run-1"}, uint64(len(deploymentSource))); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write(deploymentSource); err != nil {
		t.Fatal(err)
	}
	request := testRunTaskRequest()
	request.RunId = "run-1"
	request.Workspace.Artifact.Digest = cas.DigestBytes([]byte("not the tar body"))
	request.Workspace.Artifact.SizeBytes = uint64(len(source))
	request.Workspace.Artifact.EntryCount = 1
	if err := transport.WriteProtoFrame(&input, request); err != nil {
		t.Fatal(err)
	}
	declaredDigest := request.Workspace.Artifact.Digest
	if err := transport.WriteStreamFrameHeader(&input, transport.StreamHeader{Type: transport.StreamTypeWorkspaceArtifact, RunID: "run-1", BodyDigest: &declaredDigest}, uint64(len(source))); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Write(source); err != nil {
		t.Fatal(err)
	}
	stream := &runSetupStream{read: bytes.NewReader(input.Bytes())}

	err := handleRunConnection(context.Background(), stream, Config{}, slogDiscard(), newWaitingRunRegistry(), transport.StreamHeader{Type: transport.StreamTypeRunImage, RunID: "run-1"}, uint64(len(image)))
	if err != nil {
		t.Fatal(err)
	}
	stderr, complete := readGuestdFailureEvents(t, &stream.written)
	if !strings.Contains(stderr, "workspace artifact body digest") || complete.ExitCode != 1 {
		t.Fatalf("stderr = %q complete = %+v", stderr, complete)
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
	if err := transport.WriteProtoFrame(&stream, &runv0.ResumeAttach{
		CheckpointId: "checkpoint-1",
		WaitpointId:  "waitpoint-1",
		SessionId:    "execution-1",
	}); err != nil {
		t.Fatal(err)
	}
	start, err := readConnectionStart(&stream)
	if err != nil {
		t.Fatal(err)
	}
	if start.attach == nil || start.attach.CheckpointId != "checkpoint-1" || start.attach.WaitpointId != "waitpoint-1" {
		t.Fatalf("start = %+v", start)
	}
}

func TestReadConnectionStartAcceptsStreamHeader(t *testing.T) {
	var stream bytes.Buffer
	if err := transport.WriteStreamFrameHeader(&stream, transport.StreamHeader{Type: transport.StreamTypeRunImage, RunID: "run-1"}, 5); err != nil {
		t.Fatal(err)
	}
	stream.WriteString("hello")
	start, err := readConnectionStart(&stream)
	if err != nil {
		t.Fatal(err)
	}
	if start.streamHeader.Type != transport.StreamTypeRunImage || start.streamHeader.RunID != "run-1" || start.bodyLen != 5 {
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
	if err := transport.WriteStreamFrameHeader(&stream, transport.StreamHeader{Type: transport.StreamTypeRunImage, RunID: "run-1"}, transport.MaxFrameBytes+1); err != nil {
		t.Fatal(err)
	}
	start, err := readConnectionStart(&stream)
	if err != nil {
		t.Fatal(err)
	}
	if start.streamHeader.Type != transport.StreamTypeRunImage || start.streamHeader.RunID != "run-1" {
		t.Fatalf("header = %+v", start.streamHeader)
	}
	if start.bodyLen != uint64(transport.MaxFrameBytes+1) {
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
	err := handleCompileTaskBundle(context.Background(), stream, Config{}, transport.StreamHeader{
		Type:   transport.StreamTypeCompileTaskBundle,
		RunID:  "run-1",
		TaskID: "task-1",
	}, uint64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	frame, err := transport.ReadMessageFrame(&stream.Buffer)
	if err != nil {
		t.Fatal(err)
	}
	parseErr, ok, err := transport.DecodeParseErrorFrame(frame)
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
	err := handleCatalogDeployment(context.Background(), stream, Config{}, transport.StreamHeader{
		Type:  transport.StreamTypeCatalogDeployment,
		RunID: "deployment-1",
	}, uint64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	frame, err := transport.ReadMessageFrame(&stream.Buffer)
	if err != nil {
		t.Fatal(err)
	}
	parseErr, ok, err := transport.DecodeParseErrorFrame(frame)
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
	err := handleRunConnection(context.Background(), stream, Config{}, slogDiscard(), newWaitingRunRegistry(), transport.StreamHeader{
		Type:  transport.StreamTypeRunImage,
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

	event, err := transport.ReadRunEvent(originalHost)
	if err != nil {
		t.Fatal(err)
	}
	if event.GetWaitRequested() == nil {
		t.Fatalf("first event = %+v", event)
	}
	writeSuspendAndReadReady(t, originalHost, "waitpoint-1", "checkpoint-1")
	if err := originalHost.Close(); err != nil {
		t.Fatal(err)
	}
	if err := registry.attach("waitpoint-1", "checkpoint-1", attachedGuest); err != nil {
		t.Fatal(err)
	}
	if err := transport.WriteProtoFrame(attachedHost, &runv0.ResumeDecision{
		WaitpointId:           "waitpoint-1",
		Kind:                  "completed",
		ResolutionPayloadJson: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	var ack runv0.ResumeAck
	if err := transport.ReadProtoFrame(attachedHost, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.WaitpointId != "waitpoint-1" {
		t.Fatalf("ack = %+v", &ack)
	}

	var stdout string
	var completed bool
	for !completed {
		event, err := transport.ReadRunEvent(attachedHost)
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StdoutChunk:
			stdout += string(value.StdoutChunk)
		case *runv0.RunEvent_TaskComplete:
			completed = true
			if value.TaskComplete.ExitCode != 0 {
				t.Fatalf("exit code = %d message=%v", value.TaskComplete.ExitCode, value.TaskComplete.ErrorMessage)
			}
		}
	}
	if !strings.Contains(stdout, "after-resume") {
		t.Fatalf("stdout = %q", stdout)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestRunAdapterReadsNextCheckpointSuspendFromAttachedStream(t *testing.T) {
	t.Setenv("HELMR_GUESTD_HELPER", "resume-handoff-twice")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

	deadline := time.Now().Add(15 * time.Second)
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

	readWaitRequested(t, originalHost)
	writeSuspendAndReadReady(t, originalHost, "waitpoint-1", "checkpoint-1")
	if err := registry.attach("waitpoint-1", "checkpoint-1", firstGuest); err != nil {
		t.Fatal(err)
	}
	writeDecisionAndReadAck(t, firstHost, "waitpoint-1", "completed")

	readWaitRequested(t, firstHost)
	writeSuspendAndReadReady(t, firstHost, "waitpoint-2", "checkpoint-2")
	if err := registry.attach("waitpoint-2", "checkpoint-2", secondGuest); err != nil {
		t.Fatal(err)
	}
	writeDecisionAndReadAck(t, secondHost, "waitpoint-2", "completed")

	var stdout string
	var completed bool
	for !completed {
		event, err := transport.ReadRunEvent(secondHost)
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StdoutChunk:
			stdout += string(value.StdoutChunk)
		case *runv0.RunEvent_TaskComplete:
			completed = true
			if value.TaskComplete.ExitCode != 0 {
				t.Fatalf("exit code = %d message=%v", value.TaskComplete.ExitCode, value.TaskComplete.ErrorMessage)
			}
		}
	}
	if !strings.Contains(stdout, "after-second") {
		t.Fatalf("stdout = %q", stdout)
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

func readWaitRequested(t *testing.T, conn io.Reader) {
	t.Helper()
	for {
		event, err := transport.ReadRunEvent(conn)
		if err != nil {
			t.Fatal(err)
		}
		if event.GetWaitRequested() != nil {
			return
		}
	}
}

func writeSuspendAndReadReady(t *testing.T, conn io.ReadWriter, waitpointID string, checkpointID string) {
	t.Helper()
	if err := transport.WriteProtoFrame(conn, &runv0.SuspendForCheckpoint{
		WaitpointId:  waitpointID,
		CheckpointId: checkpointID,
	}); err != nil {
		t.Fatal(err)
	}
	header, bodyLen, err := transport.ReadStreamFrameHeader(conn)
	if err != nil {
		t.Fatal(err)
	}
	if header.Type != transport.StreamTypeCheckpointPauseReady || header.WaitpointID != waitpointID || header.CheckpointID != checkpointID || bodyLen != 0 {
		t.Fatalf("pause ready = %+v bodyLen=%d, want waitpoint=%s checkpoint=%s", header, bodyLen, waitpointID, checkpointID)
	}
}

func writeDecisionAndReadAck(t *testing.T, conn io.ReadWriter, waitpointID string, kind string) {
	t.Helper()
	if err := transport.WriteProtoFrame(conn, &runv0.ResumeDecision{
		WaitpointId:           waitpointID,
		Kind:                  kind,
		ResolutionPayloadJson: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	var ack runv0.ResumeAck
	if err := transport.ReadProtoFrame(conn, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.WaitpointId != waitpointID {
		t.Fatalf("ack = %+v, want waitpoint=%s", &ack, waitpointID)
	}
}

func stringPtr(value string) *string {
	return &value
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
		_ = control.Close()
		fmt.Println("stdout-line")
		fmt.Fprintln(os.Stderr, "stderr-line")
		time.Sleep(50 * time.Millisecond)
		return 0
	case "stdout-stderr-tail":
		control, err := helperControlWriter()
		if err != nil {
			return 2
		}
		_ = control.Close()
		fmt.Print(strings.Repeat("stdout-block\n", 8192))
		fmt.Println("stdout-tail")
		fmt.Fprint(os.Stderr, strings.Repeat("stderr-block\n", 8192))
		fmt.Fprintln(os.Stderr, "stderr-tail")
		return 0
	case "stdout-json":
		control, err := helperControlWriter()
		if err != nil {
			return 2
		}
		_ = control.Close()
		fmt.Print(`{"result":{"ok":true,"count":2}}`)
		return 0
	case "stdout-json-exit-3":
		control, err := helperControlWriter()
		if err != nil {
			return 2
		}
		_ = control.Close()
		fmt.Print(`{"result":{"ok":false}}`)
		return 3
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
		if err := transport.WriteProtoFrame(control, &runv0.RunEvent{
			Event: &runv0.RunEvent_TaskOutcome{TaskOutcome: &runv0.TaskOutcome{
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
	case "no-outcome-fd-holder":
		control, err := helperControlWriter()
		if err != nil {
			return 2
		}
		if err := startFDHolderChild(control); err != nil {
			fmt.Fprintln(os.Stderr, err)
			_ = control.Close()
			return 2
		}
		fmt.Println("supervisor-exiting-without-outcome")
		_ = control.Close()
		return 0
	case "task-outcome-after-blocked-control-event":
		control, err := helperControlWriter()
		if err != nil {
			return 2
		}
		if err := transport.WriteProtoFrame(control, &runv0.RunEvent{
			Event: &runv0.RunEvent_StdoutChunk{StdoutChunk: []byte("blocked-before-outcome\n")},
		}); err != nil {
			_ = control.Close()
			return 2
		}
		outputJSON := `{"late":true}`
		if err := transport.WriteProtoFrame(control, &runv0.RunEvent{
			Event: &runv0.RunEvent_TaskOutcome{TaskOutcome: &runv0.TaskOutcome{
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
		if err := transport.WriteProtoFrame(control, &runv0.RunEvent{
			Event: &runv0.RunEvent_WaitRequested{WaitRequested: &runv0.WaitRequested{
				CorrelationId: "approval-1",
				Kind:          "token",
				RequestJson:   `{}`,
				DisplayText:   stringPtr("approve"),
			}},
		}); err != nil {
			return 2
		}
		_ = control.Close()
		return 0
	default:
		return 2
	}
	control, err := helperControlWriter()
	if err != nil {
		return 2
	}
	if err := transport.WriteProtoFrame(control, &runv0.RunEvent{
		Event: &runv0.RunEvent_WaitRequested{WaitRequested: &runv0.WaitRequested{
			CorrelationId: "approval-1",
			Kind:          "token",
			RequestJson:   `{}`,
			DisplayText:   stringPtr("approve"),
		}},
	}); err != nil {
		return 2
	}
	var decision runv0.ResumeDecision
	if err := transport.ReadProtoFrame(os.Stdin, &decision); err != nil {
		return 2
	}
	fmt.Println("after-first")
	if os.Getenv("HELMR_GUESTD_HELPER") == "resume-handoff-twice" {
		if err := transport.WriteProtoFrame(control, &runv0.RunEvent{
			Event: &runv0.RunEvent_WaitRequested{WaitRequested: &runv0.WaitRequested{
				CorrelationId: "message-1",
				Kind:          "token",
				RequestJson:   `{}`,
				DisplayText:   stringPtr("reply"),
			}},
		}); err != nil {
			return 2
		}
		if err := transport.ReadProtoFrame(os.Stdin, &decision); err != nil {
			return 2
		}
		fmt.Println("after-second")
		return 0
	}
	fmt.Println("after-resume")
	return 0
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

func readGuestdFailureEvents(t *testing.T, stream io.Reader) (string, *runv0.TaskComplete) {
	t.Helper()
	var stderr string
	for {
		event, err := transport.ReadRunEvent(stream)
		if err != nil {
			t.Fatal(err)
		}
		switch value := event.Event.(type) {
		case *runv0.RunEvent_StderrChunk:
			stderr += string(value.StderrChunk)
		case *runv0.RunEvent_TaskComplete:
			return stderr, value.TaskComplete
		default:
			t.Fatalf("unexpected event = %+v", event)
		}
	}
}

func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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

func TestMaterializeDeploymentSourceForRuntimePlacesSourceUnderLaunchCwd(t *testing.T) {
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
	if err := os.MkdirAll(filepath.Join(imageRoot, "workspace", "node_modules", "dep"), 0o755); err != nil {
		t.Fatal(err)
	}

	taskCwd, err := materializeDeploymentSourceForRuntime(imageRoot, sourceRoot, "/workspace", &resolvedRuntimeUser{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())})
	if err != nil {
		t.Fatal(err)
	}

	if taskCwd != "/workspace/.helmr/deployment-source" {
		t.Fatalf("task cwd = %q", taskCwd)
	}
	if got := readText(t, filepath.Join(imageRoot, "workspace", ".helmr", "deployment-source", "tasks", "task.ts")); got != "task" {
		t.Fatalf("deployment source = %q", got)
	}
	if _, err := os.Stat(filepath.Join(imageRoot, "workspace", ".helmr", "deployment-source", "node_modules", "dep", "package.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("deployment source dependencies leaked into runtime: %v", err)
	}
	if _, err := os.Stat(filepath.Join(imageRoot, "workspace", "node_modules", "dep")); err != nil {
		t.Fatalf("workspace dependencies missing: %v", err)
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
	refKind := "branch"
	return &runv0.RunTaskRequest{
		TaskId:      "task",
		RunId:       "run",
		PayloadJson: "{}",
		Source: &runv0.RunTaskSource{
			Kind: &runv0.RunTaskSource_Github{
				Github: &runv0.RunTaskGitHubSource{
					Repository:   "helmrdotdev/helmr",
					RequestedRef: "main",
					ResolvedSha:  "0123456789abcdef0123456789abcdef01234567",
					RefKind:      &refKind,
				},
			},
		},
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
			VolumeKind: "copy-on-write",
			Writable:   true,
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
	path, err := safeJoin(root, "../outside")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		t.Fatalf("path %q escaped root %q", path, root)
	}
}
