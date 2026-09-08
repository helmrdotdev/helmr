package guestd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

func successorExec(authority *workspacev0.WorkspaceRunAuthority) *workspacev0.WorkspaceBasicExecRequest {
	req := testWorkspaceBasicExecRequest("process-1", strings.Repeat("a", 64))
	req.Envelope.ChannelToken = authority.GetChannelToken()
	req.Envelope.FencingGeneration = uint64(authority.GetFence().GetMountFencingGeneration() + 1)
	req.BaseWorkspaceVersionId = "published-version-2"
	req.OwnershipGeneration = authority.GetFence().GetOwnershipGeneration() + 1
	req.WriterGeneration = authority.GetFence().GetWriterGeneration() + 1
	return req
}

func framedBasicExec(t *testing.T, ctx context.Context, registry *workspaceOperationRegistry, request *workspacev0.WorkspaceBasicExecRequest) *workspacev0.WorkspaceBasicExecResult {
	t.Helper()
	var stream bytes.Buffer
	if err := frameio.WriteProtoFrame(&stream, request); err != nil {
		t.Fatal(err)
	}
	if err := handleWorkspaceBasicExecConnection(ctx, &stream, registry); err != nil {
		t.Fatal(err)
	}
	var result workspacev0.WorkspaceBasicExecResult
	if err := frameio.ReadProtoFrame(&stream, &result); err != nil {
		t.Fatal(err)
	}
	return &result
}

func TestWorkspaceBasicExecConsumesCommittedPredecessor(t *testing.T) {
	for _, kind := range []string{"capture", "reset"} {
		t.Run(kind, func(t *testing.T) {
			entry, registry, authority := testWorkspaceFinalizationMount(t)
			if kind == "capture" {
				runWorkspaceCapture(t, registry, testWorkspaceCaptureRequest(t, authority, "11111111-1111-4111-8111-111111111111"))
			} else {
				reset := testWorkspaceResetRequest(t, authority, "11111111-1111-4111-8111-111111111111", workspace.ResetTargetProto(mustEmptyResetTarget(t, "version-1")))
				if response := runWorkspaceReset(t, registry, reset, testWorkspaceRootExchange); response.GetError() != "" {
					t.Fatal(response)
				}
			}
			before, err := workspace.InspectTree(entry.workspaceRoot)
			if err != nil {
				t.Fatal(err)
			}
			entry.basicExecRun = func(*workspacev0.WorkspaceBasicExecRequest) *workspacev0.WorkspaceBasicExecResult {
				after, err := workspace.InspectTree(entry.workspaceRoot)
				if err != nil || after != before {
					return workspaceBasicExecFailure("", "tree_changed", errors.New("handoff changed bytes"))
				}
				return &workspacev0.WorkspaceBasicExecResult{Outcome: "exited"}
			}
			req := successorExec(authority)
			if result := framedBasicExec(t, t.Context(), registry, req); result.GetOutcome() != "exited" {
				t.Fatal(result)
			}
			if entry.authority != nil || entry.baseVersionID != req.GetBaseWorkspaceVersionId() || entry.currentFencingGeneration() != req.GetEnvelope().GetFencingGeneration() {
				t.Fatal("successor authority not installed")
			}
			if _, found, err := entry.readWorkspaceFinalizationJournal(); err != nil || found {
				t.Fatalf("journal retained: %v %v", found, err)
			}
			if _, err := entry.renewWorkspaceRunAuthority(&workspacev0.RenewWorkspaceAuthorityRequest{Previous: authority, NewExpiresAtUnixNano: time.Now().Add(time.Hour).UnixNano()}, time.Now()); err == nil {
				t.Fatal("old Run renewed")
			}
			if _, err := beginTestWorkspaceFinalizationRequest(t.Context(), registry, testWorkspaceFinalizationBeginRequest(authority, "22222222-2222-4222-8222-222222222222", workspace.FinalizationCaptureKind), time.Now()); err == nil {
				t.Fatal("old Run finalized")
			}
			next := proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority)
			next.Fence.BaseWorkspaceVersionId = req.GetBaseWorkspaceVersionId()
			next.Fence.WriterGeneration += 2
			next.Fence.OwnershipGeneration += 2
			next.Fence.MountFencingGeneration += 2
			if release, err := registry.admitProgram(entry, next, time.Now()); err == nil {
				release()
				t.Fatal("Program entered process-owned mount")
			}
		})
	}
}

