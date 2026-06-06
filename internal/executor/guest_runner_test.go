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
	"github.com/helmrdotdev/helmr/internal/archive"
	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkout"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/compute"
	bundlev0 "github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/transport"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workspace"
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
	stream := newScriptedCheckpointGuestStream(t, &runv0.RunEvent{
		Event: &runv0.RunEvent_LogEntry{LogEntry: "building"},
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_StdoutChunk{StdoutChunk: []byte("hello\n")},
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_EmitEvent{EmitEvent: &runv0.EmitEvent{Type: "deploy.progress", ContentJson: `{"broken":secret}`}},
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_StderrChunk{StderrChunk: []byte("warn\n")},
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskResult{TaskResult: &runv0.TaskResult{ExitCode: 7}},
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
		Artifact:         builder.Artifact{ImageTarPath: imagePath},
		DeploymentSource: builder.Source{ProjectRoot: sourceRoot},
		Workspace:        testWorkspaceArtifact(t, sourceRoot, sourceRoot),
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
	imageHeader, imageLen, err := transport.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	imageBody := readExactly(t, written, imageLen)
	if imageHeader.Type != transport.StreamTypeRunImage || imageHeader.RunID != "run-1" || string(imageBody) != "oci" {
		t.Fatalf("image header = %+v body = %q", imageHeader, imageBody)
	}

	taskHeader, taskLen, err := transport.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	taskBody := readExactly(t, written, taskLen)
	if taskHeader.Type != transport.StreamTypeDeploymentSource || taskHeader.RunID != "run-1" {
		t.Fatalf("deployment source header = %+v", taskHeader)
	}
	taskNames := tarNames(t, taskBody)
	if taskNames[".git/config"] {
		t.Fatalf("deployment tar archive included checkout metadata: %v", taskNames)
	}
	var request runv0.RunTaskRequest
	if err := transport.ReadProtoFrame(written, &request); err != nil {
		t.Fatal(err)
	}
	if request.RunId != "run-1" || request.TaskId != "deploy" || request.ModulePath != "src/task.ts" || request.Cwd != "/workspace" {
		t.Fatalf("request = %+v", &request)
	}
	if request.Workspace == nil || request.Workspace.Path != "/workspace" || request.Workspace.ProjectPath != "/workspace" {
		t.Fatalf("workspace = %+v", request.Workspace)
	}
	if request.Workspace.Artifact == nil || request.Workspace.Artifact.Encoding != "tar" || !request.Workspace.Writable {
		t.Fatalf("workspace volume = %+v", request.Workspace)
	}
	sourceHeader, sourceLen, err := transport.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	sourceBody := readExactly(t, written, sourceLen)
	if sourceHeader.Type != transport.StreamTypeWorkspaceArtifact || sourceHeader.RunID != "run-1" {
		t.Fatalf("workspace artifact header = %+v", sourceHeader)
	}
	sourceNames := tarNames(t, sourceBody)
	if sourceNames[".git/config"] {
		t.Fatalf("workspace artifact included checkout metadata: %v", sourceNames)
	}
	if !sourceNames["main.ts"] || !sourceNames[".env"] {
		t.Fatalf("workspace artifact names = %v", sourceNames)
	}
	if request.Workspace.Artifact.SizeBytes != sourceLen || request.Workspace.Artifact.EntryCount == 0 {
		t.Fatalf("workspace artifact metadata = %+v sourceLen=%d", request.Workspace.Artifact, sourceLen)
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
	stream := newScriptedCheckpointGuestStream(t, &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskResult{TaskResult: &runv0.TaskResult{ExitCode: 0, OutputJson: &outputJSON}},
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
		Artifact:         builder.Artifact{ImageTarPath: imagePath},
		DeploymentSource: builder.Source{ProjectRoot: sourceRoot},
		Workspace:        testWorkspaceArtifact(t, sourceRoot, sourceRoot),
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
	stream := newScriptedCheckpointGuestStream(t, &runv0.RunEvent{
		Event: &runv0.RunEvent_WaitRequested{WaitRequested: &runv0.WaitRequested{
			CorrelationId: "approval-1",
			Kind:          "human",
			RequestJson:   `{}`,
			DisplayText:   new("ship it"),
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
		Artifact:         builder.Artifact{ImageTarPath: imagePath},
		DeploymentSource: builder.Source{ProjectRoot: sourceRoot},
		Workspace:        testWorkspaceArtifact(t, sourceRoot, sourceRoot),
		WaitHandler:      waiter,
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

func TestGuestRunnerProcessesRunEventsBeforeCheckpointPauseReady(t *testing.T) {
	imagePath := filepath.Join(t.TempDir(), "image.oci.tar")
	if err := os.WriteFile(imagePath, []byte("oci"), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "main.ts"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	stream := newScriptedCheckpointGuestStream(t, &runv0.RunEvent{
		Event: &runv0.RunEvent_WaitRequested{WaitRequested: &runv0.WaitRequested{
			CorrelationId: "approval-1",
			Kind:          "human",
			RequestJson:   `{}`,
			DisplayText:   new("ship it"),
		}},
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_StdoutChunk{StdoutChunk: []byte("x")},
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_LogEntry{LogEntry: "checkpoint flush"},
	}, &runv0.PauseReady{
		WaitpointId:  "waitpoint-1",
		CheckpointId: "checkpoint-1",
	})
	waiter := &capturingWaitHandler{}
	store := &fakeCAS{}
	events := &capturingEventSink{}
	var stdout bytes.Buffer
	result, err := GuestRunner{
		Connector:           &fakeGuestConnector{stream: stream, checkpointable: true},
		CAS:                 store,
		CheckpointEncryptor: testCheckpointEncryptor(t),
		TempDir:             t.TempDir(),
		Events:              events,
		Stdout:              &stdout,
	}.Run(context.Background(), Request{
		Lease: api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"},
		Run: ResolvedRun{
			RunID:   "run-1",
			TaskID:  "deploy",
			Bundle:  runtimeBundle(),
			Payload: []byte(`{}`),
			Secrets: api.ResolvedSecrets{},
		},
		Artifact:         builder.Artifact{ImageTarPath: imagePath},
		DeploymentSource: builder.Source{ProjectRoot: sourceRoot},
		Workspace:        testWorkspaceArtifact(t, sourceRoot, sourceRoot),
		WaitHandler:      waiter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Detached {
		t.Fatalf("result = %+v, want detached", result)
	}
	if stdout.String() != "x" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if len(events.logs) != 1 || events.logs[0].observedSeq != 2 || string(events.logs[0].content) != "x" {
		t.Fatalf("logs = %+v", events.logs)
	}
	if len(events.entries) != 1 || events.entries[0] != "checkpoint flush" {
		t.Fatalf("entries = %+v", events.entries)
	}
	if waiter.request.Checkpointer == nil {
		t.Fatal("wait request did not include checkpointer")
	}
}

func TestGuestRunnerRestoresCheckpointAndAttachesWaitpoint(t *testing.T) {
	state := []byte("state")
	scratch := []byte("scratch")
	memory := []byte("memory")
	manifest := []byte(`{"checkpoint_id":"checkpoint-1","runtime":{"backend":"firecracker"}}`)
	encryptor := testCheckpointEncryptor(t)
	manifestObject := encryptedCheckpointObject(t, encryptor, manifest, "manifest")
	stateObject := encryptedCheckpointObject(t, encryptor, state, "vmstate")
	scratchObject := encryptedCheckpointObject(t, encryptor, scratch, "scratch-disk")
	memoryObject := encryptedCheckpointObject(t, encryptor, memory, "memory")
	stream := newScriptedCheckpointGuestStream(t, &runv0.ResumeAck{
		WaitpointId: "waitpoint-1",
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskResult{TaskResult: &runv0.TaskResult{ExitCode: 0}},
	})
	connector := &fakeGuestConnector{stream: stream}
	waiter := &capturingWaitHandler{}
	result, err := GuestRunner{
		Connector:           connector,
		CAS:                 &fakeCAS{objects: map[string][]byte{manifestObject.digest: manifestObject.body, stateObject.digest: stateObject.body, scratchObject.digest: scratchObject.body, memoryObject.digest: memoryObject.body}},
		CheckpointEncryptor: encryptor,
		TempDir:             t.TempDir(),
	}.Run(context.Background(), Request{
		Lease:       api.WorkerRunLease{ID: "execution-1", RunID: "run-1", WorkerInstanceID: "worker-1"},
		WaitHandler: waiter,
		Run: ResolvedRun{
			RunID: "run-1",
			Restore: &api.WorkerRestore{
				CheckpointID: "checkpoint-1",
				Checkpoint:   testRestoreCheckpointManifest(manifest, manifestObject, stateObject, scratchObject, memoryObject, testCheckpointWorkspaceBase()),
				Waitpoint: api.WorkerRestoreWaitpoint{
					ID:                "waitpoint-1",
					RunWaitID:         "run-wait-1",
					ResumeKind:        "completed",
					ResumePayloadJSON: json.RawMessage(`{"value":{"approved":true}}`),
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || connector.restoreRequest.ID != "checkpoint-1" || connector.restoreRequest.ScratchDisk == "" || len(connector.restoreRequest.Memory) != 1 {
		t.Fatalf("result=%+v restore=%+v", result, connector.restoreRequest)
	}
	if !bytes.Equal(connector.restoreRequest.Manifest, manifest) {
		t.Fatalf("restore manifest = %s", connector.restoreRequest.Manifest)
	}
	written := bytes.NewReader(stream.written.Bytes())
	var attach runv0.ResumeAttach
	if err := transport.ReadProtoFrame(written, &attach); err != nil {
		t.Fatal(err)
	}
	if attach.CheckpointId != "checkpoint-1" || attach.WaitpointId != "waitpoint-1" || attach.SessionId != "execution-1" {
		t.Fatalf("attach = %+v", &attach)
	}
	var decision runv0.ResumeDecision
	if err := transport.ReadProtoFrame(written, &decision); err != nil {
		t.Fatal(err)
	}
	if decision.WaitpointId != "waitpoint-1" || decision.Kind != "completed" || decision.ResumePayloadJson != `{"value":{"approved":true}}` {
		t.Fatalf("decision = %+v", &decision)
	}
	if waiter.acknowledged.RunWaitID != "run-wait-1" || waiter.acknowledged.WaitpointID != "waitpoint-1" || waiter.acknowledged.CheckpointID != "checkpoint-1" {
		t.Fatalf("acknowledged = %+v", waiter.acknowledged)
	}
}

func TestGuestRunnerRequiresRestoreAcknowledgerBeforeResumeAttach(t *testing.T) {
	stream := newScriptedCheckpointGuestStream(t, &runv0.ResumeAck{WaitpointId: "waitpoint-1"})
	session := fakeGuestSession{stream: stream}
	err := GuestRunner{}.attachAndAcknowledgeRestore(context.Background(), session, Request{
		Lease:       api.WorkerRunLease{ID: "execution-1", RunID: "run-1", WorkerInstanceID: "worker-1"},
		WaitHandler: waitOnlyHandler{},
		Run: ResolvedRun{
			Restore: &api.WorkerRestore{
				CheckpointID: "checkpoint-1",
				Waitpoint: api.WorkerRestoreWaitpoint{
					ID:                "waitpoint-1",
					RunWaitID:         "run-wait-1",
					ResumeKind:        "completed",
					ResumePayloadJSON: json.RawMessage(`{"value":{"approved":true}}`),
				},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "restore acknowledger is required") {
		t.Fatalf("err = %v, want restore acknowledger", err)
	}
	if stream.written.Len() != 0 {
		t.Fatalf("resume attach was written before acknowledger validation: %x", stream.written.Bytes())
	}
}

type waitOnlyHandler struct{}

func (waitOnlyHandler) Wait(context.Context, WaitRequest) error {
	return ErrDetached
}

func TestGuestRunnerRestoredCheckpointCarriesWorkspaceBaseIntoNextCheckpoint(t *testing.T) {
	state := []byte("state")
	scratch := []byte("scratch")
	memory := []byte("memory")
	manifest := []byte(`{"checkpoint_id":"checkpoint-1","runtime":{"backend":"firecracker"}}`)
	encryptor := testCheckpointEncryptor(t)
	manifestObject := encryptedCheckpointObject(t, encryptor, manifest, "manifest")
	stateObject := encryptedCheckpointObject(t, encryptor, state, "vmstate")
	scratchObject := encryptedCheckpointObject(t, encryptor, scratch, "scratch-disk")
	memoryObject := encryptedCheckpointObject(t, encryptor, memory, "memory")
	workspaceBase := testCheckpointWorkspaceBase()
	stream := newScriptedCheckpointGuestStream(t, &runv0.ResumeAck{
		WaitpointId: "waitpoint-1",
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_WaitRequested{WaitRequested: &runv0.WaitRequested{
			CorrelationId: "next-waitpoint",
			Kind:          "human",
			RequestJson:   `{}`,
			DisplayText:   new("continue?"),
		}},
	}, &runv0.PauseReady{
		WaitpointId:  "waitpoint-1",
		CheckpointId: "checkpoint-1",
	})
	connector := &fakeGuestConnector{stream: stream, checkpointable: true}
	waiter := &capturingWaitHandler{}
	result, err := GuestRunner{
		Connector:           connector,
		CAS:                 &fakeCAS{objects: map[string][]byte{manifestObject.digest: manifestObject.body, stateObject.digest: stateObject.body, scratchObject.digest: scratchObject.body, memoryObject.digest: memoryObject.body}},
		CheckpointEncryptor: encryptor,
		TempDir:             t.TempDir(),
	}.Run(context.Background(), Request{
		Lease:       api.WorkerRunLease{ID: "execution-1", RunID: "run-1", WorkerInstanceID: "worker-1"},
		WaitHandler: waiter,
		Run: ResolvedRun{
			RunID: "run-1",
			Restore: &api.WorkerRestore{
				CheckpointID: "checkpoint-1",
				Checkpoint:   testRestoreCheckpointManifest(manifest, manifestObject, stateObject, scratchObject, memoryObject, workspaceBase),
				Waitpoint: api.WorkerRestoreWaitpoint{
					ID:                "waitpoint-1",
					RunWaitID:         "run-wait-1",
					ResumeKind:        "completed",
					ResumePayloadJSON: json.RawMessage(`{"value":{"approved":true}}`),
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Detached {
		t.Fatalf("result = %+v, want detached", result)
	}
	if waiter.manifest.WorkspaceState.Base != workspaceBase {
		t.Fatalf("checkpoint workspace base = %+v, want %+v", waiter.manifest.WorkspaceState.Base, workspaceBase)
	}
}

func TestValidateRestoreIdentity(t *testing.T) {
	valid := api.WorkerCheckpointManifest{
		RecoveryPoint: api.WorkerCheckpointRecoveryPoint{Runtime: api.WorkerCheckpointRuntime{
			Backend:         "firecracker",
			ID:              "sha256:runtime",
			Arch:            runtime.GOARCH,
			ABI:             "helmr.firecracker.snapshot.v0",
			KernelDigest:    "sha256:kernel",
			InitramfsDigest: "sha256:initramfs",
			RootfsDigest:    "sha256:rootfs",
			ConfigDigest:    "sha256:runtime-config",
		}},
		RuntimeState: api.WorkerCheckpointRuntimeState{
			ConfigArtifact: api.WorkerCheckpointArtifact{Digest: "sha256:manifest", MediaType: cas.CheckpointRuntimeConfigMediaType},
		},
	}
	tests := []struct {
		name       string
		checkpoint api.WorkerCheckpointManifest
		want       string
	}{
		{name: "valid", checkpoint: valid},
		{name: "backend", checkpoint: withCheckpointManifest(valid, func(c *api.WorkerCheckpointManifest) { c.RecoveryPoint.Runtime.Backend = "test" }), want: `recovery_point.runtime.backend "test" is not supported`},
		{name: "arch", checkpoint: withCheckpointManifest(valid, func(c *api.WorkerCheckpointManifest) { c.RecoveryPoint.Runtime.Arch = "other" }), want: `recovery_point.runtime.arch "other" does not match`},
		{name: "abi", checkpoint: withCheckpointManifest(valid, func(c *api.WorkerCheckpointManifest) { c.RecoveryPoint.Runtime.ABI = "" }), want: "recovery_point.runtime.abi is required"},
		{name: "id", checkpoint: withCheckpointManifest(valid, func(c *api.WorkerCheckpointManifest) { c.RecoveryPoint.Runtime.ID = "" }), want: "recovery_point.runtime.id is required"},
		{name: "kernel", checkpoint: withCheckpointManifest(valid, func(c *api.WorkerCheckpointManifest) { c.RecoveryPoint.Runtime.KernelDigest = "" }), want: "recovery_point.runtime.kernel_digest is required"},
		{name: "initramfs", checkpoint: withCheckpointManifest(valid, func(c *api.WorkerCheckpointManifest) { c.RecoveryPoint.Runtime.InitramfsDigest = "" }), want: "recovery_point.runtime.initramfs_digest is required"},
		{name: "rootfs", checkpoint: withCheckpointManifest(valid, func(c *api.WorkerCheckpointManifest) { c.RecoveryPoint.Runtime.RootfsDigest = " " }), want: "recovery_point.runtime.rootfs_digest is required"},
		{name: "config", checkpoint: withCheckpointManifest(valid, func(c *api.WorkerCheckpointManifest) { c.RecoveryPoint.Runtime.ConfigDigest = "" }), want: "recovery_point.runtime.config_digest is required"},
		{name: "manifest", checkpoint: withCheckpointManifest(valid, func(c *api.WorkerCheckpointManifest) { c.RuntimeState.ConfigArtifact = api.WorkerCheckpointArtifact{} }), want: "runtime_state.config_artifact.digest is required"},
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
		Artifact:         builder.Artifact{ImageTarPath: imagePath},
		DeploymentSource: builder.Source{ProjectRoot: sourceRoot},
		Workspace:        testWorkspaceArtifact(t, sourceRoot, sourceRoot),
	})
	if err == nil || !strings.Contains(err.Error(), "max_duration") {
		t.Fatalf("err = %v", err)
	}
	if !stream.isClosed() {
		t.Fatal("expected timeout to close guest stream")
	}
}

func TestGuestRunnerTreatsTaskResultErrorMessageAsRuntimeFailure(t *testing.T) {
	imagePath := filepath.Join(t.TempDir(), "image.oci.tar")
	if err := os.WriteFile(imagePath, []byte("oci"), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "main.ts"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	stream := newScriptedGuestStream(t, &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskResult{TaskResult: &runv0.TaskResult{
			ExitCode:     1,
			ErrorMessage: new("read adapter control event: malformed frame"),
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
		Artifact:         builder.Artifact{ImageTarPath: imagePath},
		DeploymentSource: builder.Source{ProjectRoot: sourceRoot},
		Workspace:        testWorkspaceArtifact(t, sourceRoot, sourceRoot),
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
			Artifact:         builder.Artifact{ImageTarPath: imagePath},
			DeploymentSource: builder.Source{ProjectRoot: sourceRoot},
			Workspace:        testWorkspaceArtifact(t, sourceRoot, sourceRoot),
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
		Event: &runv0.RunEvent_TaskResult{TaskResult: &runv0.TaskResult{ExitCode: 0}},
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
		Artifact:         builder.Artifact{ImageTarPath: imagePath},
		DeploymentSource: builder.Source{CheckoutRoot: repoRoot, ProjectRoot: appRoot},
		Workspace:        testWorkspaceArtifact(t, repoRoot, appRoot),
	})
	if err != nil {
		t.Fatal(err)
	}
	written := bytes.NewReader(stream.written.Bytes())
	imageHeader, imageLen, err := transport.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	if imageHeader.Type != transport.StreamTypeRunImage {
		t.Fatalf("image header = %+v", imageHeader)
	}
	_ = readExactly(t, written, imageLen)
	sourceHeader, sourceLen, err := transport.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	_ = readExactly(t, written, sourceLen)
	if sourceHeader.Type != transport.StreamTypeDeploymentSource {
		t.Fatalf("deployment source header = %+v", sourceHeader)
	}
	var request runv0.RunTaskRequest
	if err := transport.ReadProtoFrame(written, &request); err != nil {
		t.Fatal(err)
	}
	sourceHeader, sourceLen, err = transport.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	sourceBody := readExactly(t, written, sourceLen)
	if sourceHeader.Type != transport.StreamTypeWorkspaceArtifact {
		t.Fatalf("workspace artifact header = %+v", sourceHeader)
	}
	names := tarNames(t, sourceBody)
	if names[".git/config"] || names["packages/console/main.ts"] || !names["main.ts"] {
		t.Fatalf("workspace artifact names = %+v", names)
	}
	if request.Cwd != "/workspace" {
		t.Fatalf("cwd = %q", request.Cwd)
	}
	if request.Workspace == nil || request.Workspace.Path != "/workspace" || request.Workspace.ProjectPath != "/workspace" {
		t.Fatalf("workspace = %+v", request.Workspace)
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
		Workspace: checkout.WorkspaceArtifact{
			Digest:     "sha256:" + string(bytes.Repeat([]byte{'0'}, 64)),
			MediaType:  workspace.ArtifactMediaType,
			Encoding:   workspace.ArtifactEncoding,
			VolumeKind: workspace.VolumeKind,
		},
	})
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
				Kind:          "human",
				RequestJson:   `{}`,
				DisplayText:   &oversized,
			},
			want: "display_text exceeds max",
		},
		{
			name: "message",
			wait: &runv0.WaitRequested{
				CorrelationId: "wait-1",
				Kind:          "human",
				RequestJson:   `{}`,
				DisplayText:   &oversized,
			},
			want: "display_text exceeds max",
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

	sourceTar, cleanup, err := archive.CreateTar(sourceRoot, tempDir)
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
	network        compute.NetworkPolicy
	restoreRequest vm.RestoreRequest
}

func (c *fakeGuestConnector) Connect(_ context.Context, network compute.NetworkPolicy) (vm.Session, error) {
	c.network = network
	if c.checkpointable {
		return fakeCheckpointableGuestSession{fakeGuestSession{stream: c.stream}}, nil
	}
	return fakeGuestSession{stream: c.stream}, nil
}

func (c *fakeGuestConnector) Restore(_ context.Context, request vm.RestoreRequest) (vm.Session, error) {
	c.restoreRequest = request
	if c.checkpointable {
		return fakeCheckpointableGuestSession{fakeGuestSession{stream: c.stream}}, nil
	}
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
	scratch, err := os.CreateTemp("", "helmr-test-scratch-*")
	if err != nil {
		return vm.SnapshotArtifact{}, err
	}
	defer scratch.Close()
	if _, err := scratch.WriteString("scratch"); err != nil {
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
		ScratchDisk:    vm.SnapshotFile{Path: scratch.Name(), MediaType: cas.CheckpointScratchDiskMediaType},
		Memory:         []vm.SnapshotFile{{Path: memory.Name(), MediaType: cas.CheckpointMemoryMediaType}},
		Manifest:       json.RawMessage(`{"runtime":{"backend":"test"}}`),
	}, nil
}

func (s fakeCheckpointableGuestSession) Resume(context.Context) error {
	return nil
}

type capturingWaitHandler struct {
	request      WaitRequest
	manifest     api.WorkerCheckpointManifest
	acknowledged RestoreAcknowledgement
}

func (h *capturingWaitHandler) Wait(_ context.Context, request WaitRequest) error {
	h.request = request
	if request.Checkpointer != nil {
		manifest, err := request.Checkpointer.CreateCheckpoint(context.Background(), CheckpointRequest{
			RunID:        request.Lease.RunID,
			WaitpointID:  "waitpoint-1",
			CheckpointID: "checkpoint-1",
		})
		if err != nil {
			return err
		}
		h.manifest = manifest
	}
	return ErrDetached
}

func (h *capturingWaitHandler) AcknowledgeRestore(_ context.Context, request RestoreAcknowledgement) error {
	h.acknowledged = request
	return nil
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
		if ready, ok := message.(*runv0.PauseReady); ok {
			if err := transport.WriteStreamFrameHeader(&read, transport.StreamHeader{
				Type:         transport.StreamTypeCheckpointPauseReady,
				WaitpointID:  ready.WaitpointId,
				CheckpointID: ready.CheckpointId,
			}, 0); err != nil {
				t.Fatal(err)
			}
			continue
		}
		body, err := proto.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := transport.WriteMessageFrame(&read, body); err != nil {
			t.Fatal(err)
		}
	}
	return &scriptedGuestStream{read: bytes.NewReader(read.Bytes())}
}

func newScriptedCheckpointGuestStream(t *testing.T, messages ...proto.Message) *scriptedGuestStream {
	t.Helper()
	var read bytes.Buffer
	for _, message := range messages {
		if ready, ok := message.(*runv0.PauseReady); ok {
			if err := transport.WriteStreamFrameHeader(&read, transport.StreamHeader{
				Type:         transport.StreamTypeCheckpointPauseReady,
				WaitpointID:  ready.WaitpointId,
				CheckpointID: ready.CheckpointId,
			}, 0); err != nil {
				t.Fatal(err)
			}
			continue
		}
		body, err := proto.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		if err := transport.WriteMessageFrame(&read, body); err != nil {
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
	mu        sync.Mutex
	mediaType string
	content   []byte
	objects   map[string][]byte
}

func (f *fakeCAS) Put(_ context.Context, mediaType string, body io.Reader) (cas.Object, error) {
	content, err := io.ReadAll(body)
	if err != nil {
		return cas.Object{}, err
	}
	return f.put(mediaType, content), nil
}

func (f *fakeCAS) Stage(_ context.Context, mediaType string) (cas.Stage, error) {
	return &fakeCASStage{store: f, mediaType: mediaType}, nil
}

func (f *fakeCAS) put(mediaType string, content []byte) cas.Object {
	object := cas.Object{Digest: cas.DigestBytes(content), SizeBytes: int64(len(content)), MediaType: mediaType}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mediaType = mediaType
	f.content = append([]byte(nil), content...)
	if f.objects != nil {
		f.objects[object.Digest] = append([]byte(nil), content...)
	}
	return object
}

type fakeCASStage struct {
	store     *fakeCAS
	mediaType string
	content   bytes.Buffer
	closed    bool
}

func (s *fakeCASStage) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("stage is closed")
	}
	return s.content.Write(p)
}

func (s *fakeCASStage) Close() error {
	s.closed = true
	return nil
}

func (s *fakeCASStage) Commit(context.Context) (cas.Object, error) {
	s.closed = true
	return s.store.put(s.mediaType, s.content.Bytes()), nil
}

func (s *fakeCASStage) Abort(context.Context) error {
	s.closed = true
	return nil
}

func (f *fakeCAS) Stat(context.Context, string) (cas.Object, error) {
	return cas.Object{}, nil
}

func (f *fakeCAS) Get(_ context.Context, digest string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return io.NopCloser(bytes.NewReader(append([]byte(nil), f.objects[digest]...))), nil
}

func (f *fakeCAS) Delete(context.Context, string) error {
	return nil
}

type encryptedCheckpoint struct {
	digest string
	body   []byte
}

func testRestoreCheckpointManifest(config []byte, configObject encryptedCheckpoint, stateObject encryptedCheckpoint, scratchObject encryptedCheckpoint, memoryObject encryptedCheckpoint, workspaceBase api.WorkerCheckpointWorkspaceBase) api.WorkerCheckpointManifest {
	return api.WorkerCheckpointManifest{
		RecoveryPoint: api.WorkerCheckpointRecoveryPoint{
			ID: "checkpoint-1",
			Runtime: api.WorkerCheckpointRuntime{
				Backend:         "firecracker",
				ID:              "sha256:runtime",
				Arch:            runtime.GOARCH,
				ABI:             "helmr.firecracker.snapshot.v0",
				KernelDigest:    "sha256:kernel",
				InitramfsDigest: "sha256:initramfs",
				RootfsDigest:    "sha256:rootfs",
				ConfigDigest:    "sha256:runtime-config",
			},
		},
		RuntimeState: api.WorkerCheckpointRuntimeState{
			ConfigArtifact:      api.WorkerCheckpointArtifact{Digest: configObject.digest, MediaType: cas.CheckpointRuntimeConfigMediaType},
			VMStateArtifact:     api.WorkerCheckpointArtifact{Digest: stateObject.digest, MediaType: cas.CheckpointVMStateMediaType},
			ScratchDiskArtifact: api.WorkerCheckpointArtifact{Digest: scratchObject.digest, MediaType: cas.CheckpointScratchDiskMediaType},
			MemoryArtifacts:     []api.WorkerCheckpointArtifact{{Digest: memoryObject.digest, MediaType: cas.CheckpointMemoryMediaType}},
			Config:              append([]byte(nil), config...),
		},
		WorkspaceState: api.WorkerCheckpointWorkspaceState{
			Base: workspaceBase,
		},
	}
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

const testResolvedSHA = "0123456789abcdef0123456789abcdef01234567"

func testWorkspaceArtifact(t *testing.T, checkoutRoot, projectRoot string) checkout.WorkspaceArtifact {
	t.Helper()
	artifact, cleanup, err := checkout.CreateWorkspaceArtifact(checkout.Worktree{
		CheckoutRoot: checkoutRoot,
		ProjectRoot:  projectRoot,
		SHA:          testResolvedSHA,
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	return artifact
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
