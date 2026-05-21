package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	bundlev0 "github.com/helmrdotdev/helmr/internal/gen/helmr/bundle/v0"
	runv0 "github.com/helmrdotdev/helmr/internal/gen/helmr/run/v0"
	"github.com/helmrdotdev/helmr/internal/guest"
	"github.com/helmrdotdev/helmr/internal/sourcetar"
	"github.com/helmrdotdev/helmr/internal/vm"
	"google.golang.org/protobuf/proto"
)

func TestGuestRunnerWritesRunFramesAndReadsCompletion(t *testing.T) {
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "main.ts"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(sourceRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, ".git", "config"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, ".env"), []byte("TOKEN=committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(t.TempDir(), "image.oci.tar")
	if err := os.WriteFile(imagePath, []byte("oci"), 0o644); err != nil {
		t.Fatal(err)
	}
	stream := newScriptedGuestStream(t, &runv0.RunEvent{
		Event: &runv0.RunEvent_LogEntry{LogEntry: "building"},
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_StdoutChunk{StdoutChunk: []byte("hello\n")},
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_EmitEvent{EmitEvent: &runv0.EmitEvent{Type: "deploy.progress", ContentJson: `{"broken":secret}`}},
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_StderrChunk{StderrChunk: []byte("warn\n")},
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskComplete{TaskComplete: &runv0.TaskComplete{ExitCode: 7}},
	})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	events := &capturingEventSink{}
	claim := api.WorkerRunLease{ID: "execution-1", RunID: "run-1", WorkerInstanceID: "worker-1"}
	result, err := GuestRunner{
		Connector: &fakeGuestConnector{stream: stream},
		Events:    events,
		TempDir:   t.TempDir(),
		Stdout:    &stdout,
		Stderr:    &stderr,
	}.Run(context.Background(), Request{
		Lease: claim,
		Run: ResolvedRun{
			RunID:   "run-1",
			TaskID:  "deploy",
			Bundle:  runtimeBundle(),
			Payload: []byte(`{"ok":true}`),
			Secrets: api.ResolvedSecrets{},
		},
		Artifact:        builder.Artifact{ImageTarPath: imagePath},
		TaskSource:      builder.Source{ProjectRoot: sourceRoot},
		WorkspaceSource: builder.Source{ProjectRoot: sourceRoot},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 7 || stdout.String() != "hello\n" {
		t.Fatalf("result = %+v stdout = %q", result, stdout.String())
	}
	if stderr.String() != "warn\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if len(events.logs) != 2 || events.logs[0].stream != api.WorkerLogStreamStdout || events.logs[0].observedSeq != 2 || string(events.logs[0].content) != "hello\n" {
		t.Fatalf("stdout event = %+v", events.logs)
	}
	if events.logs[1].stream != api.WorkerLogStreamStderr || events.logs[1].observedSeq != 4 || string(events.logs[1].content) != "warn\n" {
		t.Fatalf("stderr event = %+v", events.logs)
	}
	if len(events.entries) != 1 || events.entries[0] != "building" {
		t.Fatalf("log entries = %+v", events.entries)
	}
	if len(events.emits) != 1 || events.emits[0].eventType != "deploy.progress" {
		t.Fatalf("emits = %+v", events.emits)
	}
	var emitContent map[string]string
	if err := json.Unmarshal(events.emits[0].content, &emitContent); err != nil {
		t.Fatal(err)
	}
	if emitContent["raw"] != `{"broken":secret}` || !strings.Contains(emitContent["parse_error"], "invalid emit event content_json") {
		t.Fatalf("emit content = %+v", emitContent)
	}

	written := bytes.NewReader(stream.written.Bytes())
	imageHeader, imageLen, err := guest.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	imageBody := readExactly(t, written, imageLen)
	if imageHeader.Type != guest.StreamTypeRunImage || imageHeader.RunID != "run-1" || string(imageBody) != "oci" {
		t.Fatalf("image header = %+v body = %q", imageHeader, imageBody)
	}

	taskHeader, taskLen, err := guest.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	taskBody := readExactly(t, written, taskLen)
	if taskHeader.Type != guest.StreamTypeTaskSource || taskHeader.RunID != "run-1" {
		t.Fatalf("task source header = %+v", taskHeader)
	}
	taskNames := tarNames(t, taskBody)
	if taskNames[".git/config"] {
		t.Fatalf("task source tar included checkout metadata: %v", taskNames)
	}
	sourceHeader, sourceLen, err := guest.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	sourceBody := readExactly(t, written, sourceLen)
	if sourceHeader.Type != guest.StreamTypeWorkspaceSource || sourceHeader.RunID != "run-1" {
		t.Fatalf("workspace source header = %+v", sourceHeader)
	}
	sourceNames := tarNames(t, sourceBody)
	if sourceNames[".git/config"] {
		t.Fatalf("source tar included checkout metadata: %v", sourceNames)
	}
	if !sourceNames["main.ts"] || !sourceNames[".env"] {
		t.Fatalf("source tar names = %v", sourceNames)
	}

	var request runv0.RunTaskRequest
	if err := guest.ReadProtoFrame(written, &request); err != nil {
		t.Fatal(err)
	}
	if request.RunId != "run-1" || request.TaskId != "deploy" || request.ModulePath != "src/task.ts" || request.Cwd != "/workspace" {
		t.Fatalf("request = %+v", &request)
	}
	if request.WorkspaceOverlay == nil || request.WorkspaceOverlay.UpperKind != "tmpfs" {
		t.Fatalf("workspace overlay = %+v", request.WorkspaceOverlay)
	}
}

func TestGuestRunnerCarriesTaskOutput(t *testing.T) {
	imagePath := filepath.Join(t.TempDir(), "image.oci.tar")
	if err := os.WriteFile(imagePath, []byte("oci"), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "main.ts"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	outputJSON := `{"ok":true,"count":2}`
	stream := newScriptedGuestStream(t, &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskOutput{TaskOutput: &runv0.TaskOutput{OutputJson: outputJSON}},
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskComplete{TaskComplete: &runv0.TaskComplete{ExitCode: 0}},
	})
	result, err := GuestRunner{
		Connector: &fakeGuestConnector{stream: stream},
		TempDir:   t.TempDir(),
	}.Run(context.Background(), Request{
		Lease: api.WorkerRunLease{ID: "execution-1", RunID: "run-1", WorkerInstanceID: "worker-1"},
		Run: ResolvedRun{
			RunID:   "run-1",
			TaskID:  "deploy",
			Bundle:  runtimeBundle(),
			Payload: []byte(`{}`),
			Secrets: api.ResolvedSecrets{},
		},
		Artifact:        builder.Artifact{ImageTarPath: imagePath},
		TaskSource:      builder.Source{ProjectRoot: sourceRoot},
		WorkspaceSource: builder.Source{ProjectRoot: sourceRoot},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || string(result.Output) != outputJSON {
		t.Fatalf("result = %+v", result)
	}
}

func TestGuestRunnerProvidesCheckpointableWaitHandler(t *testing.T) {
	imagePath := filepath.Join(t.TempDir(), "image.oci.tar")
	if err := os.WriteFile(imagePath, []byte("oci"), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "main.ts"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	stream := newScriptedGuestStream(t, &runv0.RunEvent{
		Event: &runv0.RunEvent_WaitRequested{WaitRequested: &runv0.WaitRequested{
			CorrelationId: "approval-1",
			Kind: &runv0.WaitRequested_Approval{Approval: &runv0.ApprovalWait{
				Message: "ship it",
			}},
		}},
	}, &runv0.PauseReady{
		WaitpointId:  "waitpoint-1",
		CheckpointId: "checkpoint-1",
	})
	waiter := &capturingWaitHandler{}
	store := &fakeCAS{}
	result, err := GuestRunner{
		Connector:           &fakeGuestConnector{stream: stream, checkpointable: true},
		CAS:                 store,
		CheckpointEncryptor: testCheckpointEncryptor(t),
		TempDir:             t.TempDir(),
	}.Run(context.Background(), Request{
		Lease: api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"},
		Run: ResolvedRun{
			RunID:      "run-1",
			TaskID:     "deploy",
			Bundle:     runtimeBundle(),
			Payload:    []byte(`{}`),
			Secrets:    api.ResolvedSecrets{},
			ActiveUsed: 2 * time.Second,
		},
		Artifact:        builder.Artifact{ImageTarPath: imagePath},
		TaskSource:      builder.Source{ProjectRoot: sourceRoot},
		WorkspaceSource: builder.Source{ProjectRoot: sourceRoot},
		WaitHandler:     waiter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Detached {
		t.Fatalf("result = %+v, want detached", result)
	}
	if waiter.request.Checkpointer == nil {
		t.Fatal("wait request did not include checkpointer")
	}
	if waiter.request.ActiveDuration < 2*time.Second {
		t.Fatalf("active duration = %s", waiter.request.ActiveDuration)
	}
	if bytes.Contains(store.content, []byte("memory")) {
		t.Fatal("stored checkpoint memory contains plaintext")
	}
}

func TestGuestRunnerRestoresCheckpointAndAttachesWaitpoint(t *testing.T) {
	state := []byte("state")
	memory := []byte("memory")
	encryptor := testCheckpointEncryptor(t)
	stateObject := encryptedCheckpointObject(t, encryptor, state, "vmstate")
	memoryObject := encryptedCheckpointObject(t, encryptor, memory, "memory")
	stream := newScriptedGuestStream(t, &runv0.ResumeAck{
		WaitpointId: "waitpoint-1",
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskComplete{TaskComplete: &runv0.TaskComplete{ExitCode: 0}},
	})
	connector := &fakeGuestConnector{stream: stream}
	result, err := GuestRunner{
		Connector:           connector,
		CAS:                 &fakeCAS{objects: map[string][]byte{stateObject.digest: stateObject.body, memoryObject.digest: memoryObject.body}},
		CheckpointEncryptor: encryptor,
		TempDir:             t.TempDir(),
	}.Run(context.Background(), Request{
		Lease: api.WorkerRunLease{ID: "execution-1", RunID: "run-1", WorkerInstanceID: "worker-1"},
		Run: ResolvedRun{
			RunID: "run-1",
			Restore: &api.WorkerRestore{
				CheckpointID: "checkpoint-1",
				Checkpoint: api.WorkerCheckpointManifest{
					RuntimeBackend:      "firecracker",
					RuntimeArch:         runtime.GOARCH,
					RuntimeABI:          "helmr.firecracker.snapshot.v0",
					KernelDigest:        stringPtr("sha256:kernel"),
					RootfsDigest:        stringPtr("sha256:rootfs"),
					RuntimeConfigDigest: stringPtr("sha256:runtime-config"),
					VMStateDigest:       &stateObject.digest,
					MemoryDigests:       []string{memoryObject.digest},
				},
				Waitpoint: api.WorkerRestoreWaitpoint{
					ID:                    "waitpoint-1",
					ResolutionKind:        "approved",
					ResolutionPayloadJSON: json.RawMessage(`{"approved":true}`),
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || connector.restoreRequest.ID != "checkpoint-1" || len(connector.restoreRequest.Memory) != 1 {
		t.Fatalf("result=%+v restore=%+v", result, connector.restoreRequest)
	}
	written := bytes.NewReader(stream.written.Bytes())
	var attach runv0.ResumeAttach
	if err := guest.ReadProtoFrame(written, &attach); err != nil {
		t.Fatal(err)
	}
	if attach.CheckpointId != "checkpoint-1" || attach.WaitpointId != "waitpoint-1" || attach.SessionId != "execution-1" {
		t.Fatalf("attach = %+v", &attach)
	}
	var decision runv0.ResumeDecision
	if err := guest.ReadProtoFrame(written, &decision); err != nil {
		t.Fatal(err)
	}
	if decision.WaitpointId != "waitpoint-1" || decision.Kind != "approved" || decision.ResolutionPayloadJson != `{"approved":true}` {
		t.Fatalf("decision = %+v", &decision)
	}
}

func TestValidateRestoreIdentity(t *testing.T) {
	valid := api.WorkerCheckpointManifest{
		RuntimeBackend:      "firecracker",
		RuntimeArch:         runtime.GOARCH,
		RuntimeABI:          "helmr.firecracker.snapshot.v0",
		KernelDigest:        stringPtr("sha256:kernel"),
		RootfsDigest:        stringPtr("sha256:rootfs"),
		RuntimeConfigDigest: stringPtr("sha256:runtime-config"),
	}
	tests := []struct {
		name       string
		checkpoint api.WorkerCheckpointManifest
		want       string
	}{
		{name: "valid", checkpoint: valid},
		{name: "backend", checkpoint: withCheckpointManifest(valid, func(c *api.WorkerCheckpointManifest) { c.RuntimeBackend = "test" }), want: `runtime_backend "test" is not supported`},
		{name: "arch", checkpoint: withCheckpointManifest(valid, func(c *api.WorkerCheckpointManifest) { c.RuntimeArch = "other" }), want: `runtime_arch "other" does not match`},
		{name: "abi", checkpoint: withCheckpointManifest(valid, func(c *api.WorkerCheckpointManifest) { c.RuntimeABI = "" }), want: "runtime_abi is required"},
		{name: "kernel", checkpoint: withCheckpointManifest(valid, func(c *api.WorkerCheckpointManifest) { c.KernelDigest = nil }), want: "kernel_digest is required"},
		{name: "rootfs", checkpoint: withCheckpointManifest(valid, func(c *api.WorkerCheckpointManifest) { c.RootfsDigest = stringPtr(" ") }), want: "rootfs_digest is required"},
		{name: "config", checkpoint: withCheckpointManifest(valid, func(c *api.WorkerCheckpointManifest) { c.RuntimeConfigDigest = nil }), want: "runtime_config_digest is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRestoreIdentity(tt.checkpoint)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestReadResumeAckTimesOut(t *testing.T) {
	stream := newBlockingGuestStream()
	session := fakeGuestSession{stream: stream}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := readResumeAck(ctx, session)
	if err == nil {
		t.Fatal("expected timeout")
	}
	if !stream.isClosed() {
		t.Fatal("expected timeout to close guest stream")
	}
}

func TestGuestRunnerEnforcesMaxDuration(t *testing.T) {
	imagePath := filepath.Join(t.TempDir(), "image.oci.tar")
	if err := os.WriteFile(imagePath, []byte("oci"), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "main.ts"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	stream := newBlockingGuestStream()
	_, err := GuestRunner{
		Connector: &fakeGuestConnector{stream: stream},
		TempDir:   t.TempDir(),
	}.Run(context.Background(), Request{
		Run: ResolvedRun{
			RunID:       "run-1",
			TaskID:      "deploy",
			Bundle:      runtimeBundle(),
			Payload:     []byte(`{}`),
			Secrets:     api.ResolvedSecrets{},
			MaxDuration: 10 * time.Millisecond,
		},
		Artifact:        builder.Artifact{ImageTarPath: imagePath},
		TaskSource:      builder.Source{ProjectRoot: sourceRoot},
		WorkspaceSource: builder.Source{ProjectRoot: sourceRoot},
	})
	if err == nil || !strings.Contains(err.Error(), "max_duration") {
		t.Fatalf("err = %v", err)
	}
	if !stream.isClosed() {
		t.Fatal("expected timeout to close guest stream")
	}
}

func TestGuestRunnerTreatsTaskCompleteErrorMessageAsRuntimeFailure(t *testing.T) {
	imagePath := filepath.Join(t.TempDir(), "image.oci.tar")
	if err := os.WriteFile(imagePath, []byte("oci"), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "main.ts"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	stream := newScriptedGuestStream(t, &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskComplete{TaskComplete: &runv0.TaskComplete{
			ExitCode:     1,
			ErrorMessage: stringPtr("read adapter control event: malformed frame"),
		}},
	})
	_, err := GuestRunner{
		Connector: &fakeGuestConnector{stream: stream},
		TempDir:   t.TempDir(),
	}.Run(context.Background(), Request{
		Lease: api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"},
		Run: ResolvedRun{
			RunID:   "run-1",
			TaskID:  "deploy",
			Bundle:  runtimeBundle(),
			Payload: []byte(`{}`),
			Secrets: api.ResolvedSecrets{},
		},
		Artifact:        builder.Artifact{ImageTarPath: imagePath},
		TaskSource:      builder.Source{ProjectRoot: sourceRoot},
		WorkspaceSource: builder.Source{ProjectRoot: sourceRoot},
	})
	if err == nil || !strings.Contains(err.Error(), "read adapter control event") {
		t.Fatalf("err = %v", err)
	}
}

func TestFailedResultPreservesMaxDurationFailureKind(t *testing.T) {
	result := failedResult(fmt.Errorf("run artifact: %w", MaxDurationError{Limit: 30 * time.Second}))
	if result.FailureKind == nil || *result.FailureKind != "max_duration" {
		t.Fatalf("failure kind = %+v", result.FailureKind)
	}
	if result.LimitSeconds == nil || *result.LimitSeconds != 30 {
		t.Fatalf("limit seconds = %+v", result.LimitSeconds)
	}
}

func TestGuestRunnerReadCancellationClosesSession(t *testing.T) {
	imagePath := filepath.Join(t.TempDir(), "image.oci.tar")
	if err := os.WriteFile(imagePath, []byte("oci"), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "main.ts"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	stream := newBlockingGuestStream()
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := GuestRunner{
			Connector: &fakeGuestConnector{stream: stream},
			TempDir:   t.TempDir(),
		}.Run(ctx, Request{
			Run: ResolvedRun{
				RunID:       "run-1",
				TaskID:      "deploy",
				Bundle:      runtimeBundle(),
				Payload:     []byte(`{}`),
				Secrets:     api.ResolvedSecrets{},
				MaxDuration: time.Hour,
			},
			Artifact:        builder.Artifact{ImageTarPath: imagePath},
			TaskSource:      builder.Source{ProjectRoot: sourceRoot},
			WorkspaceSource: builder.Source{ProjectRoot: sourceRoot},
		})
		errs <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not return after context cancellation")
	}
	if !stream.isClosed() {
		t.Fatal("expected cancellation to close guest stream")
	}
}

func TestGuestRunnerArchivesProjectRootForSubpath(t *testing.T) {
	repoRoot := t.TempDir()
	appRoot := filepath.Join(repoRoot, "packages", "console")
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".git", "config"), []byte("repo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(appRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appRoot, "main.ts"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(t.TempDir(), "image.oci.tar")
	if err := os.WriteFile(imagePath, []byte("oci"), 0o644); err != nil {
		t.Fatal(err)
	}
	stream := newScriptedGuestStream(t, &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskComplete{TaskComplete: &runv0.TaskComplete{ExitCode: 0}},
	})
	_, err := GuestRunner{
		Connector: &fakeGuestConnector{stream: stream},
		TempDir:   t.TempDir(),
	}.Run(context.Background(), Request{
		Run: ResolvedRun{
			RunID:   "run-1",
			TaskID:  "deploy",
			Bundle:  runtimeBundle(),
			Payload: []byte(`{}`),
			Secrets: api.ResolvedSecrets{},
		},
		Artifact:        builder.Artifact{ImageTarPath: imagePath},
		TaskSource:      builder.Source{CheckoutRoot: repoRoot, ProjectRoot: appRoot},
		WorkspaceSource: builder.Source{CheckoutRoot: repoRoot, ProjectRoot: appRoot},
	})
	if err != nil {
		t.Fatal(err)
	}
	written := bytes.NewReader(stream.written.Bytes())
	imageHeader, imageLen, err := guest.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	if imageHeader.Type != guest.StreamTypeRunImage {
		t.Fatalf("image header = %+v", imageHeader)
	}
	_ = readExactly(t, written, imageLen)
	sourceHeader, sourceLen, err := guest.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	_ = readExactly(t, written, sourceLen)
	if sourceHeader.Type != guest.StreamTypeTaskSource {
		t.Fatalf("task source header = %+v", sourceHeader)
	}
	sourceHeader, sourceLen, err = guest.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	sourceBody := readExactly(t, written, sourceLen)
	if sourceHeader.Type != guest.StreamTypeWorkspaceSource {
		t.Fatalf("workspace source header = %+v", sourceHeader)
	}
	names := tarNames(t, sourceBody)
	if names[".git/config"] || names["packages/console/main.ts"] || !names["main.ts"] {
		t.Fatalf("source tar names = %+v", names)
	}
	var request runv0.RunTaskRequest
	if err := guest.ReadProtoFrame(written, &request); err != nil {
		t.Fatal(err)
	}
	if request.Cwd != "/workspace" {
		t.Fatalf("cwd = %q", request.Cwd)
	}
}

func TestGuestRunnerRejectsMissingResolvedSecrets(t *testing.T) {
	imagePath := filepath.Join(t.TempDir(), "image.oci.tar")
	if err := os.WriteFile(imagePath, []byte("oci"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runTaskRequest(Request{
		Run: ResolvedRun{
			RunID:   "run-1",
			TaskID:  "deploy",
			Bundle:  runtimeBundleWithSecret(),
			Payload: []byte(`{}`),
			Secrets: api.ResolvedSecrets{},
		},
		Artifact: builder.Artifact{ImageTarPath: imagePath},
	}, "sha256:"+string(bytes.Repeat([]byte{'0'}, 64)))
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("required")) {
		t.Fatalf("err = %v", err)
	}
}

func TestRuntimeWaitRequestRejectsOversizedDisplayText(t *testing.T) {
	oversized := strings.Repeat("x", maxWaitDisplayTextBytes+1)
	tests := []struct {
		name string
		wait *runv0.WaitRequested
		want string
	}{
		{
			name: "approval",
			wait: &runv0.WaitRequested{
				CorrelationId: "wait-1",
				Kind: &runv0.WaitRequested_Approval{Approval: &runv0.ApprovalWait{
					Message: oversized,
				}},
			},
			want: "approval message exceeds max",
		},
		{
			name: "message",
			wait: &runv0.WaitRequested{
				CorrelationId: "wait-1",
				Kind: &runv0.WaitRequested_Message{Message: &runv0.MessageWait{
					Prompt: stringPtr(oversized),
				}},
			},
			want: "message prompt exceeds max",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runtimeWaitRequest(Request{}, tt.wait)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCreateSourceTarCreatesTempDir(t *testing.T) {
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "main.ts"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	tempDir := filepath.Join(t.TempDir(), "missing", "tmp")

	sourceTar, cleanup, err := sourcetar.CreateTar(sourceRoot, tempDir)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := os.Stat(sourceTar.Path); err != nil {
		t.Fatal(err)
	}
}

type fakeGuestConnector struct {
	stream         io.ReadWriteCloser
	checkpointable bool
	restoreRequest vm.RestoreRequest
}

func (c *fakeGuestConnector) Connect(context.Context) (vm.Session, error) {
	if c.checkpointable {
		return fakeCheckpointableGuestSession{fakeGuestSession{stream: c.stream}}, nil
	}
	return fakeGuestSession{stream: c.stream}, nil
}

func (c *fakeGuestConnector) Restore(_ context.Context, request vm.RestoreRequest) (vm.Session, error) {
	c.restoreRequest = request
	return fakeGuestSession{stream: c.stream}, nil
}

type fakeGuestSession struct {
	stream io.ReadWriteCloser
}

func (s fakeGuestSession) Stream() io.ReadWriteCloser {
	return s.stream
}

func (s fakeGuestSession) Close() error {
	return s.stream.Close()
}

type fakeCheckpointableGuestSession struct {
	fakeGuestSession
}

func (s fakeCheckpointableGuestSession) CreateSnapshot(context.Context, vm.SnapshotRequest) (vm.SnapshotArtifact, error) {
	state, err := os.CreateTemp("", "helmr-test-state-*")
	if err != nil {
		return vm.SnapshotArtifact{}, err
	}
	defer state.Close()
	if _, err := state.WriteString("state"); err != nil {
		return vm.SnapshotArtifact{}, err
	}
	memory, err := os.CreateTemp("", "helmr-test-memory-*")
	if err != nil {
		return vm.SnapshotArtifact{}, err
	}
	defer memory.Close()
	if _, err := memory.WriteString("memory"); err != nil {
		return vm.SnapshotArtifact{}, err
	}
	return vm.SnapshotArtifact{
		RuntimeBackend: "test",
		RuntimeArch:    "test",
		RuntimeABI:     "test.v0",
		VMState:        vm.SnapshotFile{Path: state.Name(), MediaType: cas.CheckpointVMStateMediaType},
		Memory:         []vm.SnapshotFile{{Path: memory.Name(), MediaType: cas.CheckpointMemoryMediaType}},
		Manifest:       json.RawMessage(`{"runtime":{"backend":"test"}}`),
	}, nil
}

func (s fakeCheckpointableGuestSession) Resume(context.Context) error {
	return nil
}

type capturingWaitHandler struct {
	request WaitRequest
}

func (h *capturingWaitHandler) Wait(_ context.Context, request WaitRequest) error {
	h.request = request
	if request.Checkpointer != nil {
		_, err := request.Checkpointer.CreateCheckpoint(context.Background(), CheckpointRequest{
			RunID:        request.Lease.RunID,
			WaitpointID:  "waitpoint-1",
			CheckpointID: "checkpoint-1",
		})
		if err != nil {
			return err
		}
	}
	return ErrDetached
}

type capturedLogEvent struct {
	claim       api.WorkerRunLease
	stream      api.WorkerLogStream
	observedSeq uint64
	content     []byte
}

type capturedEmitEvent struct {
	claim     api.WorkerRunLease
	eventType string
	content   json.RawMessage
}

type capturingEventSink struct {
	logs    []capturedLogEvent
	entries []string
	emits   []capturedEmitEvent
}

func (s *capturingEventSink) AppendLog(_ context.Context, claim api.WorkerRunLease, stream api.WorkerLogStream, observedSeq uint64, content []byte) (api.WorkerEventResponse, error) {
	s.logs = append(s.logs, capturedLogEvent{
		claim:       claim,
		stream:      stream,
		observedSeq: observedSeq,
		content:     append([]byte(nil), content...),
	})
	return api.WorkerEventResponse{RunID: claim.RunID}, nil
}

func (s *capturingEventSink) RecordLogEntry(_ context.Context, claim api.WorkerRunLease, entry string) (api.WorkerEventResponse, error) {
	s.entries = append(s.entries, entry)
	return api.WorkerEventResponse{RunID: claim.RunID}, nil
}

func (s *capturingEventSink) EmitEvent(_ context.Context, claim api.WorkerRunLease, eventType string, content json.RawMessage) (api.WorkerEventResponse, error) {
	s.emits = append(s.emits, capturedEmitEvent{
		claim:     claim,
		eventType: eventType,
		content:   append(json.RawMessage(nil), content...),
	})
	return api.WorkerEventResponse{RunID: claim.RunID}, nil
}

type scriptedGuestStream struct {
	read    *bytes.Reader
	written bytes.Buffer
}

func newScriptedGuestStream(t *testing.T, messages ...proto.Message) *scriptedGuestStream {
	t.Helper()
	var read bytes.Buffer
	for _, message := range messages {
		body, err := proto.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := guest.WriteMessageFrame(&read, body); err != nil {
			t.Fatal(err)
		}
	}
	return &scriptedGuestStream{read: bytes.NewReader(read.Bytes())}
}

func (s *scriptedGuestStream) Read(p []byte) (int, error)  { return s.read.Read(p) }
func (s *scriptedGuestStream) Write(p []byte) (int, error) { return s.written.Write(p) }
func (s *scriptedGuestStream) Close() error                { return nil }

type blockingGuestStream struct {
	written bytes.Buffer
	closed  chan struct{}
	once    sync.Once
}

func newBlockingGuestStream() *blockingGuestStream {
	return &blockingGuestStream{closed: make(chan struct{})}
}

func (s *blockingGuestStream) Read([]byte) (int, error) {
	<-s.closed
	return 0, io.ErrClosedPipe
}

func (s *blockingGuestStream) Write(p []byte) (int, error) {
	return s.written.Write(p)
}

func (s *blockingGuestStream) Close() error {
	s.once.Do(func() {
		close(s.closed)
	})
	return nil
}

func (s *blockingGuestStream) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

type fakeCAS struct {
	mediaType string
	content   []byte
	objects   map[string][]byte
}

func (f *fakeCAS) Put(_ context.Context, mediaType string, body io.Reader) (cas.Object, error) {
	content, err := io.ReadAll(body)
	if err != nil {
		return cas.Object{}, err
	}
	f.mediaType = mediaType
	f.content = content
	return cas.Object{Digest: cas.DigestBytes(content), SizeBytes: int64(len(content)), MediaType: mediaType}, nil
}

func (f *fakeCAS) Stat(context.Context, string) (cas.Object, error) {
	return cas.Object{}, nil
}

func (f *fakeCAS) Get(_ context.Context, digest string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.objects[digest])), nil
}

func (f *fakeCAS) Delete(context.Context, string) error {
	return nil
}

type encryptedCheckpoint struct {
	digest string
	body   []byte
}

func testCheckpointEncryptor(t *testing.T) *checkpoint.Encryptor {
	t.Helper()
	encryptor, err := checkpoint.New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return encryptor
}

func encryptedCheckpointObject(t *testing.T, encryptor *checkpoint.Encryptor, plaintext []byte, suffix string) encryptedCheckpoint {
	t.Helper()
	var body bytes.Buffer
	if err := encryptor.Encrypt(context.Background(), bytes.NewReader(plaintext), &body, checkpointPurpose(suffix)); err != nil {
		t.Fatal(err)
	}
	encrypted := body.Bytes()
	return encryptedCheckpoint{digest: cas.DigestBytes(encrypted), body: encrypted}
}

func stringPtr(value string) *string {
	return &value
}

func withCheckpointManifest(checkpoint api.WorkerCheckpointManifest, edit func(*api.WorkerCheckpointManifest)) api.WorkerCheckpointManifest {
	edit(&checkpoint)
	return checkpoint
}

func runtimeBundle() *bundlev0.Bundle {
	return &bundlev0.Bundle{
		Sandbox: &bundlev0.SandboxSpec{Workspace: &bundlev0.WorkspaceRuntimeBinding{MountPath: "/workspace"}},
		Task:    &bundlev0.TaskSpec{ModulePath: "src/task.ts"},
	}
}

func runtimeBundleWithSecret() *bundlev0.Bundle {
	bundle := runtimeBundle()
	bundle.Task.Secrets = []*bundlev0.SecretPlacement{{
		Name: "TOKEN",
		Placement: &bundlev0.Placement{Kind: &bundlev0.Placement_Env{
			Env: &bundlev0.EnvPlacement{Name: "TOKEN"},
		}},
	}}
	return bundle
}

func readExactly(t *testing.T, reader io.Reader, size uint64) []byte {
	t.Helper()
	body, err := io.ReadAll(io.LimitReader(reader, int64(size)))
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(body)) != size {
		t.Fatalf("read %d bytes, want %d", len(body), size)
	}
	return body
}

func tarNames(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	reader := tar.NewReader(bytes.NewReader(body))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names[header.Name] = true
	}
}
