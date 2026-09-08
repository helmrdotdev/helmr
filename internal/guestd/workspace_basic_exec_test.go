package guestd

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
)

func TestWorkspaceBasicExecReplaysOneExecution(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32
	entry := &workspaceMountEntry{
		basicExecRun: func(request *workspacev0.WorkspaceBasicExecRequest) *workspacev0.WorkspaceBasicExecResult {
			if runs.Add(1) == 1 {
				close(started)
			}
			<-release
			return &workspacev0.WorkspaceBasicExecResult{
				Outcome: "exited", RequestFingerprint: request.GetEnvelope().GetRequestFingerprint(),
			}
		},
	}
	registry := testWorkspaceBasicExecRegistry(entry)
	request := testWorkspaceBasicExecRequest("process-1", strings.Repeat("a", 64))
	first := make(chan *workspacev0.WorkspaceBasicExecResult, 1)
	second := make(chan *workspacev0.WorkspaceBasicExecResult, 1)
	go func() { first <- registry.runWorkspaceBasicExec(context.Background(), entry, request) }()
	<-started
	go func() { second <- registry.runWorkspaceBasicExec(context.Background(), entry, request) }()
	close(release)
	firstResult := <-first
	secondResult := <-second
	if runs.Load() != 1 {
		t.Fatalf("executions = %d, want 1", runs.Load())
	}
	if firstResult.GetOutcome() != "exited" ||
		secondResult.GetOutcome() != "exited" ||
		firstResult.GetRequestFingerprint() != request.GetEnvelope().GetRequestFingerprint() ||
		secondResult.GetRequestFingerprint() != request.GetEnvelope().GetRequestFingerprint() {
		t.Fatalf("replayed results = %+v / %+v", firstResult, secondResult)
	}
}

func TestWorkspaceBasicExecSurvivesCallerDisconnect(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32
	entry := &workspaceMountEntry{
		basicExecRun: func(request *workspacev0.WorkspaceBasicExecRequest) *workspacev0.WorkspaceBasicExecResult {
			runs.Add(1)
			close(started)
			<-release
			return &workspacev0.WorkspaceBasicExecResult{
				Outcome: "exited", RequestFingerprint: request.GetEnvelope().GetRequestFingerprint(),
			}
		},
	}
	registry := testWorkspaceBasicExecRegistry(entry)
	request := testWorkspaceBasicExecRequest("process-1", strings.Repeat("a", 64))
	ctx, cancel := context.WithCancel(context.Background())
	disconnected := make(chan *workspacev0.WorkspaceBasicExecResult, 1)
	go func() { disconnected <- registry.runWorkspaceBasicExec(ctx, entry, request) }()
	<-started
	cancel()
	if result := <-disconnected; result.GetOutcome() != "workspace_exec_result_uncertain" {
		t.Fatalf("disconnect outcome = %q", result.GetOutcome())
	}
	close(release)
	replayed := registry.runWorkspaceBasicExec(context.Background(), entry, request)
	if replayed.GetOutcome() != "exited" || runs.Load() != 1 {
		t.Fatalf("replay outcome = %q, executions = %d", replayed.GetOutcome(), runs.Load())
	}
}

func TestWorkspaceBasicExecRejectsFingerprintChange(t *testing.T) {
	entry := &workspaceMountEntry{basicExecRun: func(*workspacev0.WorkspaceBasicExecRequest) *workspacev0.WorkspaceBasicExecResult {
		return &workspacev0.WorkspaceBasicExecResult{Outcome: "exited"}
	}}
	registry := testWorkspaceBasicExecRegistry(entry)
	first := registry.runWorkspaceBasicExec(context.Background(), entry, testWorkspaceBasicExecRequest("process-1", strings.Repeat("a", 64)))
	if first.GetOutcome() != "exited" {
		t.Fatal(first)
	}
	result := registry.runWorkspaceBasicExec(context.Background(), entry, testWorkspaceBasicExecRequest("process-1", strings.Repeat("b", 64)))
	if result.GetOutcome() != "workspace_exec_fingerprint_conflict" {
		t.Fatal(result)
	}
}

