package guestd

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

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
	request := testWorkspaceBasicExecRequest("process-1", strings.Repeat("a", 64))
	first := make(chan *workspacev0.WorkspaceBasicExecResult, 1)
	second := make(chan *workspacev0.WorkspaceBasicExecResult, 1)
	go func() { first <- entry.runWorkspaceBasicExec(context.Background(), request) }()
	<-started
	go func() { second <- entry.runWorkspaceBasicExec(context.Background(), request) }()
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
	request := testWorkspaceBasicExecRequest("process-1", strings.Repeat("a", 64))
	ctx, cancel := context.WithCancel(context.Background())
	disconnected := make(chan *workspacev0.WorkspaceBasicExecResult, 1)
	go func() { disconnected <- entry.runWorkspaceBasicExec(ctx, request) }()
	<-started
	cancel()
	if result := <-disconnected; result.GetOutcome() != "workspace_exec_result_uncertain" {
		t.Fatalf("disconnect outcome = %q", result.GetOutcome())
	}
	close(release)
	replayed := entry.runWorkspaceBasicExec(context.Background(), request)
	if replayed.GetOutcome() != "exited" || runs.Load() != 1 {
		t.Fatalf("replay outcome = %q, executions = %d", replayed.GetOutcome(), runs.Load())
	}
}

func TestWorkspaceBasicExecRejectsFingerprintChange(t *testing.T) {
	entry := &workspaceMountEntry{
		basicExecs: map[string]*workspaceBasicExec{
			"process-1": {
				fingerprint: strings.Repeat("a", 64),
				done:        closedWorkspaceBasicExecDone(),
				result: &workspacev0.WorkspaceBasicExecResult{
					Outcome: "exited", RequestFingerprint: strings.Repeat("a", 64),
				},
			},
		},
	}
	result := entry.runWorkspaceBasicExec(
		context.Background(),
		testWorkspaceBasicExecRequest("process-1", strings.Repeat("b", 64)),
	)
	if result.GetOutcome() != "workspace_exec_fingerprint_conflict" {
		t.Fatalf("outcome = %q", result.GetOutcome())
	}
}

func testWorkspaceBasicExecRequest(
	processID string,
	fingerprint string,
) *workspacev0.WorkspaceBasicExecRequest {
	return &workspacev0.WorkspaceBasicExecRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			OperationId: processID, RequestFingerprint: fingerprint,
		},
		RequestJson: `{"command":["true"],"cwd":"/workspace","env":{},"timeout_ms":1000}`,
	}
}

func closedWorkspaceBasicExecDone() chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}
