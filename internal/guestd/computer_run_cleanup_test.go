package guestd

import (
	"context"
	"errors"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
	"testing"
	"time"
)

func cleanupRequest(a *computerv0.ComputerRunAuthority) *computerv0.ComputerRunCleanupRequest {
	f := a.GetFence()
	return &computerv0.ComputerRunCleanupRequest{ComputerId: f.ComputerId, ComputerInstanceId: f.ComputerInstanceId, WriterGeneration: f.WriterGeneration, ChannelCredential: a.ChannelCredential, RunId: f.RunId, RunLeaseId: f.RunLeaseId, AttemptNumber: f.AttemptNumber}
}
func TestProgramCleanupWaitsForScopeAndPreservesPeer(t *testing.T) {
	entry, r, a := testProgramMount(t)
	release, err := r.admitProgram(entry, a, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	peer := proto.Clone(a).(*computerv0.ComputerRunAuthority)
	peer.Fence.RunId = "peer"
	peer.Fence.RunLeaseId = "peer-lease"
	peerRelease, err := r.admitProgram(entry, peer, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer peerRelease()
	stopped := make(chan struct{})
	r.bindProgramStop(entry, a, func() { close(stopped) })
	peerCtx, peerStop := context.WithCancel(t.Context())
	defer peerStop()
	r.bindProgramStop(entry, peer, peerStop)
	done := make(chan error, 1)
	go func() { done <- r.cleanupProgram(t.Context(), cleanupRequest(a)) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("target not stopped")
	}
	select {
	case err := <-done:
		t.Fatalf("acknowledged before scope cleanup: %v", err)
	default:
	}
	if peerCtx.Err() != nil {
		t.Fatal("peer cancelled")
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := r.cleanupProgram(t.Context(), cleanupRequest(a)); err != nil {
			t.Fatal(err)
		}
	}
	if rel, err := r.admitProgram(entry, a, time.Now); err == nil {
		rel()
		t.Fatal("retired lease relaunched")
	}
	if peerCtx.Err() != nil {
		t.Fatal("peer cancelled by replay")
	}
}
func TestProgramCleanupBlocksDelayedAdmissionAndRejectsStaleIdentity(t *testing.T) {
	entry, r, a := testProgramMount(t)
	request := cleanupRequest(a)
	for _, alter := range []func(*computerv0.ComputerRunCleanupRequest){
		func(v *computerv0.ComputerRunCleanupRequest) { v.WriterGeneration++ },
		func(v *computerv0.ComputerRunCleanupRequest) { v.ChannelCredential = "wrong" },
		func(v *computerv0.ComputerRunCleanupRequest) { v.ComputerInstanceId = "wrong" },
	} {
		wrong := proto.Clone(request).(*computerv0.ComputerRunCleanupRequest)
		alter(wrong)
		if err := r.cleanupProgram(t.Context(), wrong); err == nil {
			t.Fatal("stale cleanup accepted")
		}
	}
	if err := r.cleanupProgram(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if release, err := r.admitProgram(entry, a, time.Now); err == nil {
		release()
		t.Fatal("late launch accepted after absence proof")
	}
	request.AttemptNumber++
	if err := r.cleanupProgram(t.Context(), request); err == nil {
		t.Fatal("changed attempt accepted")
	}
}
func TestProgramCleanupRetainsFailedProof(t *testing.T) {
	entry, r, a := testProgramMount(t)
	release, err := r.admitProgram(entry, a, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	claim := r.bindProgramStop(entry, a, func() {})
	r.mu.Lock()
	claim.cleanupErr = errors.New("cgroup is not empty")
	r.mu.Unlock()
	release()
	for range 2 {
		if err := r.cleanupProgram(t.Context(), cleanupRequest(a)); err == nil {
			t.Fatal("failed cleanup became proof")
		}
	}
}
func TestProgramCleanupBeforeStopBinding(t *testing.T) {
	entry, r, a := testProgramMount(t)
	release, err := r.admitProgram(entry, a, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// Mark stop intent even when the caller loses its connection before the handler binds.
	if err := r.cleanupProgram(ctx, cleanupRequest(a)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	stopped := false
	r.bindProgramStop(entry, a, func() { stopped = true })
	if !stopped {
		t.Fatal("lost cleanup request allowed launch")
	}
	release()
	if err := r.cleanupProgram(t.Context(), cleanupRequest(a)); err != nil {
		t.Fatal(err)
	}
}