func TestWorkspaceBasicExecRejectsAnotherOperation(t *testing.T) {
	var runs atomic.Int32
	entry := &workspaceMountEntry{
		basicExecRun: func(request *workspacev0.WorkspaceBasicExecRequest) *workspacev0.WorkspaceBasicExecResult {
			runs.Add(1)
			return &workspacev0.WorkspaceBasicExecResult{
				Outcome: "exited", RequestFingerprint: request.GetEnvelope().GetRequestFingerprint(),
			}
		},
	}
	registry := testWorkspaceBasicExecRegistry(entry)
	first := registry.runWorkspaceBasicExec(
		context.Background(), entry,
		testWorkspaceBasicExecRequest("process-1", strings.Repeat("a", 64)),
	)
	second := registry.runWorkspaceBasicExec(
		context.Background(), entry,
		testWorkspaceBasicExecRequest("process-2", strings.Repeat("b", 64)),
	)
	if first.GetOutcome() != "exited" || second.GetOutcome() != "workspace_exec_unavailable" {
		t.Fatalf("outcomes = %q / %q", first.GetOutcome(), second.GetOutcome())
	}
	if runs.Load() != 1 {
		t.Fatalf("executions = %d, want 1", runs.Load())
	}
}

func TestWorkspaceBasicExecRejectsAnotherOperationWhileRunning(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32
	entry := &workspaceMountEntry{
		basicExecRun: func(request *workspacev0.WorkspaceBasicExecRequest) *workspacev0.WorkspaceBasicExecResult {
			runs.Add(1)
			close(started)
			<-release
			return &workspacev0.WorkspaceBasicExecResult{
				Outcome: "exited", RequestFingerprint: request.GetEnvelope().GetRequestFingerprint(),
			}
		},
	}
	registry := testWorkspaceBasicExecRegistry(entry)
	first := make(chan *workspacev0.WorkspaceBasicExecResult, 1)
	go func() {
		first <- registry.runWorkspaceBasicExec(
			context.Background(), entry,
			testWorkspaceBasicExecRequest("process-1", strings.Repeat("a", 64)),
		)
	}()
	<-started
	second := registry.runWorkspaceBasicExec(
		context.Background(), entry,
		testWorkspaceBasicExecRequest("process-2", strings.Repeat("b", 64)),
	)
	close(release)
	if result := <-first; result.GetOutcome() != "exited" {
		t.Fatalf("first outcome = %q", result.GetOutcome())
	}
	if second.GetOutcome() != "workspace_exec_unavailable" {
		t.Fatalf("second outcome = %q", second.GetOutcome())
	}
	if runs.Load() != 1 {
		t.Fatalf("executions = %d, want 1", runs.Load())
	}
}

func TestWorkspaceBasicExecDoesNotRequestManagedProgramMounts(t *testing.T) {
	options := workspaceBasicExecImageCommandOptions()
	if options.ManagedProgram ||
		options.CgroupNamespace ||
		options.CgroupLeaf != "" ||
		options.StartProof ||
		options.Pty {
		t.Fatalf("Workspace BasicExec image command options = %+v", options)
	}
}

func testWorkspaceBasicExecRequest(
	processID string,
	fingerprint string,
) *workspacev0.WorkspaceBasicExecRequest {
	return &workspacev0.WorkspaceBasicExecRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			OperationId: processID, RequestFingerprint: fingerprint,
			WorkspaceMountId: "mount-1", WorkspaceId: "workspace-1", ChannelToken: "channel-token",
			InstanceLeaseId: "process-lease", WriteLeaseId: "process-lease", FencingToken: "capability", FencingGeneration: 1, OperationExpiresAtUnixNano: time.Now().Add(time.Minute).UnixNano(),
		},
		BaseWorkspaceVersionId: "version-1", OwnershipGeneration: 1, WriterGeneration: 1,
		RequestJson: `{"command":["true"],"cwd":"/workspace","env":{},"timeout_ms":1000}`,
	}
}

func testWorkspaceBasicExecRegistry(entry *workspaceMountEntry) *workspaceOperationRegistry {
	entry.workspaceID = "workspace-1"
	entry.channelToken = "channel-token"
	entry.baseVersionID = "version-1"
	entry.fencingGeneration = 1
	registry := newWorkspaceOperationRegistry()
	registry.register("mount-1", entry)
	return registry
}

// Existing finalization/turn-commit fixtures simulate an independently admitted process.
func (entry *workspaceMountEntry) beginWorkspaceExecAdmission() (func(), error) {
	entry.processesMu.Lock()
	if entry.authorityState == workspaceAuthorityFinalizing ||
		entry.recoveryRequired ||
		entry.turnCommitBlocked {
		entry.processesMu.Unlock()
		return func() {}, errors.New("workspace is unavailable for exec admission")
	}
	entry.processAdmissions++
	entry.processesMu.Unlock()
	return func() {
		entry.processesMu.Lock()
		entry.processAdmissions--
		entry.processesMu.Unlock()
	}, nil
}
