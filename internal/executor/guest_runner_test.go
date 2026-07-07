package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
	"github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/substrate"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
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
	}.runDirect(context.Background(), Request{
		Leases: staticLease(claim),
		Run: ResolvedRun{
			RunID:     "run-1",
			SessionID: "session-1",
			TaskID:    "deploy",
			Bundle:    runtimeBundle(),
			Payload:   []byte(`{"ok":true}`),
			Secrets:   api.ResolvedSecrets{},
			Trace: api.TraceContext{
				TraceID:     "0123456789abcdef0123456789abcdef",
				SpanID:      "0123456789abcdef",
				Traceparent: "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01",
			},
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
	if result.ActiveDuration < 0 || result.ActiveDuration > time.Minute {
		t.Fatalf("active duration = %s", result.ActiveDuration)
	}
	if stderr.String() != "warn\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if len(events.logs) != 2 || events.logs[0].stream != api.WorkerLogStreamStdout || events.logs[0].observedSeq != 2 || string(events.logs[0].content) != "hello\n" {
		t.Fatalf("stdout event = %+v", events.logs)
	}
	if events.logs[1].stream != api.WorkerLogStreamStderr || events.logs[1].observedSeq != 3 || string(events.logs[1].content) != "warn\n" {
		t.Fatalf("stderr event = %+v", events.logs)
	}
	if len(events.entries) != 1 || events.entries[0] != "building" {
		t.Fatalf("log entries = %+v", events.entries)
	}
	written := bytes.NewReader(stream.written.Bytes())
	imageHeader, imageLen, err := wire.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	imageBody := readExactly(t, written, imageLen)
	if imageHeader.Type != wire.StreamTypeRunImage || imageHeader.RunID != "run-1" || string(imageBody) != "oci" {
		t.Fatalf("image header = %+v body = %q", imageHeader, imageBody)
	}

	taskHeader, taskLen, err := wire.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	taskBody := readExactly(t, written, taskLen)
	if taskHeader.Type != wire.StreamTypeDeploymentSource || taskHeader.RunID != "run-1" {
		t.Fatalf("deployment source header = %+v", taskHeader)
	}
	taskNames := tarNames(t, taskBody)
	if taskNames[".git/config"] {
		t.Fatalf("deployment tar archive included checkout metadata: %v", taskNames)
	}
	var request runv0.RunTaskRequest
	if err := frameio.ReadProtoFrame(written, &request); err != nil {
		t.Fatal(err)
	}
	if request.RunId != "run-1" || request.TaskId != "deploy" || request.ModulePath != "src/task.ts" || request.Cwd != "/workspace" {
		t.Fatalf("request = %+v", &request)
	}
	if request.Trace == nil || request.Trace.TraceId != "0123456789abcdef0123456789abcdef" || request.Trace.SpanId != "0123456789abcdef" || request.Trace.Traceparent != "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01" {
		t.Fatalf("trace = %+v", request.Trace)
	}
	if request.Workspace == nil || request.Workspace.Path != "/workspace" || request.Workspace.ProjectPath != "/workspace" {
		t.Fatalf("workspace = %+v", request.Workspace)
	}
	if request.Workspace.Artifact == nil || request.Workspace.Artifact.Encoding != "tar" || !request.Workspace.Writable {
		t.Fatalf("workspace volume = %+v", request.Workspace)
	}
	sourceHeader, sourceLen, err := wire.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	sourceBody := readExactly(t, written, sourceLen)
	if sourceHeader.Type != wire.StreamTypeWorkspaceArtifact || sourceHeader.RunID != "run-1" {
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

func TestGuestRunnerUsesLiveWorkspaceMountSession(t *testing.T) {
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "main.ts"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	stream := newScriptedCheckpointGuestStream(t, &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskResult{TaskResult: &runv0.TaskResult{ExitCode: 0}},
	})
	sessions := NewWorkspaceMountSessions()
	unregister := sessions.RegisterWorkspaceMountSession(api.WorkerWorkspaceMount{ID: "mat-1"}, fakeGuestSession{stream: stream}, "channel-token")
	defer unregister()
	connector := &fakeGuestConnector{}
	result, err := GuestRunner{
		Connector:       connector,
		WorkspaceMounts: sessions,
		TempDir:         t.TempDir(),
	}.Run(context.Background(), Request{
		Leases: staticLease(api.WorkerRunLease{ID: "execution-1", RunID: "run-1", WorkerInstanceID: "worker-1"}),
		Run: ResolvedRun{
			RunID:     "run-1",
			SessionID: "session-1",
			TaskID:    "deploy",
			Bundle:    runtimeBundle(),
			Payload:   []byte(`{"ok":true}`),
			Workspace: api.WorkerWorkspace{
				ID:                "workspace-1",
				WorkspaceMountID:  "mat-1",
				FencingGeneration: 2,
				WriteLeaseID:      "write-lease-1",
				WriteFencingToken: "write-token-1",
				MountPath:         "/workspace",
			},
		},
		DeploymentSource: builder.Source{ProjectRoot: sourceRoot},
		Workspace:        testWorkspaceArtifact(t, sourceRoot, sourceRoot),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d", result.ExitCode)
	}
	if connector.connectCalls != 0 {
		t.Fatalf("connector Connect calls = %d, want 0", connector.connectCalls)
	}
	written := bytes.NewReader(stream.written.Bytes())
	header, bodyLen, err := wire.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	if header.Type != wire.StreamTypeWorkspaceRun || header.RunID != "run-1" || header.WorkspaceID != "workspace-1" || header.WorkspaceMountID != "mat-1" || bodyLen != 0 {
		t.Fatalf("workspace run header = %+v bodyLen=%d", header, bodyLen)
	}
	var envelope workspacev0.WorkspaceOperationEnvelope
	if err := frameio.ReadProtoFrame(written, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.GetWorkspaceMountId() != "mat-1" || envelope.GetWorkspaceId() != "workspace-1" || envelope.GetChannelToken() != "channel-token" || envelope.GetFencingGeneration() != 2 || envelope.GetWriteLeaseId() != "write-lease-1" || envelope.GetFencingToken() != "write-token-1" {
		t.Fatalf("workspace run envelope = %+v", &envelope)
	}
	sourceHeader, sourceLen, err := wire.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	if sourceHeader.Type != wire.StreamTypeDeploymentSource || sourceHeader.RunID != "run-1" {
		t.Fatalf("deployment source header = %+v", sourceHeader)
	}
	_ = readExactly(t, written, sourceLen)
	var request runv0.RunTaskRequest
	if err := frameio.ReadProtoFrame(written, &request); err != nil {
		t.Fatal(err)
	}
	if request.RunId != "run-1" || request.Workspace == nil || request.Workspace.Path != "/workspace" {
		t.Fatalf("request = %+v", &request)
	}
	if written.Len() != 0 {
		t.Fatalf("unexpected extra materialized run input bytes = %d", written.Len())
	}
}

func TestGuestRunnerRejectsInvalidOutputStreamJSON(t *testing.T) {
	events := &capturingEventSink{}
	err := (GuestRunner{Events: events}).appendOutputStream(context.Background(), api.WorkerRunLease{RunID: "run-1"}, &runv0.OutputStreamAppended{
		Stream:      "diagnostics",
		PayloadJson: "{",
	})
	if err == nil || !strings.Contains(err.Error(), "payload_json") {
		t.Fatalf("error = %v, want payload_json validation", err)
	}
	if len(events.outputs) != 0 {
		t.Fatalf("outputs = %+v, want none", events.outputs)
	}
}

func TestGuestRunnerCreatesToken(t *testing.T) {
	events := &capturingEventSink{}
	timeout := "2026-06-15T12:00:00Z"
	result := (GuestRunner{Events: events}).createToken(context.Background(), api.WorkerRunLease{ID: "session-1", RunID: "run-1"}, &runv0.TokenCreateRequested{
		TimeoutAt:    &timeout,
		Tags:         []string{"approval"},
		MetadataJson: new(`{"bridge":"slack"}`),
	})
	if result.GetErrorMessage() != "" {
		t.Fatalf("result error = %s", result.GetErrorMessage())
	}
	if result.Id != "token-1" || result.GetPublicAccessToken() != "public-token" || result.GetMetadataJson() != `{"bridge":"slack"}` {
		t.Fatalf("result = %+v", result)
	}
	if len(events.tokens) != 1 {
		t.Fatalf("tokens = %+v", events.tokens)
	}
	request := events.tokens[0]
	if request.Lease.RunID != "run-1" || strings.Join(request.Tags, ",") != "approval" || string(request.Metadata) != `{"bridge":"slack"}` {
		t.Fatalf("request = %+v metadata=%s", request, request.Metadata)
	}
	if request.TimeoutAt == nil || !request.TimeoutAt.Equal(time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("timeout_at = %v", request.TimeoutAt)
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
	}.runDirect(context.Background(), Request{
		Leases: staticLease(api.WorkerRunLease{ID: "execution-1", RunID: "run-1", WorkerInstanceID: "worker-1"}),
		Run: ResolvedRun{
			RunID:     "run-1",
			SessionID: "session-1",
			TaskID:    "deploy",
			Bundle:    runtimeBundle(),
			Payload:   []byte(`{}`),
			Secrets:   api.ResolvedSecrets{},
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
		Event: &runv0.RunEvent_RunWaitRequested{RunWaitRequested: &runv0.RunWaitRequested{
			CorrelationId: "approval-1",
			Kind:          "token",
			ParamsJson:    `{}`,
		}},
	}, &runv0.CheckpointPauseReady{
		RunWaitId:    "run-wait-id-1",
		CheckpointId: "checkpoint-1",
	})
	waiter := &capturingRunWaitHandler{}
	store := &fakeCAS{}
	result, err := GuestRunner{
		Connector:           &fakeGuestConnector{stream: stream, checkpointable: true},
		CAS:                 store,
		CheckpointEncryptor: testCheckpointEncryptor(t),
		TempDir:             t.TempDir(),
	}.runDirect(context.Background(), Request{
		Leases: staticLease(api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"}),
		Run: ResolvedRun{
			RunID:      "run-1",
			SessionID:  "session-1",
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

func TestRuntimeWaitRequestRejectsInvalidMetadataJSON(t *testing.T) {
	_, err := runtimeWaitRequest(Request{}, &runv0.RunWaitRequested{
		CorrelationId: "approval-1",
		Kind:          "token",
		ParamsJson:    `{}`,
		MetadataJson:  new(`[]`),
	})
	if err == nil || !strings.Contains(err.Error(), "metadata_json must be a JSON object") {
		t.Fatalf("err = %v", err)
	}
}

func TestRuntimeWaitRequestPreservesIdleTimeout(t *testing.T) {
	timeout := uint32(60)
	idleTimeout := uint32(10)
	request, err := runtimeWaitRequest(Request{
		Leases: staticLease(api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"}),
	}, &runv0.RunWaitRequested{
		CorrelationId: "approval-1",
		Kind:          "token",
		ParamsJson:    `{}`,
		Timeout:       &timeout,
		IdleTimeout:   &idleTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.TimeoutSeconds == nil || *request.TimeoutSeconds != 60 {
		t.Fatalf("timeout = %v, want 60", request.TimeoutSeconds)
	}
	if request.IdleTimeoutSeconds == nil || *request.IdleTimeoutSeconds != 10 {
		t.Fatalf("idle timeout = %v, want 10", request.IdleTimeoutSeconds)
	}
}

func TestRuntimeWaitRequestRejectsOversizedMetadataJSON(t *testing.T) {
	_, err := runtimeWaitRequest(Request{}, &runv0.RunWaitRequested{
		CorrelationId: "approval-1",
		Kind:          "token",
		ParamsJson:    `{}`,
		MetadataJson:  new(`{"value":"` + strings.Repeat("x", waitMetadataJSONMaxBytes) + `"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "metadata_json is") || !strings.Contains(err.Error(), "exceeds max") {
		t.Fatalf("err = %v", err)
	}
}

func TestRuntimeWaitRequestRejectsTooManyTags(t *testing.T) {
	tags := make([]string, waitTagsMaxCount+1)
	for i := range tags {
		tags[i] = "approval"
	}
	_, err := runtimeWaitRequest(Request{}, &runv0.RunWaitRequested{
		CorrelationId: "approval-1",
		Kind:          "token",
		ParamsJson:    `{}`,
		Tags:          tags,
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds max") {
		t.Fatalf("err = %v", err)
	}
}

func TestRuntimeWaitRequestRejectsOversizedTag(t *testing.T) {
	_, err := runtimeWaitRequest(Request{}, &runv0.RunWaitRequested{
		CorrelationId: "approval-1",
		Kind:          "token",
		ParamsJson:    `{}`,
		Tags:          []string{strings.Repeat("x", waitTagMaxBytes+1)},
	})
	if err == nil || !strings.Contains(err.Error(), "exceeds max") {
		t.Fatalf("err = %v", err)
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
		Event: &runv0.RunEvent_RunWaitRequested{RunWaitRequested: &runv0.RunWaitRequested{
			CorrelationId: "approval-1",
			Kind:          "token",
			ParamsJson:    `{}`,
		}},
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_StdoutChunk{StdoutChunk: []byte("x")},
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_LogEntry{LogEntry: "checkpoint flush"},
	}, &runv0.CheckpointPauseReady{
		RunWaitId:    "run-wait-id-1",
		CheckpointId: "checkpoint-1",
	})
	waiter := &capturingRunWaitHandler{}
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
	}.runDirect(context.Background(), Request{
		Leases: staticLease(api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"}),
		Run: ResolvedRun{
			RunID:     "run-1",
			SessionID: "session-1",
			TaskID:    "deploy",
			Bundle:    runtimeBundle(),
			Payload:   []byte(`{}`),
			Secrets:   api.ResolvedSecrets{},
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

func TestGuestRunnerRestoresCheckpointAndAttachesRunWait(t *testing.T) {
	state := []byte("state")
	scratch := []byte("scratch")
	substrate := []byte("substrate")
	memory := []byte("memory")
	manifest := []byte(`{"checkpoint_id":"checkpoint-1","runtime":{"backend":"firecracker"}}`)
	encryptor := testCheckpointEncryptor(t)
	manifestObject := encryptedCheckpointObject(t, encryptor, manifest, "manifest")
	stateObject := encryptedCheckpointObject(t, encryptor, state, "vmstate")
	scratchObject := encryptedCheckpointObject(t, encryptor, scratch, "scratch-disk")
	substrateDigest := sha256sum.DigestBytes(substrate)
	substrateObject := encryptedRuntimeSubstrateObject(t, encryptor, substrate, substrateDigest)
	memoryObject := encryptedCheckpointObject(t, encryptor, memory, "memory")
	stream := newScriptedCheckpointGuestStream(t, &runv0.ResumeAck{
		RunWaitId: "run-wait-id-1",
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskResult{TaskResult: &runv0.TaskResult{ExitCode: 0}},
	})
	connector := &fakeGuestConnector{
		stream: stream,
		restorePhases: []vm.RuntimePhase{{
			Name:       "restore_unpack_scratch_filepack",
			DurationMs: 12,
			Role:       "scratch-disk",
			MediaType:  cas.CheckpointScratchDiskMediaType,
			Filepack: &vm.FilepackStats{
				LogicalBytes:       1024,
				SparseSupported:    new(true),
				EncodedChunks:      1,
				UnpackWrittenBytes: 512,
			},
		}, {
			Name:       "restore_unpack_memory_filepack",
			DurationMs: 34,
			Role:       "memory",
			MediaType:  cas.CheckpointMemoryMediaType,
		}},
		concurrentRestorePhases: true,
	}
	waiter := &capturingRunWaitHandler{}
	checkpoint := testRestoreCheckpointManifest(manifest, manifestObject, stateObject, scratchObject, memoryObject, testCheckpointWorkspaceBase())
	checkpoint.RecoveryPoint.Runtime.Substrate = &api.WorkerCheckpointRuntimeSubstrate{
		Digest:     substrateDigest,
		Format:     "ext4",
		BuilderABI: "helmr.runtime-substrate.builder.v0",
		LayoutABI:  "helmr.runtime-substrate.layout.v0",
	}
	checkpoint.RuntimeState.RuntimeSubstrate = &api.WorkerRuntimeSubstrate{
		ID:                  "019f1790-0000-7000-8000-000000000003",
		DeploymentSandboxID: "019f1790-0000-7000-8000-000000000004",
		Artifact: api.CASObject{
			Digest:    substrateObject.digest,
			SizeBytes: int64(len(substrateObject.body)),
			MediaType: cas.RuntimeSubstrateMediaType,
		},
		SubstrateDigest: substrateDigest,
		Format:          "ext4",
		BuilderABI:      "helmr.runtime-substrate.builder.v0",
		LayoutABI:       "helmr.runtime-substrate.layout.v0",
		SizeBytes:       int64(len(substrate)),
	}
	var logBuffer bytes.Buffer
	result, err := GuestRunner{
		Connector:           connector,
		CAS:                 &fakeCAS{objects: map[string][]byte{manifestObject.digest: manifestObject.body, stateObject.digest: stateObject.body, scratchObject.digest: scratchObject.body, substrateObject.digest: substrateObject.body, memoryObject.digest: memoryObject.body}},
		CheckpointEncryptor: encryptor,
		TempDir:             t.TempDir(),
		Log:                 slog.New(slog.NewJSONHandler(&logBuffer, nil)),
	}.Run(context.Background(), Request{
		Leases:      staticLease(api.WorkerRunLease{ID: "execution-1", RunID: "run-1", WorkerInstanceID: "worker-1"}),
		WaitHandler: waiter,
		Run: ResolvedRun{
			RunID: "run-1",
			Restore: &api.WorkerRestore{
				CheckpointID: "checkpoint-1",
				Checkpoint:   checkpoint,
				RunWait: api.WorkerRestoreRunWait{
					ID:                "run-wait-id-1",
					ResumeKind:        "completed",
					ResumePayloadJSON: json.RawMessage(`{"approved":true}`),
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
	if connector.restoreRequest.Topology.Substrate == nil {
		t.Fatal("restore request missing runtime substrate")
	}
	if connector.restoreRequest.Topology.Substrate.Digest != sha256sum.DigestBytes(substrate) ||
		connector.restoreRequest.Topology.Substrate.Format != "ext4" ||
		connector.restoreRequest.Topology.Substrate.Path == "" {
		t.Fatalf("restore substrate = %+v", connector.restoreRequest.Topology.Substrate)
	}
	if connector.restoreRequest.RecordPhase == nil {
		t.Fatal("restore request missing phase recorder")
	}
	logBody := logBuffer.String()
	for _, want := range []string{
		"checkpoint restore telemetry",
		"restore_materialize_manifest",
		"restore_materialize_substrate_cas",
		"restore_materialize_scratch_filepack",
		"restore_unpack_scratch_filepack",
		"restore_attach_guest_resume",
	} {
		if !strings.Contains(logBody, want) {
			t.Fatalf("restore telemetry log missing %q: %s", want, logBody)
		}
	}
	written := bytes.NewReader(stream.written.Bytes())
	var attach runv0.ResumeAttach
	if err := frameio.ReadProtoFrame(written, &attach); err != nil {
		t.Fatal(err)
	}
	if attach.CheckpointId != "checkpoint-1" || attach.RunWaitId != "run-wait-id-1" || attach.RunLeaseId != "execution-1" {
		t.Fatalf("attach = %+v", &attach)
	}
	var decision runv0.ResumeDecision
	if err := frameio.ReadProtoFrame(written, &decision); err != nil {
		t.Fatal(err)
	}
	if decision.RunWaitId != "run-wait-id-1" || decision.Kind != "completed" || decision.DataJson != `{"approved":true}` {
		t.Fatalf("decision = %+v", &decision)
	}
	if waiter.acknowledged.RunWaitID != "run-wait-id-1" || waiter.acknowledged.CheckpointID != "checkpoint-1" {
		t.Fatalf("acknowledged = %+v", waiter.acknowledged)
	}
	if !checkpointPhaseHasFilepackStats(waiter.acknowledged.Phases, "restore_unpack_scratch_filepack") ||
		!checkpointPhaseNamed(waiter.acknowledged.Phases, "restore_attach_guest_resume") {
		t.Fatalf("acknowledged phases = %+v", waiter.acknowledged.Phases)
	}
}

func TestGuestRunnerRestoresCheckpointSubstrateFromLocalCache(t *testing.T) {
	state := []byte("state")
	scratch := []byte("scratch")
	substrateBytes := []byte("substrate")
	memory := []byte("memory")
	manifest := []byte(`{"checkpoint_id":"checkpoint-1","runtime":{"backend":"firecracker"}}`)
	encryptor := testCheckpointEncryptor(t)
	manifestObject := encryptedCheckpointObject(t, encryptor, manifest, "manifest")
	stateObject := encryptedCheckpointObject(t, encryptor, state, "vmstate")
	scratchObject := encryptedCheckpointObject(t, encryptor, scratch, "scratch-disk")
	substrateDigest := sha256sum.DigestBytes(substrateBytes)
	substrateObject := encryptedRuntimeSubstrateObject(t, encryptor, substrateBytes, substrateDigest)
	memoryObject := encryptedCheckpointObject(t, encryptor, memory, "memory")
	cacheDir := t.TempDir()
	cachedSubstratePath := filepath.Join(cacheDir, "cached-substrate.ext4")
	if err := os.WriteFile(cachedSubstratePath, substrateBytes, 0o444); err != nil {
		t.Fatal(err)
	}
	stream := newScriptedCheckpointGuestStream(t, &runv0.ResumeAck{
		RunWaitId: "run-wait-id-1",
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_TaskResult{TaskResult: &runv0.TaskResult{ExitCode: 0}},
	})
	connector := &fakeGuestConnector{stream: stream}
	waiter := &capturingRunWaitHandler{}
	checkpoint := testRestoreCheckpointManifest(manifest, manifestObject, stateObject, scratchObject, memoryObject, testCheckpointWorkspaceBase())
	checkpoint.RecoveryPoint.Runtime.Substrate = &api.WorkerCheckpointRuntimeSubstrate{
		Digest:     substrateDigest,
		Format:     "ext4",
		BuilderABI: "helmr.runtime-substrate.builder.v0",
		LayoutABI:  "helmr.runtime-substrate.layout.v0",
	}
	checkpoint.RuntimeState.RuntimeSubstrate = &api.WorkerRuntimeSubstrate{
		ID:                  "019f1790-0000-7000-8000-000000000003",
		DeploymentSandboxID: "019f1790-0000-7000-8000-000000000004",
		Artifact: api.CASObject{
			Digest:    substrateObject.digest,
			SizeBytes: int64(len(substrateObject.body)),
			MediaType: cas.RuntimeSubstrateMediaType,
		},
		SubstrateDigest: substrateDigest,
		Format:          "ext4",
		BuilderABI:      "helmr.runtime-substrate.builder.v0",
		LayoutABI:       "helmr.runtime-substrate.layout.v0",
		SizeBytes:       int64(len(substrateBytes)),
	}
	store := &fakeCAS{objects: map[string][]byte{
		manifestObject.digest:  manifestObject.body,
		stateObject.digest:     stateObject.body,
		scratchObject.digest:   scratchObject.body,
		substrateObject.digest: substrateObject.body,
		memoryObject.digest:    memoryObject.body,
	}}
	lookup := &fakeRuntimeSubstrateLookup{
		digest: substrateDigest,
		path:   cachedSubstratePath,
	}
	result, err := GuestRunner{
		Connector:           connector,
		CAS:                 store,
		CheckpointEncryptor: encryptor,
		TempDir:             t.TempDir(),
		Substrates:          lookup,
	}.Run(context.Background(), Request{
		Leases:      staticLease(api.WorkerRunLease{ID: "execution-1", RunID: "run-1", WorkerInstanceID: "worker-1"}),
		WaitHandler: waiter,
		Run: ResolvedRun{
			RunID: "run-1",
			Restore: &api.WorkerRestore{
				CheckpointID: "checkpoint-1",
				Checkpoint:   checkpoint,
				RunWait: api.WorkerRestoreRunWait{
					ID:                "run-wait-id-1",
					ResumeKind:        "completed",
					ResumePayloadJSON: json.RawMessage(`{"approved":true}`),
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || connector.restoreRequest.Topology.Substrate == nil {
		t.Fatalf("result=%+v restore=%+v", result, connector.restoreRequest)
	}
	if lookup.calls != 1 {
		t.Fatalf("lookup calls = %d, want 1", lookup.calls)
	}
	if store.getCalls[substrateObject.digest] != 0 {
		t.Fatalf("substrate CAS get calls = %d, want 0", store.getCalls[substrateObject.digest])
	}
	if connector.restoreRequest.Topology.Substrate.Path == cachedSubstratePath {
		t.Fatal("restore substrate path should be staged into checkpoint jail path, not passed directly from cache")
	}
	if connector.restoreRequest.Topology.Substrate.Digest != substrateDigest {
		t.Fatalf("restore substrate = %+v", connector.restoreRequest.Topology.Substrate)
	}
	if waiter.acknowledged.RunWaitID != "run-wait-id-1" || waiter.acknowledged.CheckpointID != "checkpoint-1" {
		t.Fatalf("acknowledged = %+v", waiter.acknowledged)
	}
	if !checkpointPhaseNamed(waiter.acknowledged.Phases, "restore_lookup_substrate_cache_hit") ||
		!checkpointPhaseNamed(waiter.acknowledged.Phases, "restore_materialize_substrate_cache") ||
		checkpointPhaseNamed(waiter.acknowledged.Phases, "restore_materialize_substrate_cas") {
		t.Fatalf("acknowledged phases = %+v", waiter.acknowledged.Phases)
	}
}

func TestGuestRunnerRejectsCheckpointSubstrateArtifactDigestMismatch(t *testing.T) {
	state := []byte("state")
	scratch := []byte("scratch")
	substrate := []byte("substrate")
	memory := []byte("memory")
	manifest := []byte(`{"checkpoint_id":"checkpoint-1","runtime":{"backend":"firecracker"}}`)
	encryptor := testCheckpointEncryptor(t)
	manifestObject := encryptedCheckpointObject(t, encryptor, manifest, "manifest")
	stateObject := encryptedCheckpointObject(t, encryptor, state, "vmstate")
	scratchObject := encryptedCheckpointObject(t, encryptor, scratch, "scratch-disk")
	expectedSubstrateDigest := sha256sum.DigestBytes([]byte("different substrate"))
	substrateObject := encryptedRuntimeSubstrateObject(t, encryptor, substrate, expectedSubstrateDigest)
	memoryObject := encryptedCheckpointObject(t, encryptor, memory, "memory")
	checkpoint := testRestoreCheckpointManifest(manifest, manifestObject, stateObject, scratchObject, memoryObject, testCheckpointWorkspaceBase())
	checkpoint.RecoveryPoint.Runtime.Substrate = &api.WorkerCheckpointRuntimeSubstrate{
		Digest:     expectedSubstrateDigest,
		Format:     "ext4",
		BuilderABI: "helmr.runtime-substrate.builder.v0",
		LayoutABI:  "helmr.runtime-substrate.layout.v0",
	}
	checkpoint.RuntimeState.RuntimeSubstrate = &api.WorkerRuntimeSubstrate{
		ID:                  "019f1790-0000-7000-8000-000000000005",
		DeploymentSandboxID: "019f1790-0000-7000-8000-000000000006",
		Artifact: api.CASObject{
			Digest:    substrateObject.digest,
			SizeBytes: int64(len(substrateObject.body)),
			MediaType: cas.RuntimeSubstrateMediaType,
		},
		SubstrateDigest: expectedSubstrateDigest,
		Format:          "ext4",
		BuilderABI:      "helmr.runtime-substrate.builder.v0",
		LayoutABI:       "helmr.runtime-substrate.layout.v0",
		SizeBytes:       int64(len(substrate)),
	}
	connector := &fakeGuestConnector{stream: newScriptedCheckpointGuestStream(t)}
	_, err := GuestRunner{
		Connector:           connector,
		CAS:                 &fakeCAS{objects: map[string][]byte{manifestObject.digest: manifestObject.body, stateObject.digest: stateObject.body, scratchObject.digest: scratchObject.body, substrateObject.digest: substrateObject.body, memoryObject.digest: memoryObject.body}},
		CheckpointEncryptor: encryptor,
		TempDir:             t.TempDir(),
	}.Run(context.Background(), Request{
		Leases:      staticLease(api.WorkerRunLease{ID: "execution-1", RunID: "run-1", WorkerInstanceID: "worker-1"}),
		WaitHandler: &capturingRunWaitHandler{},
		Run: ResolvedRun{
			RunID: "run-1",
			Restore: &api.WorkerRestore{
				CheckpointID: "checkpoint-1",
				Checkpoint:   checkpoint,
				RunWait: api.WorkerRestoreRunWait{
					ID:                "run-wait-id-1",
					ResumeKind:        "completed",
					ResumePayloadJSON: json.RawMessage(`{"approved":true}`),
				},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "checkpoint runtime substrate digest mismatch") {
		t.Fatalf("err = %v, want substrate digest mismatch", err)
	}
	if connector.restoreRequest.ID != "" {
		t.Fatalf("restore connector was called: %+v", connector.restoreRequest)
	}
}

func TestGuestRunnerRequiresRestoreAcknowledgerBeforeResumeAttach(t *testing.T) {
	stream := newScriptedCheckpointGuestStream(t, &runv0.ResumeAck{RunWaitId: "run-wait-id-1"})
	session := fakeGuestSession{stream: stream}
	err := GuestRunner{}.attachAndAcknowledgeRestore(context.Background(), session, Request{
		Leases:      staticLease(api.WorkerRunLease{ID: "execution-1", RunID: "run-1", WorkerInstanceID: "worker-1"}),
		WaitHandler: waitOnlyHandler{},
		Run: ResolvedRun{
			Restore: &api.WorkerRestore{
				CheckpointID: "checkpoint-1",
				RunWait: api.WorkerRestoreRunWait{
					ID:                "run-wait-id-1",
					ResumeKind:        "completed",
					ResumePayloadJSON: json.RawMessage(`{"approved":true}`),
				},
			},
		},
	}, &runtimePhaseCollector{})
	if err == nil || !strings.Contains(err.Error(), "restore acknowledger is required") {
		t.Fatalf("err = %v, want restore acknowledger", err)
	}
	if stream.written.Len() != 0 {
		t.Fatalf("resume attach was written before acknowledger validation: %x", stream.written.Bytes())
	}
}

func checkpointPhaseNamed(phases []api.WorkerCheckpointPhase, name string) bool {
	for _, phase := range phases {
		if phase.Name == name {
			return true
		}
	}
	return false
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
		RunWaitId: "run-wait-id-1",
	}, &runv0.RunEvent{
		Event: &runv0.RunEvent_RunWaitRequested{RunWaitRequested: &runv0.RunWaitRequested{
			CorrelationId: "next-run wait",
			Kind:          "token",
			ParamsJson:    `{}`,
		}},
	}, &runv0.CheckpointPauseReady{
		RunWaitId:    "run-wait-id-1",
		CheckpointId: "checkpoint-1",
	})
	connector := &fakeGuestConnector{stream: stream, checkpointable: true}
	waiter := &capturingRunWaitHandler{}
	result, err := GuestRunner{
		Connector:           connector,
		CAS:                 &fakeCAS{objects: map[string][]byte{manifestObject.digest: manifestObject.body, stateObject.digest: stateObject.body, scratchObject.digest: scratchObject.body, memoryObject.digest: memoryObject.body}},
		CheckpointEncryptor: encryptor,
		TempDir:             t.TempDir(),
	}.Run(context.Background(), Request{
		Leases:      staticLease(api.WorkerRunLease{ID: "execution-1", RunID: "run-1", WorkerInstanceID: "worker-1"}),
		WaitHandler: waiter,
		Run: ResolvedRun{
			RunID: "run-1",
			Restore: &api.WorkerRestore{
				CheckpointID: "checkpoint-1",
				Checkpoint:   testRestoreCheckpointManifest(manifest, manifestObject, stateObject, scratchObject, memoryObject, workspaceBase),
				RunWait: api.WorkerRestoreRunWait{
					ID:                "run-wait-id-1",
					ResumeKind:        "completed",
					ResumePayloadJSON: json.RawMessage(`{"approved":true}`),
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
	}.runDirect(context.Background(), Request{
		Leases: staticLease(api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"}),
		Run: ResolvedRun{
			RunID:       "run-1",
			SessionID:   "session-1",
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

func TestGuestRunnerEnforcesMaxDurationDuringActiveStreamRead(t *testing.T) {
	imagePath := filepath.Join(t.TempDir(), "image.oci.tar")
	if err := os.WriteFile(imagePath, []byte("oci"), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "main.ts"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}
	stream := newScriptedGuestStream(t, &runv0.RunEvent{
		Event: &runv0.RunEvent_ActiveStreamReadRequested{ActiveStreamReadRequested: &runv0.ActiveStreamReadRequested{
			CorrelationId: "read-1",
			Stream:        "approval",
			Block:         true,
		}},
	})
	events := &blockingInputEventSink{capturingEventSink: &capturingEventSink{}}
	_, err := GuestRunner{
		Connector: &fakeGuestConnector{stream: stream},
		Events:    events,
		TempDir:   t.TempDir(),
	}.runDirect(context.Background(), Request{
		Leases: staticLease(api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"}),
		Run: ResolvedRun{
			RunID:       "run-1",
			SessionID:   "session-1",
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
		t.Fatalf("err = %v, want max_duration", err)
	}
	if len(events.reads) != 1 || events.reads[0].Stream != "approval" || !events.reads[0].Block {
		t.Fatalf("active reads = %+v", events.reads)
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
	}.runDirect(context.Background(), Request{
		Leases: staticLease(api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"}),
		Run: ResolvedRun{
			RunID:     "run-1",
			SessionID: "session-1",
			TaskID:    "deploy",
			Bundle:    runtimeBundle(),
			Payload:   []byte(`{}`),
			Secrets:   api.ResolvedSecrets{},
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
		}.runDirect(ctx, Request{
			Leases: staticLease(api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"}),
			Run: ResolvedRun{
				RunID:       "run-1",
				SessionID:   "session-1",
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
	}.runDirect(context.Background(), Request{
		Leases: staticLease(api.WorkerRunLease{RunID: "run-1", WorkerInstanceID: "worker-1"}),
		Run: ResolvedRun{
			RunID:     "run-1",
			SessionID: "session-1",
			TaskID:    "deploy",
			Bundle:    runtimeBundle(),
			Payload:   []byte(`{}`),
			Secrets:   api.ResolvedSecrets{},
		},
		Artifact:         builder.Artifact{ImageTarPath: imagePath},
		DeploymentSource: builder.Source{CheckoutRoot: repoRoot, ProjectRoot: appRoot},
		Workspace:        testWorkspaceArtifact(t, repoRoot, appRoot),
	})
	if err != nil {
		t.Fatal(err)
	}
	written := bytes.NewReader(stream.written.Bytes())
	imageHeader, imageLen, err := wire.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	if imageHeader.Type != wire.StreamTypeRunImage {
		t.Fatalf("image header = %+v", imageHeader)
	}
	_ = readExactly(t, written, imageLen)
	sourceHeader, sourceLen, err := wire.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	_ = readExactly(t, written, sourceLen)
	if sourceHeader.Type != wire.StreamTypeDeploymentSource {
		t.Fatalf("deployment source header = %+v", sourceHeader)
	}
	var request runv0.RunTaskRequest
	if err := frameio.ReadProtoFrame(written, &request); err != nil {
		t.Fatal(err)
	}
	sourceHeader, sourceLen, err = wire.ReadStreamFrameHeader(written)
	if err != nil {
		t.Fatal(err)
	}
	sourceBody := readExactly(t, written, sourceLen)
	if sourceHeader.Type != wire.StreamTypeWorkspaceArtifact {
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
			RunID:     "run-1",
			SessionID: "session-1",
			TaskID:    "deploy",
			Bundle:    runtimeBundleWithSecret(),
			Payload:   []byte(`{}`),
			Secrets:   api.ResolvedSecrets{},
		},
		Artifact: builder.Artifact{ImageTarPath: imagePath},
		Workspace: workspace.WorkspaceArtifact{
			Digest:    "sha256:" + string(bytes.Repeat([]byte{'0'}, 64)),
			MediaType: workspace.ArtifactMediaType,
			Encoding:  workspace.ArtifactEncoding,
		},
	})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("required")) {
		t.Fatalf("err = %v", err)
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
	stream                  io.ReadWriteCloser
	checkpointable          bool
	network                 compute.NetworkPolicy
	restoreRequest          vm.RestoreRequest
	restorePhases           []vm.RuntimePhase
	concurrentRestorePhases bool
	connectCalls            int
}

func (c *fakeGuestConnector) Connect(_ context.Context, request vm.ConnectRequest) (vm.Session, error) {
	c.connectCalls++
	c.network = request.Network
	if c.checkpointable {
		return fakeCheckpointableGuestSession{fakeGuestSession{stream: c.stream}}, nil
	}
	return fakeGuestSession{stream: c.stream}, nil
}

func (c *fakeGuestConnector) Restore(_ context.Context, request vm.RestoreRequest) (vm.Session, error) {
	c.restoreRequest = request
	if request.RecordPhase != nil && len(c.restorePhases) > 0 {
		if c.concurrentRestorePhases {
			var group sync.WaitGroup
			for _, phase := range c.restorePhases {
				group.Go(func() {
					request.RecordPhase(phase)
				})
			}
			group.Wait()
		} else {
			for _, phase := range c.restorePhases {
				request.RecordPhase(phase)
			}
		}
	}
	if c.checkpointable {
		return fakeCheckpointableGuestSession{fakeGuestSession{stream: c.stream}}, nil
	}
	return fakeGuestSession{stream: c.stream}, nil
}

type fakeGuestSession struct {
	stream io.ReadWriteCloser
}

type fakeRuntimeSubstrateLookup struct {
	digest string
	path   string
	calls  int
	err    error
}

func (f *fakeRuntimeSubstrateLookup) Resolve(context.Context, string, substrate.Source) (substrate.Result, error) {
	return substrate.Result{}, errors.New("unexpected substrate source resolve")
}

func (f *fakeRuntimeSubstrateLookup) LookupDigest(_ context.Context, digest string) (substrate.Result, error) {
	f.calls++
	if f.err != nil {
		return substrate.Result{}, f.err
	}
	if digest != f.digest {
		return substrate.Result{}, os.ErrNotExist
	}
	info, err := os.Stat(f.path)
	if err != nil {
		return substrate.Result{}, err
	}
	return substrate.Result{
		Path:       f.path,
		Digest:     digest,
		Format:     substrate.Format,
		BuilderABI: substrate.BuilderABI,
		LayoutABI:  substrate.LayoutABI,
		SizeBytes:  info.Size(),
	}, nil
}

func (s fakeGuestSession) Stream() io.ReadWriteCloser {
	return s.stream
}

func (s fakeGuestSession) OpenStream(context.Context) (io.ReadWriteCloser, error) {
	return s.stream, nil
}

func (s fakeGuestSession) Close(context.Context) error {
	return s.stream.Close()
}

func (s fakeGuestSession) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
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

type capturingRunWaitHandler struct {
	request      WaitRequest
	added        []WaitRequest
	manifest     api.WorkerCheckpointManifest
	acknowledged RestoreAcknowledgement
}

func (h *capturingRunWaitHandler) Wait(_ context.Context, request WaitRequest) error {
	h.request = request
	if request.Checkpointer != nil {
		checkpoint, err := request.Checkpointer.CreateCheckpoint(context.Background(), CheckpointRequest{
			RunID:        request.Lease.RunID,
			RunWaitID:    "run-wait-id-1",
			CheckpointID: "checkpoint-1",
		})
		if err != nil {
			return err
		}
		h.manifest = checkpoint.Manifest
	}
	return ErrDetached
}

func (h *capturingRunWaitHandler) AddRunWait(_ context.Context, request WaitRequest) (api.WorkerCreateRunWaitResponse, error) {
	h.added = append(h.added, request)
	return api.WorkerCreateRunWaitResponse{
		RunID:     request.Lease.RunID,
		RunWaitID: fmt.Sprintf("run-wait-id-%d", len(h.added)+1),
	}, nil
}

func (h *capturingRunWaitHandler) AcknowledgeRestore(_ context.Context, request RestoreAcknowledgement) error {
	h.acknowledged = request
	return nil
}

type capturedLogEvent struct {
	claim       api.WorkerRunLease
	stream      api.WorkerLogStream
	observedSeq uint64
	content     []byte
}

type capturedOutputEvent struct {
	request api.WorkerOutputStreamAppendRequest
}

type capturedMetadataEvent struct {
	request api.WorkerUpdateRunMetadataRequest
}

type capturingEventSink struct {
	logs     []capturedLogEvent
	entries  []string
	outputs  []capturedOutputEvent
	reads    []api.WorkerActiveStreamReadRequest
	metadata []capturedMetadataEvent
	tokens   []api.WorkerCreateTokenRequest
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

func (s *capturingEventSink) AppendOutputStream(_ context.Context, request api.WorkerOutputStreamAppendRequest) (api.AppendStreamRecordResponse, error) {
	request.Data = append(json.RawMessage(nil), request.Data...)
	s.outputs = append(s.outputs, capturedOutputEvent{request: request})
	contentType := request.ContentType
	if contentType == "" {
		contentType = "application/json"
	}
	return api.AppendStreamRecordResponse{
		Record: api.StreamRecordResponse{
			ID:          "output-record-1",
			StreamID:    "stream-1",
			Sequence:    1,
			Data:        append(json.RawMessage(nil), request.Data...),
			ContentType: contentType,
			CreatedAt:   time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		},
	}, nil
}

func (s *capturingEventSink) ReadInputStream(_ context.Context, request api.WorkerActiveStreamReadRequest) (api.WorkerActiveStreamReadResponse, error) {
	s.reads = append(s.reads, request)
	record := api.StreamRecordResponse{
		ID:          "stream-record-1",
		StreamID:    "stream-1",
		Sequence:    request.AfterSequence + 1,
		Data:        json.RawMessage(`{"ok":true}`),
		ContentType: "application/json",
		CreatedAt:   time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
	}
	return api.WorkerActiveStreamReadResponse{Record: &record}, nil
}

type blockingInputEventSink struct {
	*capturingEventSink
}

func (s *blockingInputEventSink) ReadInputStream(ctx context.Context, request api.WorkerActiveStreamReadRequest) (api.WorkerActiveStreamReadResponse, error) {
	s.reads = append(s.reads, request)
	<-ctx.Done()
	return api.WorkerActiveStreamReadResponse{}, ctx.Err()
}

func (s *capturingEventSink) UpdateRunMetadata(_ context.Context, request api.WorkerUpdateRunMetadataRequest) (api.WorkerEventResponse, error) {
	request.Value = append(json.RawMessage(nil), request.Value...)
	request.Patch = append(json.RawMessage(nil), request.Patch...)
	s.metadata = append(s.metadata, capturedMetadataEvent{request: request})
	return api.WorkerEventResponse{RunID: request.Lease.RunID}, nil
}

func (s *capturingEventSink) CreateRuntimeToken(_ context.Context, request api.WorkerCreateTokenRequest) (api.TokenResponse, error) {
	request.Metadata = append(json.RawMessage(nil), request.Metadata...)
	s.tokens = append(s.tokens, request)
	timeout := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	return api.TokenResponse{
		ID:                "token-1",
		Status:            "waiting",
		CallbackURL:       "https://api.example.test/api/v1/tokens/token-1/callback/secret",
		PublicAccessToken: "public-token",
		TimeoutAt:         &timeout,
		Tags:              []string{"approval"},
		Metadata:          json.RawMessage(`{"bridge":"slack"}`),
	}, nil
}

type scriptedGuestStream struct {
	read    *bytes.Reader
	written bytes.Buffer
}

func newScriptedGuestStream(t *testing.T, messages ...proto.Message) *scriptedGuestStream {
	t.Helper()
	var read bytes.Buffer
	for _, message := range messages {
		if ready, ok := message.(*runv0.CheckpointPauseReady); ok {
			if err := wire.WriteStreamFrameHeader(&read, wire.StreamHeader{
				Type:         wire.StreamTypeCheckpointPauseReady,
				RunWaitID:    ready.RunWaitId,
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
		if err := frameio.WriteMessageFrame(&read, body); err != nil {
			t.Fatal(err)
		}
	}
	return &scriptedGuestStream{read: bytes.NewReader(read.Bytes())}
}

func newScriptedCheckpointGuestStream(t *testing.T, messages ...proto.Message) *scriptedGuestStream {
	t.Helper()
	var read bytes.Buffer
	for _, message := range messages {
		if ready, ok := message.(*runv0.CheckpointPauseReady); ok {
			if err := wire.WriteStreamFrameHeader(&read, wire.StreamHeader{
				Type:         wire.StreamTypeCheckpointPauseReady,
				RunWaitID:    ready.RunWaitId,
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
		if err := frameio.WriteMessageFrame(&read, body); err != nil {
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
	metadata  map[string]cas.Object
	getCalls  map[string]int
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
	object := cas.Object{Digest: sha256sum.DigestBytes(content), SizeBytes: int64(len(content)), MediaType: mediaType}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mediaType = mediaType
	f.content = append([]byte(nil), content...)
	if f.objects != nil {
		f.objects[object.Digest] = append([]byte(nil), content...)
	}
	if f.metadata == nil {
		f.metadata = map[string]cas.Object{}
	}
	f.metadata[object.Digest] = object
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

func (f *fakeCAS) Stat(_ context.Context, digest string) (cas.Object, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.metadata[digest]
	if !ok {
		return cas.Object{}, os.ErrNotExist
	}
	return object, nil
}

func (f *fakeCAS) Get(_ context.Context, digest string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getCalls == nil {
		f.getCalls = map[string]int{}
	}
	f.getCalls[digest]++
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
	return encryptedCheckpoint{digest: sha256sum.DigestBytes(encrypted), body: encrypted}
}

func encryptedRuntimeSubstrateObject(t *testing.T, encryptor *checkpoint.Encryptor, plaintext []byte, rawDigest string) encryptedCheckpoint {
	t.Helper()
	var body bytes.Buffer
	if err := encryptor.Encrypt(context.Background(), bytes.NewReader(plaintext), &body, runtimeSubstratePurpose(rawDigest)); err != nil {
		t.Fatal(err)
	}
	encrypted := body.Bytes()
	return encryptedCheckpoint{digest: sha256sum.DigestBytes(encrypted), body: encrypted}
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

func testWorkspaceArtifact(t *testing.T, checkoutRoot, projectRoot string) workspace.WorkspaceArtifact {
	t.Helper()
	rel, err := filepath.Rel(checkoutRoot, projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		t.Fatalf("workspace project root %q is not inside checkout root %q", projectRoot, checkoutRoot)
	}
	tarArchive, cleanup, err := archive.CreateTarWithOptions(projectRoot, t.TempDir(), archive.TarOptions{
		ExcludePatterns: []string{"**/.git/**"},
		MaxBytes:        workspace.MaxArtifactExtractedBytes,
		MaxEntries:      workspace.MaxArtifactEntries,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	return workspace.WorkspaceArtifact{
		Path:       tarArchive.Path,
		Digest:     tarArchive.Digest,
		MediaType:  workspace.ArtifactMediaType,
		Encoding:   workspace.ArtifactEncoding,
		SizeBytes:  tarArchive.SizeBytes,
		EntryCount: tarArchive.EntryCount,
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
