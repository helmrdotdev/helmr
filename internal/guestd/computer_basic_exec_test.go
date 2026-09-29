package guestd

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
)

func TestComputerBasicExecReplaysOneExecution(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32
	entry := &computerMountEntry{
		basicExecRun: func(_ context.Context, request *computerv0.ComputerBasicExecRequest) *computerv0.ComputerBasicExecResult {
			if runs.Add(1) == 1 {
				close(started)
			}
			<-release
			return &computerv0.ComputerBasicExecResult{
				Outcome: "exited", RequestFingerprint: request.GetEnvelope().GetRequestFingerprint(),
			}
		},
	}
	registry := testComputerBasicExecRegistry(t, entry)
	request := testComputerBasicExecRequest("process-1", strings.Repeat("a", 64))
	first := make(chan *computerv0.ComputerBasicExecResult, 1)
	second := make(chan *computerv0.ComputerBasicExecResult, 1)
	go func() { first <- registry.runComputerBasicExec(context.Background(), entry, request) }()
	<-started
	go func() { second <- registry.runComputerBasicExec(context.Background(), entry, request) }()
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

func TestComputerBasicExecSurvivesCallerDisconnect(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var runs atomic.Int32
	entry := &computerMountEntry{
		basicExecRun: func(_ context.Context, request *computerv0.ComputerBasicExecRequest) *computerv0.ComputerBasicExecResult {
			runs.Add(1)
			close(started)
			<-release
			return &computerv0.ComputerBasicExecResult{
				Outcome: "exited", RequestFingerprint: request.GetEnvelope().GetRequestFingerprint(),
			}
		},
	}
	registry := testComputerBasicExecRegistry(t, entry)
	request := testComputerBasicExecRequest("process-1", strings.Repeat("a", 64))
	ctx, cancel := context.WithCancel(context.Background())
	disconnected := make(chan *computerv0.ComputerBasicExecResult, 1)
	go func() { disconnected <- registry.runComputerBasicExec(ctx, entry, request) }()
	<-started
	cancel()
	if result := <-disconnected; result.GetOutcome() != "computer_command_result_uncertain" {
		t.Fatalf("disconnect outcome = %q", result.GetOutcome())
	}
	close(release)
	replayed := registry.runComputerBasicExec(context.Background(), entry, request)
	if replayed.GetOutcome() != "exited" || runs.Load() != 1 {
		t.Fatalf("replay outcome = %q, executions = %d", replayed.GetOutcome(), runs.Load())
	}
}

func TestComputerBasicExecRejectsFingerprintChange(t *testing.T) {
	entry := &computerMountEntry{basicExecRun: func(context.Context, *computerv0.ComputerBasicExecRequest) *computerv0.ComputerBasicExecResult {
		return &computerv0.ComputerBasicExecResult{Outcome: "exited"}
	}}
	registry := testComputerBasicExecRegistry(t, entry)
	first := registry.runComputerBasicExec(context.Background(), entry, testComputerBasicExecRequest("process-1", strings.Repeat("a", 64)))
	if first.GetOutcome() != "exited" {
		t.Fatal(first)
	}
	result := registry.runComputerBasicExec(context.Background(), entry, testComputerBasicExecRequest("process-1", strings.Repeat("b", 64)))
	if result.GetOutcome() != "computer_command_fingerprint_conflict" {
		t.Fatal(result)
	}
}

func TestComputerBasicExecSharesInstance(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var runs atomic.Int32
	entry := &computerMountEntry{basicExecRun: func(_ context.Context, r *computerv0.ComputerBasicExecRequest) *computerv0.ComputerBasicExecResult {
		runs.Add(1)
		started <- struct{}{}
		<-release
		return &computerv0.ComputerBasicExecResult{Outcome: "exited", RequestFingerprint: r.GetEnvelope().GetRequestFingerprint()}
	}}
	registry := testComputerBasicExecRegistry(t, entry)
	done := make(chan *computerv0.ComputerBasicExecResult, 2)
	for _, id := range []string{"one", "two"} {
		go func() {
			done <- registry.runComputerBasicExec(t.Context(), entry, testComputerBasicExecRequest(id, strings.Repeat("a", 64)))
		}()
	}
	<-started
	<-started
	close(release)
	for range 2 {
		if result := <-done; result.GetOutcome() != "exited" {
			t.Fatal(result)
		}
	}
	if runs.Load() != 2 {
		t.Fatalf("executions=%d", runs.Load())
	}
}

func TestComputerBasicExecDoesNotRequestManagedProgramMounts(t *testing.T) {
	options := computerBasicExecImageCommandOptions("exec-test")
	if options.ManagedProgram ||
		!options.CgroupNamespace ||
		options.CgroupLeaf != "exec-test" ||
		options.StartProof ||
		options.Pty {
		t.Fatalf("Computer BasicExec image command options = %+v", options)
	}
}

func testComputerBasicExecRequest(
	commandID string,
	fingerprint string,
) *computerv0.ComputerBasicExecRequest {
	return &computerv0.ComputerBasicExecRequest{
		Envelope: &computerv0.ComputerCommandAuthority{
			OperationId: commandID, RequestFingerprint: fingerprint,
			ComputerInstanceId: "instance-1", ComputerId: "computer-1", ChannelToken: "channel-token",
			WriterGeneration: 1, OperationExpiresAtUnixNano: time.Now().Add(time.Minute).UnixNano(),
		},
		RequestJson: `{"command":["true"],"cwd":"/workspace","env":{},"timeout_ms":1000}`,
	}
}

func testComputerBasicExecRegistry(t *testing.T, entry *computerMountEntry) *computerOperationRegistry {
	t.Setenv("HELMR_GUESTD_TMPDIR", t.TempDir())
	entry.computerID = "computer-1"
	entry.computerInstanceID = "instance-1"
	entry.writerGeneration = 1
	entry.channelToken = "channel-token"
	entry.baseComputerDiskVersionID = "version-1"
	entry.writerGeneration = 1
	registry := newComputerOperationRegistry()
	registry.register(entry.computerInstanceID, entry)
	t.Cleanup(func() { registry.retire(entry.computerInstanceID, entry) })
	return registry
}
