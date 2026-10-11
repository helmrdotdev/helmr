//go:build linux

package firecracker

import (
	"context"
	"errors"
	"github.com/firecracker-microvm/firecracker-go-sdk/vsock"
	"github.com/helmrdotdev/helmr/internal/vm"
	"io"
	"net"
	"testing"
	"time"
)

func TestAgentControlExcludesConcurrentLifecycle(t *testing.T) {
	machine := &guestMachine{}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- machine.WithRunningGuestControl(t.Context(), vm.GuestControlOrdinary, func(context.Context) error { close(entered); <-release; return nil })
	}()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if unlock, err := machine.lockComputer(ctx); err == nil {
		unlock()
		t.Error("lifecycle overlapped authority sampling")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	unlock, err := machine.lockComputer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestAgentControlRejectsPausedAndAllowsResumedAbort(t *testing.T) {
	machine := &guestMachine{}
	hold := &checkpointCapture{machine: machine}
	machine.checkpointHold = hold
	called := false
	exchange := func(context.Context) error { called = true; return nil }
	if err := machine.WithRunningGuestControl(t.Context(), vm.GuestControlOrdinary, exchange); err == nil || called {
		t.Fatal("sampled authority while paused")
	}
	hold.resumed = true
	if err := machine.WithRunningGuestControl(t.Context(), vm.GuestControlOrdinary, exchange); err != nil || !called {
		t.Fatalf("resumed abort control: %v", err)
	}
	machine.checkpointHold = nil
	machine.computerCaptureBlocked = true
	if err := machine.WithRunningGuestControl(t.Context(), vm.GuestControlOrdinary, exchange); err == nil {
		t.Fatal("controlled terminally fenced machine")
	}
	machine.computerCaptureBlocked = false
	machine.closed = true
	if err := machine.WithRunningGuestControl(t.Context(), vm.GuestControlOrdinary, exchange); err == nil {
		t.Fatal("controlled closed machine")
	}
}

func TestAgentControlCancellationJoinsExchange(t *testing.T) {
	machine := &guestMachine{}
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- machine.WithRunningGuestControl(t.Context(), vm.GuestControlOrdinary, func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() })
	}()
	<-entered
	machine.mu.Lock()
	cancel := machine.computerCancel
	machine.mu.Unlock()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("control cancellation: %v", err)
	}
	unlock, err := machine.lockComputer(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	unlock()
}

func TestAgentControlUncertainInstallationFencesPauseUntilActivation(t *testing.T) {
	machine := &guestMachine{}
	lostReply := errors.New("installation response lost after observation")
	if err := machine.WithRunningGuestControl(t.Context(), vm.GuestControlInstallation, func(context.Context) error { return lostReply }); !errors.Is(err, lostReply) {
		t.Fatal(err)
	}
	assertFenced := func() {
		t.Helper()
		if _, err := machine.BeginCheckpoint(t.Context(), vm.SnapshotRequest{ID: "must-not-pause"}); err == nil {
			t.Fatal("uncertain continuation allowed pause")
		}
	}
	assertFenced()
	success := func(context.Context) error { return nil }
	if err := machine.WithRunningGuestControl(t.Context(), vm.GuestControlOrdinary, success); err != nil {
		t.Fatal(err)
	}
	assertFenced()
	if err := machine.WithRunningGuestControl(t.Context(), vm.GuestControlInstallation, success); err != nil {
		t.Fatal(err)
	}
	assertFenced()
	if err := machine.WithRunningGuestControl(t.Context(), vm.GuestControlActivation, func(context.Context) error { return lostReply }); err == nil {
		t.Fatal("lost activation succeeded")
	}
	assertFenced()
	if err := machine.WithRunningGuestControl(t.Context(), vm.GuestControlActivation, success); err != nil {
		t.Fatal(err)
	}
	if _, err := machine.BeginCheckpoint(t.Context(), vm.SnapshotRequest{ID: "new-capture"}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointGuestPreparationOwnsExactHold(t *testing.T) {
	previousDial := dialVsock
	t.Cleanup(func() { dialVsock = previousDial })
	dialVsock = func(context.Context, string, uint32, ...vsock.DialOption) (net.Conn, error) {
		client, server := net.Pipe()
		go func() { defer server.Close(); _, _ = io.Copy(io.Discard, server) }()
		return client, nil
	}
	machine := &guestMachine{}
	handle, err := machine.BeginCheckpoint(t.Context(), vm.SnapshotRequest{ID: "capture"})
	if err != nil {
		t.Fatal(err)
	}
	capture := handle.(*checkpointCapture)
	lost := errors.New("lost freeze receipt")
	if err := capture.PrepareGuest(t.Context(), func(context.Context, vm.Stream) error { return lost }); !errors.Is(err, lost) {
		t.Fatal(err)
	}
	if _, err := capture.CreateSnapshot(t.Context()); err == nil {
		t.Fatal("failed preparation permitted snapshot")
	}
	if !machine.computerCaptureBlocked || machine.checkpointHold != capture {
		t.Fatal("failed freeze released hold")
	}
	if err := machine.WithRunningGuestControl(t.Context(), vm.GuestControlOrdinary, func(context.Context) error { t.Fatal("ordinary exchange entered checkpoint"); return nil }); err == nil {
		t.Fatal("ordinary exchange allowed")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- capture.PrepareGuest(t.Context(), func(context.Context, vm.Stream) error { close(entered); <-release; return nil })
	}()
	<-entered
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := capture.ResumeGuestControl(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("abort overlapped freeze: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if capture.preparationFailed {
		t.Fatal("successful retry retained failure fence")
	}
	if err := capture.ResumeGuestControl(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := capture.PrepareGuest(t.Context(), func(context.Context, vm.Stream) error { t.Fatal("freeze after resume"); return nil }); err == nil {
		t.Fatal("resumed hold allowed freeze")
	}
	if err := capture.CompleteAbort(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := machine.BeginCheckpoint(t.Context(), vm.SnapshotRequest{ID: "next"}); err != nil {
		t.Fatal(err)
	}
	if err := capture.PrepareGuest(t.Context(), func(context.Context, vm.Stream) error { t.Fatal("old hold entered"); return nil }); err == nil {
		t.Fatal("stale handle allowed freeze")
	}
}

func TestCheckpointGuestPreparationRejectsSnapshotAndClose(t *testing.T) {
	for _, state := range []string{"snapshot", "closed"} {
		t.Run(state, func(t *testing.T) {
			machine := &guestMachine{}
			handle, err := machine.BeginCheckpoint(t.Context(), vm.SnapshotRequest{ID: "capture"})
			if err != nil {
				t.Fatal(err)
			}
			capture := handle.(*checkpointCapture)
			if state == "snapshot" {
				capture.attempted = true
			} else {
				machine.closed = true
			}
			if err := capture.PrepareGuest(t.Context(), func(context.Context, vm.Stream) error { t.Fatal("invalid exchange entered"); return nil }); err == nil {
				t.Fatal("invalid capture allowed freeze")
			}
		})
	}
}
