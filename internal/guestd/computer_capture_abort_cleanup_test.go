package guestd

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"net"
	"testing"
	"time"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

// Use admission's real release path: it erases physical grant fields, removes
// the live claim and retains the scoped cleanup result for subsequent replays.
func captureAbortCleanupFixture(t *testing.T) (*computerOperationRegistry, *waitingRunRegistry, *computerv0.ComputerCaptureAbortRequest, func(), <-chan struct{}, <-chan struct{}) {
	t.Helper()
	r, entry, capture := captureBarrierFixture(2)
	r.programClaims = nil
	waits := newWaitingRunRegistry()
	q := &computerv0.ComputerCaptureAbortRequest{Capture: capture, AbortDesiredVersion: capture.DesiredVersion + 1}
	var releaseCancelled func()
	var stopped, peerStopped <-chan struct{}
	for i, member := range capture.Runs {
		grant := testComputerRunAuthority(time.Now().Add(time.Minute))
		grant.ChannelCredential = entry.channelCredential
		grant.Fence.ComputerId, grant.Fence.ComputerInstanceId = capture.ComputerId, capture.ComputerInstanceId
		grant.Fence.WriterGeneration = capture.WriterGeneration
		grant.Fence.RunId, grant.Fence.RunLeaseId, grant.Fence.AttemptNumber = member.RunId, member.RunLeaseId, member.AttemptNumber
		release, err := r.admitProgram(entry, grant, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
		ctx, stop := context.WithCancel(t.Context())
		t.Cleanup(stop)
		r.bindProgramStop(entry, grant, stop)
		m := &computerv0.ComputerCaptureAbortMember{Member: member, Authority: grant}
		if i == 0 {
			releaseCancelled, stopped = release, ctx.Done()
			m.Cancelled, m.Authority = true, nil
		} else {
			peerStopped = ctx.Done()
		}
		q.Members = append(q.Members, m)
		registerCaptureMember(t, waits, capture, i).markFrozen()
	}
	if err := r.sealComputerCapture(capture, time.Now); err != nil {
		t.Fatal(err)
	}
	return r, waits, q, releaseCancelled, stopped, peerStopped
}

func TestCaptureAbortRetainsExitedMemberCleanupAcrossReplay(t *testing.T) {
	for _, prepareFirst := range []bool{false, true} {
		for _, failedCleanup := range []bool{false, true} {
			name := map[bool]string{false: "exit before prepare", true: "exit after prepare"}[prepareFirst]
			name += map[bool]string{false: "/success", true: "/failed proof"}[failedCleanup]
			t.Run(name, func(t *testing.T) {
				r, waits, q, release, stopped, peerStopped := captureAbortCleanupFixture(t)
				claim := r.programClaims[0]
				request := cleanupRequest(claim.authority)
				if prepareFirst {
					if err := r.applyCaptureAbort(t.Context(), waits, q, time.Now); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				if err := r.cleanupProgram(ctx, request); err == nil {
					t.Fatal("ordinary cleanup bypassed the capture seal")
				}
				select {
				case <-stopped:
					t.Fatal("sealed member stopped by ordinary cleanup")
				default:
				}

				var cleanupErr error
				if failedCleanup {
					cleanupErr = errors.New("cancelled scope still contains processes")
					claim.cleanupErr = cleanupErr
				}
				release()
				if claim.stop != nil || !proto.Equal(r.captureRequest, q.Capture) {
					t.Fatal("cleanup did not retire the claim while retaining the seal")
				}
				for range 2 {
					if err := r.applyCaptureAbort(ctx, waits, q, time.Now); err != nil {
						t.Fatalf("prepare replay after cleanup: %v", err)
					}
				}
				prepareAbortFixtureStreams(t, r, waits, q)
				healthy := waits.slots[q.Capture.Runs[1].RunWaitId]
				close(healthy.abortDone)
				q.Activate = true
				for range 2 {
					if err := r.applyCaptureAbort(ctx, waits, q, time.Now); !errors.Is(err, cleanupErr) {
						t.Fatalf("activation/replay lost cleanup result: %v", err)
					}
				}
				select {
				case <-healthy.abortResume:
					if failedCleanup {
						t.Fatal("healthy member resumed without cancellation cleanup proof")
					}
				default:
					if !failedCleanup {
						t.Fatal("healthy member did not resume")
					}
				}
				if r.captureSealed() != failedCleanup {
					t.Fatal("seal does not reflect joined cleanup result")
				}
				select {
				case <-peerStopped:
					t.Fatal("healthy peer stopped")
				case <-waits.slots[q.Capture.Runs[0].RunWaitId].abortResume:
					t.Fatal("cancelled member resumed")
				default:
				}
			})
		}
	}
}

func TestCaptureAbortRetainsExitedMemberCleanupIdentity(t *testing.T) {
	for _, changed := range []string{"none", "run", "lease", "attempt"} {
		t.Run(changed, func(t *testing.T) {
			r, waits, q, release, _, _ := captureAbortCleanupFixture(t)
			claim := r.programClaims[0]
			release() // Natural exit before cancellation: no stopRequested or stop remains.
			switch changed {
			case "run":
				claim.authority.Fence.RunId = "other"
			case "lease":
				claim.authority.Fence.RunLeaseId = "other"
			case "attempt":
				claim.authority.Fence.AttemptNumber++
			}
			err := r.applyCaptureAbort(t.Context(), waits, q, time.Now)
			if changed != "none" {
				if err == nil {
					t.Fatal("foreign cleanup proof accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			prepareAbortFixtureStreams(t, r, waits, q)
			close(waits.slots[q.Capture.Runs[1].RunWaitId].abortDone)
			q.Activate = true
			for range 2 {
				if err := r.applyCaptureAbort(t.Context(), waits, q, time.Now); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestCaptureSealRejectsCleanupAlreadyInProgress(t *testing.T) {
	r, _, q, release, stopped, _ := captureAbortCleanupFixture(t)
	r.captureRequest = nil
	claim := r.programClaims[0]
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- r.cleanupProgram(ctx, cleanupRequest(claim.authority)) }()
	<-stopped
	if err := r.sealComputerCapture(q.Capture, time.Now); err == nil || r.captureSealed() {
		t.Fatal("capture sealed a member whose cleanup is in progress")
	}
	release()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestCaptureAbortReplaysAfterActivationRetiresCancelledMember(t *testing.T) {
	r, waits, q, release, stopped, peerStopped := captureAbortCleanupFixture(t)
	claim := r.programClaims[0]
	if err := r.applyCaptureAbort(t.Context(), waits, q, time.Now); err != nil {
		t.Fatal(err)
	}
	prepareAbortFixtureStreams(t, r, waits, q)
	close(waits.slots[q.Capture.Runs[1].RunWaitId].abortDone)
	released := make(chan struct{})
	go func() {
		<-stopped
		release()
		close(released)
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	q.Activate = true
	if err := r.applyCaptureAbort(ctx, waits, q, time.Now); err != nil {
		t.Fatal(err)
	}
	<-released
	if claim.stop != nil || r.captureSealed() {
		t.Fatal("activation did not retire the cancelled member and clear the seal")
	}
	// The activation succeeded but its reply was lost. Host reconciliation
	// repeats preparation before asking for activation again.
	for _, activate := range []bool{false, true} {
		q.Activate = activate
		if err := r.applyCaptureAbort(ctx, waits, q, time.Now); err != nil {
			t.Fatalf("replay activate=%v: %v", activate, err)
		}
	}
	select {
	case <-peerStopped:
		t.Fatal("replay stopped the healthy peer")
	case <-waits.slots[q.Capture.Runs[0].RunWaitId].abortResume:
		t.Fatal("replay resumed the cancelled member")
	default:
	}
}

func TestCaptureAbortReportsOnlyCompletedCleanupFailure(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending timeout", true: "completed failure"}[completed], func(t *testing.T) {
			r, waits, q, release, _, _ := captureAbortCleanupFixture(t)
			if err := r.applyCaptureAbort(t.Context(), waits, q, time.Now); err != nil {
				t.Fatal(err)
			}
			prepareAbortFixtureStreams(t, r, waits, q)
			if completed {
				r.programClaims[0].cleanupErr = errors.New("scope did not empty")
				release()
			}
			for _, phase := range []string{"prepare", "foreign", "activate", "replay"} {
				request := proto.Clone(q).(*computerv0.ComputerCaptureAbortRequest)
				request.Activate = phase != "prepare"
				if phase == "foreign" {
					request.Capture.CheckpointId = "foreign"
				}
				host, guest := net.Pipe()
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				done := make(chan error, 1)
				go func() { defer guest.Close(); done <- handleComputerCaptureAbort(ctx, guest, 0, r, waits) }()
				if err := frameio.WriteProtoFrame(host, request); err != nil {
					t.Fatal(err)
				}
				var response computerv0.ComputerCaptureAbortResponse
				err := frameio.ReadProtoFrame(host, &response)
				handlerErr := <-done
				switch {
				case phase == "prepare":
					if err != nil || handlerErr != nil || response.CleanupFailed || response.Activated || response.CheckpointId != q.Capture.CheckpointId {
						t.Fatalf("prepare response: %v %v %+v", err, handlerErr, &response)
					}
				case phase == "foreign":
					if err == nil || handlerErr == nil || response.CleanupFailed {
						t.Fatal("foreign authority became definitive cleanup failure")
					}
				case completed:
					if err != nil || handlerErr != nil || !response.CleanupFailed || response.Activated || response.CheckpointId != q.Capture.CheckpointId || response.AbortDesiredVersion != q.AbortDesiredVersion {
						t.Fatalf("wrong definitive response: %v %v %+v", err, handlerErr, &response)
					}
				default:
					if err == nil || response.CleanupFailed || !errors.Is(handlerErr, context.DeadlineExceeded) {
						t.Fatalf("pending cleanup result: %v %v %+v", err, handlerErr, &response)
					}
				}

				cancel()
				host.Close()
			}
			if !r.captureSealed() {
				t.Fatal("failed or pending cleanup removed seal")
			}
			select {
			case <-waits.slots[q.Capture.Runs[1].RunWaitId].abortResume:
				t.Fatal("healthy peer thawed without proof")
			default:
			}
		})
	}
}
