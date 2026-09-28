package guestd

import (
	"bytes"
	"context"
	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

func TestProgramResumeGrantRequiresInstalledComputerAuthority(t *testing.T) {
	entry, mounts, authority := testProgramMount(t)
	releaseProgram, err := mounts.admitProgram(entry, authority, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseProgram()
	waits := newWaitingRunRegistry()
	pause := &programv0.CheckpointPauseRequest{
		RunId: "run-1", AttemptNumber: 2, RunLeaseId: "source-lease",
		RunWaitId: "wait-1", CorrelationId: "correlation-1", CheckpointId: "checkpoint-1",
		ResumeAttachId: "attach-1", CheckpointRequestVersion: 3,
	}
	if _, err := waits.registerProgram(pause); err != nil {
		t.Fatal(err)
	}
	request := &computerv0.GrantProgramResumeRequest{
		Authority: authority, RunWaitId: "wait-1", CheckpointId: "checkpoint-1",
	}
	var requestBytes bytes.Buffer
	if err := frameio.WriteProtoFrame(&requestBytes, request); err != nil {
		t.Fatal(err)
	}
	stream := &scriptedNetConn{reader: bytes.NewReader(requestBytes.Bytes())}
	if err := handleProgramResumeGrantConnection(stream, 0, mounts, waits, time.Now); err != nil {
		t.Fatal(err)
	}
	var response computerv0.GrantProgramResumeResponse
	if err := frameio.ReadProtoFrame(bytes.NewReader(stream.written.Bytes()), &response); err != nil {
		t.Fatal(err)
	}
	attach := &programv0.ResumeAttach{
		RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1",
		RunWaitId: "wait-1", CorrelationId: "correlation-1", CheckpointId: "checkpoint-1",
		ResumeAttachId: "attach-1", ResumeRequestVersion: 3,
	}
	if !proto.Equal(response.Attach, attach) {
		t.Fatalf("Guest attachment differs from captured scope: %v", response.Attach)
	}
	entry.processesMu.Lock()
	entry.recoveryRequired = true
	entry.processesMu.Unlock()
	if err := waits.attachResume(attach, &bytes.Buffer{}); err == nil {
		t.Fatal("Computer requiring recovery retained its Program resume grant")
	}
	entry.processesMu.Lock()
	entry.recoveryRequired = false
	entry.processesMu.Unlock()
	if err := waits.attachResume(attach, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	altered := proto.Clone(authority).(*computerv0.ComputerRunAuthority)
	altered.Fence.RunLeaseId = "substituted-lease"
	requestBytes.Reset()
	request.Authority = altered
	if err := frameio.WriteProtoFrame(&requestBytes, request); err != nil {
		t.Fatal(err)
	}
	stream = &scriptedNetConn{reader: bytes.NewReader(requestBytes.Bytes())}
	if err := handleProgramResumeGrantConnection(stream, 0, mounts, waits, time.Now); err == nil {
		t.Fatal("uninstalled restore authority was accepted")
	}
	stale := proto.Clone(authority).(*computerv0.ComputerRunAuthority)
	stale.Fence.WriterGeneration--
	requestBytes.Reset()
	request.Authority = stale
	if err := frameio.WriteProtoFrame(&requestBytes, request); err != nil {
		t.Fatal(err)
	}
	stream = &scriptedNetConn{reader: bytes.NewReader(requestBytes.Bytes())}
	if err := handleProgramResumeGrantConnection(stream, 0, mounts, waits, time.Now); err == nil {
		t.Fatal("stale Computer writer generation was accepted")
	}
}

func TestProgramResumeGrantPreservesFrozenScope(t *testing.T) {
	for _, scenario := range []string{"task", "actor", "actor_turn"} {
		t.Run(scenario, func(t *testing.T) {
			entry, mounts, authority := testProgramMount(t)
			release, err := mounts.admitProgram(entry, authority, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			pause := &programv0.CheckpointPauseRequest{
				RunId: "run-1", AttemptNumber: 2, RunLeaseId: "source-lease",
				RunWaitId: "wait-1", CorrelationId: "correlation-1", CheckpointId: "checkpoint-1",
				ResumeAttachId: "attach-1", CheckpointRequestVersion: 3,
			}
			if scenario != "task" {
				pause.Execution = &programv0.SessionExecution{SessionId: "session-1", RunId: "run-1", AttemptNumber: 2, RunGeneration: 1}
			}
			if scenario == "actor_turn" {
				turn := "turn-1"
				pause.TurnId = &turn
			}
			frozen := proto.Clone(pause).(*programv0.CheckpointPauseRequest)
			waits := newWaitingRunRegistry()
			registration, err := waits.registerProgram(pause)
			if err != nil {
				t.Fatal(err)
			}
			if pause.Execution != nil {
				pause.Execution.SessionId = "mutated-session"
			}
			if pause.TurnId != nil {
				*pause.TurnId = "mutated-turn"
			}
			grant := func() {
				t.Helper()
				request := &computerv0.GrantProgramResumeRequest{
					Authority: authority, RunWaitId: "wait-1", CheckpointId: "checkpoint-1",
				}
				var encoded bytes.Buffer
				if err := frameio.WriteProtoFrame(&encoded, request); err != nil {
					t.Fatal(err)
				}
				stream := &scriptedNetConn{reader: bytes.NewReader(encoded.Bytes())}
				if err := handleProgramResumeGrantConnection(stream, 0, mounts, waits, time.Now); err != nil {
					t.Fatal(err)
				}
				var response computerv0.GrantProgramResumeResponse
				if err := frameio.ReadProtoFrame(bytes.NewReader(stream.written.Bytes()), &response); err != nil {
					t.Fatal(err)
				}
			}
			grant()
			grant()
			attach := &programv0.ResumeAttach{
				Execution: frozen.Execution, TurnId: frozen.TurnId,
				RunId: "run-1", AttemptNumber: 2, RunLeaseId: "run-lease-1",
				RunWaitId: "wait-1", CorrelationId: "correlation-1", CheckpointId: "checkpoint-1",
				ResumeAttachId: "attach-1", ResumeRequestVersion: 3,
			}
			mutations := map[string]func(*programv0.ResumeAttach){
				"turn": func(a *programv0.ResumeAttach) { turn := "different-turn"; a.TurnId = &turn },
			}
			if scenario != "task" {
				mutations["missing_execution"] = func(a *programv0.ResumeAttach) { a.Execution = nil }
				mutations["session"] = func(a *programv0.ResumeAttach) { a.Execution.SessionId = "different-session" }
				mutations["run"] = func(a *programv0.ResumeAttach) { a.Execution.RunId = "different-run" }
				mutations["attempt"] = func(a *programv0.ResumeAttach) { a.Execution.AttemptNumber++ }
				mutations["generation"] = func(a *programv0.ResumeAttach) { a.Execution.RunGeneration++ }
			} else {
				mutations["unexpected_execution"] = func(a *programv0.ResumeAttach) { a.Execution = &programv0.SessionExecution{SessionId: "session-1"} }
			}
			if scenario == "actor_turn" {
				mutations["missing_turn"] = func(a *programv0.ResumeAttach) { a.TurnId = nil }
			}
			for name, mutate := range mutations {
				changed := proto.Clone(attach).(*programv0.ResumeAttach)
				mutate(changed)
				if err := waits.attachResume(changed, &bytes.Buffer{}); err == nil {
					t.Fatalf("accepted changed %s", name)
				}
			}
			if err := waits.attachResume(attach, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			grant()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, accepted, err := registration.wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !proto.Equal(accepted, attach) {
				t.Fatal("accepted attachment changed")
			}
			if err := waits.attachResume(attach, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func testComputerAuthorityEntry() *computerMountEntry {
	return &computerMountEntry{
		computerInstanceID:        "runtime-1",
		computerID:                "computer-1",
		baseComputerDiskVersionID: "version-1",
		channelToken:              "channel-1",
		writerGeneration:          3,
	}
}

func testComputerRunAuthority(expiresAt time.Time) *computerv0.ComputerRunAuthority {
	return &computerv0.ComputerRunAuthority{
		Fence: &computerv0.ComputerAuthorityFence{
			WorkerHostId:              "worker-1",
			WorkerEpoch:               7,
			ComputerInstanceId:        "runtime-1",
			VmPlatformId:              "vm-platform-1",
			ComputerId:                "computer-1",
			RunId:                     "run-1",
			AttemptNumber:             2,
			RunLeaseId:                "run-lease-1",
			LeaseSequence:             5,
			WriterGeneration:          3,
			ExpiresAtUnixNano:         expiresAt.UnixNano(),
			BaseComputerDiskVersionId: "version-1",
		},
		ChannelToken:    "channel-1",
		WriteCapability: "write-capability",
	}
}
