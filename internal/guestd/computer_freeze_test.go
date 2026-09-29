package guestd

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func registerCaptureMember(t *testing.T, waits *waitingRunRegistry, request *computerv0.FreezeComputerRequest, index int) waitingRunRegistration {
	t.Helper()
	member := request.Runs[index]
	registration, err := waits.registerProgram(&programv0.CheckpointPauseRequest{RunId: member.RunId, AttemptNumber: member.AttemptNumber, RunLeaseId: member.RunLeaseId, RunWaitId: member.RunWaitId, CheckpointId: request.CheckpointId, CheckpointRequestVersion: request.DesiredVersion, CorrelationId: "correlation-" + member.RunId, ResumeAttachId: "attach-" + member.RunId})
	if err != nil {
		t.Fatal(err)
	}
	return registration
}

func TestComputerFreezeWaitsForEveryMember(t *testing.T) {
	mounts, _, request := captureBarrierFixture(2)
	waits := newWaitingRunRegistry()
	if err := mounts.sealComputerCapture(request, time.Now); err != nil {
		t.Fatal(err)
	}
	identity, changed, err := waits.computerCaptureProgress(request)
	if err != nil || identity != nil {
		t.Fatalf("missing waits: %v %v", identity, err)
	}
	first := registerCaptureMember(t, waits, request, 0)
	select {
	case <-changed:
	default:
		t.Fatal("registration did not notify coordinator")
	}
	first.markFrozen()
	identity, changed, err = waits.computerCaptureProgress(request)
	if err != nil || identity != nil {
		t.Fatalf("partial freeze: %v %v", identity, err)
	}
	second := registerCaptureMember(t, waits, request, 1)
	identity, changed, err = waits.computerCaptureProgress(request)
	if err != nil || identity != nil {
		t.Fatalf("unfrozen member: %v %v", identity, err)
	}
	second.markFrozen()
	select {
	case <-changed:
	default:
		t.Fatal("freeze did not notify coordinator")
	}
	identity, _, err = waits.computerCaptureProgress(request)
	if err != nil || identity == nil || len(identity.Runs) != 2 {
		t.Fatalf("complete freeze: %v %v", identity, err)
	}
	if err = verifyFrozenComputer(mounts, waits, identity); err != nil {
		t.Fatal(err)
	}
	for _, member := range identity.Runs {
		if member.CorrelationId != "correlation-"+member.RunId {
			t.Fatal("correlation invented")
		}
	}
}

// replaceComputerDiskFlush keeps tests from flushing the test machine's disks,
// whose duration depends on unrelated writers, and counts Guest flushes.
func replaceComputerDiskFlush(t *testing.T) *atomic.Int32 {
	t.Helper()
	var flushes atomic.Int32
	previous := flushComputerDisk
	flushComputerDisk = func() { flushes.Add(1) }
	t.Cleanup(func() { flushComputerDisk = previous })
	return &flushes
}

func TestComputerFreezeConnectionReturnsCompleteProof(t *testing.T) {
	flushes := replaceComputerDiskFlush(t)
	for _, count := range []int{0, 2} {
		flushes.Store(0)
		mounts, _, request := captureBarrierFixture(count)
		waits := newWaitingRunRegistry()
		for i := range count {
			registerCaptureMember(t, waits, request, i).markFrozen()
		}
		client, server := net.Pipe()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		done := make(chan error, 1)
		go func() { defer server.Close(); _, err := handleConnection(ctx, server, nil, waits, mounts); done <- err }()
		if err := wire.WriteStreamFrameHeader(client, wire.StreamHeader{Type: wire.StreamTypeComputerFreeze, ComputerID: request.ComputerId, CheckpointID: request.CheckpointId}, 0); err != nil {
			t.Fatal(err)
		}
		if err := frameio.WriteProtoFrame(client, request); err != nil {
			t.Fatal(err)
		}
		var response computerv0.FreezeComputerResponse
		if err := frameio.ReadProtoFrame(client, &response); err != nil {
			t.Fatal(err)
		}
		if got := flushes.Load(); got != 1 {
			t.Fatalf("freeze acknowledged after %d disk flushes", got)
		}
		client.Close()
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if response.DesiredVersion != request.DesiredVersion || response.MembershipRevision != request.MembershipRevision || len(response.GetIdentity().GetRuns()) != count {
			t.Fatalf("response=%v", &response)
		}
		if err := verifyFrozenComputer(mounts, waits, response.Identity); err != nil {
			t.Fatal(err)
		}
	}
}

func TestComputerFreezeRejectsWrongWaitProof(t *testing.T) {
	for _, change := range []func(*waitingRunSlot){
		func(s *waitingRunSlot) { s.runLeaseID = "other" },
		func(s *waitingRunSlot) { s.checkpointID = "other" },
		func(s *waitingRunSlot) { s.checkpointRequestVersion++ },
		func(s *waitingRunSlot) { s.correlationID = "" },
		func(s *waitingRunSlot) { s.accepted = &programv0.ResumeAttach{} },
	} {
		_, _, request := captureBarrierFixture(1)
		waits := newWaitingRunRegistry()
		registration := registerCaptureMember(t, waits, request, 0)
		registration.markFrozen()
		change(registration.slot)
		if _, _, err := waits.computerCaptureProgress(request); err == nil {
			t.Fatal("changed wait proof accepted")
		}
	}
}

func TestComputerFreezeCancellationKeepsAdmissionClosed(t *testing.T) {
	mounts, _, request := captureBarrierFixture(1)
	waits := newWaitingRunRegistry()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- handleComputerFreezeConnection(ctx, server, 0, mounts, waits) }()
	if err := frameio.WriteProtoFrame(client, request); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for !mounts.captureSealed() {
		select {
		case err := <-done:
			t.Fatalf("freeze returned before seal: %v", err)
		case <-deadline:
			t.Fatal("capture not sealed")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("freeze wait did not stop")
	}
	if !mounts.captureSealed() {
		t.Fatal("uncertain freeze reopened admission")
	}
}