func TestWorkspaceBasicExecRejectsUncommittedOrInvalidPredecessor(t *testing.T) {
	for _, fault := range []string{"missing", "begun", "prepared", "exchanged", "operation", "fence", "version", "invalid-json", "ownership", "writer", "mount", "base-missing", "expired", "recovery", "commit-blocked", "active-exec", "prune"} {
		t.Run(fault, func(t *testing.T) {
			entry, registry, authority := testWorkspaceFinalizationMount(t)
			runWorkspaceCapture(t, registry, testWorkspaceCaptureRequest(t, authority, "11111111-1111-4111-8111-111111111111"))
			req := successorExec(authority)
			journal, _, err := entry.readWorkspaceFinalizationJournal()
			if err != nil {
				t.Fatal(err)
			}
			switch fault {
			case "missing":
				if err := os.Remove(filepath.Join(entry.finalizationRoot, workspaceFinalizationJournalName)); err != nil {
					t.Fatal(err)
				}
			case "invalid-json":
				if err := os.WriteFile(filepath.Join(entry.finalizationRoot, workspaceFinalizationJournalName), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "begun", "prepared", "exchanged":
				journal.Phase = fault
				if err := entry.writeWorkspaceFinalizationJournal(journal); err != nil {
					t.Fatal(err)
				}
			case "operation":
				journal.OperationID = "22222222-2222-4222-8222-222222222222"
				if err := entry.writeWorkspaceFinalizationJournal(journal); err != nil {
					t.Fatal(err)
				}
			case "fence":
				journal.Fence.WriterGeneration++
				if err := entry.writeWorkspaceFinalizationJournal(journal); err != nil {
					t.Fatal(err)
				}
			case "version":
				journal.Version = "invalid"
				if err := entry.writeWorkspaceFinalizationJournal(journal); err != nil {
					t.Fatal(err)
				}
			case "ownership":
				req.OwnershipGeneration--
			case "writer":
				req.WriterGeneration--
			case "mount":
				req.Envelope.FencingGeneration--
			case "base-missing":
				req.BaseWorkspaceVersionId = ""
			case "expired":
				req.Envelope.OperationExpiresAtUnixNano = time.Now().Add(-time.Second).UnixNano()
			case "recovery":
				entry.recoveryRequired = true
			case "commit-blocked":
				entry.turnCommitBlocked = true
			case "active-exec":
				entry.processAdmissions = 1
			case "prune":
				artifact := filepath.Join(entry.finalizationRoot, workspaceCaptureArtifactName)
				if err := os.Remove(artifact); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(artifact, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(artifact, "unremovable-child"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			entry.basicExecRun = func(*workspacev0.WorkspaceBasicExecRequest) *workspacev0.WorkspaceBasicExecResult {
				t.Error("invalid predecessor executed")
				return &workspacev0.WorkspaceBasicExecResult{Outcome: "exited"}
			}
			if result := framedBasicExec(t, t.Context(), registry, req); result.GetOutcome() == "exited" {
				t.Fatal(result)
			}
			if entry.basicExec != nil || entry.currentFencingGeneration() != uint64(authority.GetFence().GetMountFencingGeneration()) || entry.authority == nil {
				t.Fatal("rejected claim changed ownership")
			}
			if fault == "prune" && !entry.recoveryRequired {
				t.Fatal("partial pruning did not fence recovery")
			}
		})
	}
}

func TestWorkspaceBasicExecChecksClaimAfterWaiting(t *testing.T) {
	for _, cancelClaim := range []bool{false, true} {
		t.Run(map[bool]string{false: "expiry", true: "cancellation"}[cancelClaim], func(t *testing.T) {
			entry, registry, authority := testWorkspaceFinalizationMount(t)
			runWorkspaceCapture(t, registry, testWorkspaceCaptureRequest(t, authority, "11111111-1111-4111-8111-111111111111"))
			req := successorExec(authority)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entry.turnCommitMu.Lock()
			if !cancelClaim {
				req.Envelope.OperationExpiresAtUnixNano = time.Now().Add(25 * time.Millisecond).UnixNano()
			}
			attempted := make(chan struct{})
			result := make(chan *workspacev0.WorkspaceBasicExecResult, 1)
			go func() { close(attempted); result <- registry.runWorkspaceBasicExec(ctx, entry, req) }()
			<-attempted
			if cancelClaim {
				cancel()
			} else {
				time.Sleep(time.Until(time.Unix(0, req.GetEnvelope().GetOperationExpiresAtUnixNano())))
			}
			entry.turnCommitMu.Unlock()
			if res := <-result; res.GetOutcome() == "exited" {
				t.Fatal(res)
			}
			if entry.basicExec != nil {
				t.Fatal("expired/cancelled admission published")
			}
			if _, found, err := entry.readWorkspaceFinalizationJournal(); err != nil || !found {
				t.Fatalf("journal pruned before admission %v %v", found, err)
			}
		})
	}
}

func TestWorkspaceBasicExecReplayBindsAuthorityAndAllowsRenewal(t *testing.T) {
	entry := &workspaceMountEntry{basicExecRun: func(*workspacev0.WorkspaceBasicExecRequest) *workspacev0.WorkspaceBasicExecResult {
		return &workspacev0.WorkspaceBasicExecResult{Outcome: "exited"}
	}}
	registry := testWorkspaceBasicExecRegistry(entry)
	req := testWorkspaceBasicExecRequest("process-1", strings.Repeat("a", 64))
	if result := framedBasicExec(t, t.Context(), registry, req); result.GetOutcome() != "exited" {
		t.Fatal(result)
	}
	for _, field := range []string{"expiry", "base", "owner", "writer", "mount", "lease", "capability", "instance", "renewed"} {
		t.Run(field, func(t *testing.T) {
			replay := proto.Clone(req).(*workspacev0.WorkspaceBasicExecRequest)
			switch field {
			case "expiry":
				replay.Envelope.OperationExpiresAtUnixNano = time.Now().Add(-time.Second).UnixNano()
			case "base":
				replay.BaseWorkspaceVersionId = "different"
			case "owner":
				replay.OwnershipGeneration++
			case "writer":
				replay.WriterGeneration++
			case "mount":
				replay.Envelope.FencingGeneration++
			case "lease":
				replay.Envelope.WriteLeaseId = "different"
			case "capability":
				replay.Envelope.FencingToken = "different"
			case "instance":
				replay.Envelope.InstanceLeaseId = "different"
			case "renewed":
				replay.Envelope.OperationExpiresAtUnixNano = time.Now().Add(time.Hour).UnixNano()
			}
			result := framedBasicExec(t, t.Context(), registry, replay)
			if (result.GetOutcome() == "exited") != (field == "renewed") {
				t.Fatal(result)
			}
			if entry.currentFencingGeneration() != 1 {
				t.Fatal("replay advanced fence")
			}
		})
	}
}

func TestWorkspaceStopSerializesAdmissionAndRetirement(t *testing.T) {
	entry := &workspaceMountEntry{}
	registry := testWorkspaceBasicExecRegistry(entry)
	req := testWorkspaceBasicExecRequest("process-1", strings.Repeat("a", 64))
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	stopped := make(chan error, 1)
	go func() { stopped <- handleWorkspaceStop(t.Context(), server, registry) }()
	if err := frameio.WriteProtoFrame(client, &workspacev0.StopWorkspaceRequest{Envelope: req.GetEnvelope(), FinalizeStop: true}); err != nil {
		t.Fatal(err)
	}
	// Reading only the first response byte proves stop owns the transition and is
	// blocked writing the rest; admission/retirement now race against a real stop.
	var prefix [1]byte
	if _, err := client.Read(prefix[:]); err != nil {
		t.Fatal(err)
	}
	admission := make(chan *workspacev0.WorkspaceBasicExecResult, 1)
	go func() { admission <- registry.runWorkspaceBasicExec(t.Context(), entry, req) }()
	retired := make(chan struct{})
	go func() { registry.retire("mount-1", entry); close(retired) }()
	select {
	case <-admission:
		t.Fatal("admission crossed stop")
	case <-retired:
		t.Fatal("retirement crossed stop")
	case <-time.After(20 * time.Millisecond):
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err == nil {
		t.Fatal("closed response unexpectedly succeeded")
	}
	if result := <-admission; result.GetOutcome() == "exited" {
		t.Fatal(result)
	}
	<-retired
	if entry.basicExec != nil {
		t.Fatal("stop admitted a process")
	}
}

func TestWorkspaceBasicExecDoneAllowsImmediateStop(t *testing.T) {
	entry := &workspaceMountEntry{basicExecRun: func(*workspacev0.WorkspaceBasicExecRequest) *workspacev0.WorkspaceBasicExecResult {
		return &workspacev0.WorkspaceBasicExecResult{Outcome: "exited"}
	}}
	registry := testWorkspaceBasicExecRegistry(entry)
	req := testWorkspaceBasicExecRequest("process-1", strings.Repeat("a", 64))
	if result := framedBasicExec(t, t.Context(), registry, req); result.GetOutcome() != "exited" {
		t.Fatal(result)
	}
	var stream bytes.Buffer
	if err := frameio.WriteProtoFrame(&stream, &workspacev0.StopWorkspaceRequest{Envelope: req.GetEnvelope(), FinalizeStop: true}); err != nil {
		t.Fatal(err)
	}
	if err := handleWorkspaceStop(t.Context(), &stream, registry); err != nil {
		t.Fatal(err)
	}
	if _, release, ok := registry.acquireExact("mount-1", "workspace-1", "channel-token", 1); ok {
		release()
		t.Fatal("stopped mount retained")
	}
}

func TestWorkspaceBasicExecRejectsActiveProgramAndFreshMismatch(t *testing.T) {
	entry, registry, authority := testWorkspaceFinalizationMountUnadmitted(t)
	req := successorExec(authority)
	req.BaseWorkspaceVersionId = "version-1"
	req.Envelope.FencingGeneration = 4
	mismatch := proto.Clone(req).(*workspacev0.WorkspaceBasicExecRequest)
	mismatch.BaseWorkspaceVersionId = "unpublished"
	if result := framedBasicExec(t, t.Context(), registry, mismatch); result.GetOutcome() == "exited" {
		t.Fatal(result)
	}
	release, err := registry.admitProgram(entry, authority, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if result := framedBasicExec(t, t.Context(), registry, req); result.GetOutcome() == "exited" {
		t.Fatal(result)
	}
	if entry.basicExec != nil {
		t.Fatal("active Program lost ownership")
	}
}

func TestWorkspaceBasicExecAndProgramAdmissionAreExclusive(t *testing.T) {
	for i := 0; i < 20; i++ {
		entry, registry, authority := testWorkspaceFinalizationMountUnadmitted(t)
		entry.basicExecRun = func(*workspacev0.WorkspaceBasicExecRequest) *workspacev0.WorkspaceBasicExecResult {
			return &workspacev0.WorkspaceBasicExecResult{Outcome: "exited"}
		}
		req := successorExec(authority)
		req.BaseWorkspaceVersionId = "version-1"
		req.Envelope.FencingGeneration = 4
		start := make(chan struct{})
		program := make(chan bool, 1)
		execution := make(chan bool, 1)
		releaseProgram := make(chan struct{})
		programDone := make(chan struct{})
		go func() {
			<-start
			release, err := registry.admitProgram(entry, authority, time.Now())
			program <- err == nil
			<-releaseProgram
			release()
			close(programDone)
		}()
		go func() {
			<-start
			result := registry.runWorkspaceBasicExec(t.Context(), entry, req)
			execution <- result.GetOutcome() == "exited"
		}()
		close(start)
		p, e := <-program, <-execution
		close(releaseProgram)
		<-programDone
		if p == e {
			t.Fatalf("Program=%v exec=%v; exactly one must own the mount", p, e)
		}
	}
}

func TestWorkspaceBasicExecActiveStopAndDisconnectedRetirement(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	cleaned := make(chan struct{})
	entry := &workspaceMountEntry{cleanup: func() { close(cleaned) }, basicExecRun: func(*workspacev0.WorkspaceBasicExecRequest) *workspacev0.WorkspaceBasicExecResult {
		close(started)
		<-finish
		return &workspacev0.WorkspaceBasicExecResult{Outcome: "exited"}
	}}
	registry := testWorkspaceBasicExecRegistry(entry)
	request := testWorkspaceBasicExecRequest("process-1", strings.Repeat("a", 64))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan *workspacev0.WorkspaceBasicExecResult, 1)
	go func() { result <- registry.runWorkspaceBasicExec(ctx, entry, request) }()
	<-started
	var stream bytes.Buffer
	if err := frameio.WriteProtoFrame(&stream, &workspacev0.StopWorkspaceRequest{Envelope: request.Envelope, FinalizeStop: true}); err != nil {
		t.Fatal(err)
	}
	if err := handleWorkspaceStop(t.Context(), &stream, registry); err == nil {
		t.Fatal("stop accepted active exec")
	}
	cancel()
	if response := <-result; response.GetOutcome() != "workspace_exec_result_uncertain" {
		t.Fatal(response)
	}
	registry.retire("mount-1", entry)
	select {
	case <-cleaned:
		t.Fatal("retirement cleaned image of disconnected active exec")
	default:
	}
	close(finish)
	select {
	case <-cleaned:
	case <-time.After(time.Second):
		t.Fatal("completed exec retained image")
	}
}

func TestWorkspaceStopRejectsNewOwnersUntilRetirement(t *testing.T) {
	for _, committed := range []bool{false, true} {
		for _, failedResponse := range []bool{false, true} {
			t.Run(fmt.Sprintf("committed=%v/failed-response=%v", committed, failedResponse), func(t *testing.T) {
				entry, registry, authority := testWorkspaceFinalizationMountUnadmitted(t)
				if committed {
					release, err := registry.admitProgram(entry, authority, time.Now())
					if err != nil {
						t.Fatal(err)
					}
					release()
					runWorkspaceCapture(t, registry, testWorkspaceCaptureRequest(t, authority, "11111111-1111-4111-8111-111111111111"))
				}
				envelope := &workspacev0.WorkspaceOperationEnvelope{WorkspaceMountId: entry.workspaceMountID, WorkspaceId: entry.workspaceID, ChannelToken: entry.channelToken, FencingGeneration: entry.currentFencingGeneration()}
				stop := &workspacev0.StopWorkspaceRequest{Envelope: envelope, CaptureBeforeStop: true}
				if failedResponse {
					server, client := net.Pipe()
					defer server.Close()
					done := make(chan error, 1)
					go func() { done <- handleWorkspaceStop(t.Context(), server, registry) }()
					if err := frameio.WriteProtoFrame(client, stop); err != nil {
						t.Fatal(err)
					}
					if err := client.Close(); err != nil {
						t.Fatal(err)
					}
					if err := <-done; err == nil {
						t.Fatal("closed stop transport did not fail")
					}
				} else {
					var stream bytes.Buffer
					if err := frameio.WriteProtoFrame(&stream, stop); err != nil {
						t.Fatal(err)
					}
					if err := handleWorkspaceStop(t.Context(), &stream, registry); err != nil {
						t.Fatal(err)
					}
					var response workspacev0.StopWorkspaceResponse
					if err := frameio.ReadProtoFrame(&stream, &response); err != nil {
						t.Fatal(err)
					}
					if response.GetState() != "captured" {
						t.Fatal(&response)
					}
				}
				next := proto.Clone(authority).(*workspacev0.WorkspaceRunAuthority)
				exec := successorExec(authority)
				if committed {
					next.Fence.BaseWorkspaceVersionId = exec.GetBaseWorkspaceVersionId()
					next.Fence.OwnershipGeneration++
					next.Fence.WriterGeneration++
					next.Fence.MountFencingGeneration++
				} else {
					exec.BaseWorkspaceVersionId = entry.baseVersionID
					exec.Envelope.FencingGeneration = entry.currentFencingGeneration()
				}
				if release, err := registry.admitProgram(entry, next, time.Now()); err == nil {
					release()
					t.Fatal("stopping mount admitted Program")
				} else if !strings.Contains(err.Error(), "stopping") {
					t.Fatalf("rejected for another reason: %v", err)
				}
				if result := framedBasicExec(t, t.Context(), registry, exec); result.GetOutcome() != "workspace_exec_unavailable" {
					t.Fatalf("stopping mount exec result: %v", result)
				}
				if entry.basicExec != nil || entry.currentFencingGeneration() != envelope.GetFencingGeneration() {
					t.Fatal("rejected owner mutated mount")
				}
				// Failure stays closed, but the native final-stop request must still retire it.
				var final bytes.Buffer
				if err := frameio.WriteProtoFrame(&final, &workspacev0.StopWorkspaceRequest{Envelope: envelope, FinalizeStop: true}); err != nil {
					t.Fatal(err)
				}
				if err := handleWorkspaceStop(t.Context(), &final, registry); err != nil {
					t.Fatal(err)
				}
				if _, release, ok := registry.acquireExact(entry.workspaceMountID, entry.workspaceID, entry.channelToken, envelope.GetFencingGeneration()); ok {
					release()
					t.Fatal("final stop did not retire")
				}
			})
		}
	}
}

func TestWorkspaceStopPreservesExistingExecResultReplay(t *testing.T) {
	entry := &workspaceMountEntry{workspaceRoot: t.TempDir(), basicExecRun: func(*workspacev0.WorkspaceBasicExecRequest) *workspacev0.WorkspaceBasicExecResult {
		return &workspacev0.WorkspaceBasicExecResult{Outcome: "exited", Stdout: []byte("retained")}
	}}
	registry := testWorkspaceBasicExecRegistry(entry)
	request := testWorkspaceBasicExecRequest("process-1", strings.Repeat("a", 64))
	first := framedBasicExec(t, t.Context(), registry, request)
	if first.GetOutcome() != "exited" {
		t.Fatal(first)
	}
	var stream bytes.Buffer
	if err := frameio.WriteProtoFrame(&stream, &workspacev0.StopWorkspaceRequest{Envelope: request.Envelope, CaptureBeforeStop: true}); err != nil {
		t.Fatal(err)
	}
	if err := handleWorkspaceStop(t.Context(), &stream, registry); err != nil {
		t.Fatal(err)
	}
	if replay := framedBasicExec(t, t.Context(), registry, request); !proto.Equal(first, replay) {
		t.Fatalf("stopping changed existing result: %v", replay)
	}
}

func TestWorkspaceStopRejectsActiveProgramWithoutStartingStop(t *testing.T) {
	entry, registry, authority := testWorkspaceFinalizationMountUnadmitted(t)
	release, err := registry.admitProgram(entry, authority, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var stream bytes.Buffer
	request := &workspacev0.StopWorkspaceRequest{Envelope: &workspacev0.WorkspaceOperationEnvelope{WorkspaceMountId: entry.workspaceMountID, WorkspaceId: entry.workspaceID, ChannelToken: entry.channelToken, FencingGeneration: entry.currentFencingGeneration()}, CaptureBeforeStop: true}
	if err := frameio.WriteProtoFrame(&stream, request); err != nil {
		t.Fatal(err)
	}
	if err := handleWorkspaceStop(t.Context(), &stream, registry); err == nil {
		t.Fatal("stop admitted while Program active")
	}
	if entry.stopping {
		t.Fatal("rejected stop fenced active Program")
	}
}
