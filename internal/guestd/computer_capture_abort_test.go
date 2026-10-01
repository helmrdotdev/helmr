package guestd

import (
	"context"
	"testing"
	"time"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

func captureAbortFixture(t *testing.T, count int) (*computerOperationRegistry, *waitingRunRegistry, *computerv0.ComputerCaptureAbortRequest) {
	t.Helper()
	r, _, capture := captureBarrierFixture(count)
	waits := newWaitingRunRegistry()
	q := &computerv0.ComputerCaptureAbortRequest{Capture: capture, AbortDesiredVersion: capture.DesiredVersion + 1}
	for i, claim := range r.programClaims {
		old := claim.authority.Fence
		grant := testComputerRunAuthority(time.Now().Add(time.Minute))
		grant.ChannelCredential = "token"
		grant.Fence.ComputerId = old.ComputerId
		grant.Fence.ComputerInstanceId = old.ComputerInstanceId
		grant.Fence.WriterGeneration = old.WriterGeneration
		grant.Fence.RunId = old.RunId
		grant.Fence.RunLeaseId = old.RunLeaseId
		grant.Fence.AttemptNumber = old.AttemptNumber
		claim.authority = proto.Clone(grant).(*computerv0.ComputerRunAuthority)
		q.Members = append(q.Members, &computerv0.ComputerCaptureAbortMember{Member: capture.Runs[i], Authority: grant})
		registerCaptureMember(t, waits, capture, i).markFrozen()
	}
	if err := r.sealComputerCapture(capture, time.Now); err != nil {
		t.Fatal(err)
	}
	return r, waits, q
}

func TestCaptureAbortInstallsWholeSetBeforeActivation(t *testing.T) {
	r, waits, q := captureAbortFixture(t, 2)
	for _, claim := range r.programClaims {
		claim.authority.Fence.ExpiresAtUnixNano = time.Now().Add(-time.Second).UnixNano()
	}
	bad := proto.Clone(q).(*computerv0.ComputerCaptureAbortRequest)
	bad.Members[1].Authority.Fence.RunLeaseId = "foreign"
	if err := r.applyCaptureAbort(t.Context(), waits, bad, time.Now); err == nil {
		t.Fatal("foreign member accepted")
	}
	if r.captureAbort != nil || r.programClaims[0].authority.Fence.ExpiresAtUnixNano > time.Now().UnixNano() {
		t.Fatal("partial authority installation")
	}
	for range 2 {
		if err := r.applyCaptureAbort(t.Context(), waits, q, time.Now); err != nil {
			t.Fatal(err)
		}
	}
	if !r.captureSealed() {
		t.Fatal("installation opened admission")
	}
	for _, slot := range waits.slots {
		select {
		case <-slot.abortResume:
			t.Fatal("installation released a member")
		default:
		}
	}
	q.Activate = true
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.applyCaptureAbort(ctx, waits, q, time.Now) }()
	for _, slot := range waits.slots {
		select {
		case <-slot.abortResume:
		case <-ctx.Done():
			t.Fatal("activation did not release member")
		}
	}
	select {
	case err := <-done:
		t.Fatalf("acknowledged before member thaw: %v", err)
	default:
	}
	for _, slot := range waits.slots {
		close(slot.abortDone)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r.captureSealed() {
		t.Fatal("completed abort kept admission sealed")
	}
	if err := r.applyCaptureAbort(ctx, waits, q, time.Now); err != nil {
		t.Fatalf("lost reply replay: %v", err)
	}
}

func TestCaptureAbortCancellationJoinsCleanup(t *testing.T) {
	r, waits, q := captureAbortFixture(t, 1)
	claim := r.programClaims[0]
	claim.done = make(chan struct{})
	stopped := make(chan struct{})
	claim.stop = func() { close(stopped) }
	q.Members[0].Cancelled = true
	q.Members[0].Authority = nil
	if err := r.applyCaptureAbort(t.Context(), waits, q, time.Now); err != nil {
		t.Fatal(err)
	}
	q.Activate = true
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.applyCaptureAbort(ctx, waits, q, time.Now) }()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("cancellation did not stop member")
	}
	select {
	case <-waits.slots[q.Capture.Runs[0].RunWaitId].abortResume:
		t.Fatal("cancelled member thawed")
	default:
	}
	select {
	case err := <-done:
		t.Fatalf("acknowledged before cleanup: %v", err)
	default:
	}
	close(claim.done)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCaptureAbortRejectsStaleOrExpiredReceipt(t *testing.T) {
	for _, change := range []func(*computerv0.ComputerCaptureAbortRequest){
		func(q *computerv0.ComputerCaptureAbortRequest) { q.Capture.CheckpointId = "foreign" },
		func(q *computerv0.ComputerCaptureAbortRequest) { q.AbortDesiredVersion++ },
		func(q *computerv0.ComputerCaptureAbortRequest) {
			q.Members[0].Authority.Fence.ExpiresAtUnixNano = time.Now().Add(-time.Second).UnixNano()
		},
		func(q *computerv0.ComputerCaptureAbortRequest) { q.Members[1] = q.Members[0] },
		func(q *computerv0.ComputerCaptureAbortRequest) { q.Activate = true },
	} {
		r, waits, q := captureAbortFixture(t, 2)
		change(q)
		if err := r.applyCaptureAbort(t.Context(), waits, q, time.Now); err == nil {
			t.Fatal("invalid abort accepted")
		}
		if r.captureAbort != nil || !r.captureSealed() {
			t.Fatal("invalid abort changed hold")
		}
	}
}

func completeAbortFixture(t *testing.T, r *computerOperationRegistry, waits *waitingRunRegistry, q *computerv0.ComputerCaptureAbortRequest) {
	t.Helper()
	if err := r.applyCaptureAbort(t.Context(), waits, q, time.Now); err != nil {
		t.Fatal(err)
	}
	for _, slot := range waits.slots {
		close(slot.abortDone)
	}
	q.Activate = true
	if err := r.applyCaptureAbort(t.Context(), waits, q, time.Now); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureAbortBeforeSealAfterPreviousAbort(t *testing.T) {
	r, waits, first := captureAbortFixture(t, 1)
	completeAbortFixture(t, r, waits, first)
	previous := r.captureAbort
	next := proto.Clone(first).(*computerv0.ComputerCaptureAbortRequest)
	next.Activate = false
	next.Capture.CheckpointId = "second-checkpoint"
	next.Capture.DesiredVersion = first.AbortDesiredVersion + 1
	next.AbortDesiredVersion = next.Capture.DesiredVersion + 1
	waits = newWaitingRunRegistry()
	registerCaptureMember(t, waits, next.Capture, 0).markFrozen()
	stale := proto.Clone(next).(*computerv0.ComputerCaptureAbortRequest)
	stale.Capture.DesiredVersion = first.Capture.DesiredVersion
	stale.AbortDesiredVersion = first.AbortDesiredVersion
	if err := r.applyCaptureAbort(t.Context(), waits, stale, time.Now); err == nil {
		t.Fatal("stale capture replaced completed receipt")
	}
	if r.captureAbort != previous || r.captureSealed() {
		t.Fatal("rejected capture changed receipt")
	}
	if err := r.sealComputerCapture(first.Capture, time.Now); err == nil {
		t.Fatal("old seal reopened completed capture")
	}
	completeAbortFixture(t, r, waits, next)
	if r.captureAbort == previous || r.captureSealed() {
		t.Fatal("next early abort did not complete")
	}
	if err := r.applyCaptureAbort(t.Context(), waits, first, time.Now); err == nil {
		t.Fatal("previous abort replay replaced newer capture")
	}
}

func TestCaptureAbortBeforeSealAfterRestoration(t *testing.T) {
	for _, activated := range []bool{false, true} {
		t.Run(map[bool]string{false: "restore pending", true: "restore completed"}[activated], func(t *testing.T) {
			r, waits, q := captureAbortFixture(t, 1)
			r.captureRequest = nil
			previous := &computerv0.MaterializeComputerRequest{RestoredCheckpointId: "previous-checkpoint"}
			r.restoredMaterialization = previous
			r.restoreInstallation = &computerv0.ComputerRestoreInstallation{DesiredVersion: q.Capture.DesiredVersion - 1}
			r.restoreActivated = activated
			if !activated {
				if err := r.applyCaptureAbort(t.Context(), waits, q, time.Now); err == nil {
					t.Fatal("abort interrupted incomplete restoration")
				}
				if r.restoredMaterialization != previous || r.captureAbort != nil {
					t.Fatal("rejected abort removed restore receipt")
				}
				return
			}
			completeAbortFixture(t, r, waits, q)
			if r.restoredMaterialization != nil || r.restoreInstallation != nil || r.restoreActivated || r.captureSealed() {
				t.Fatal("early abort did not retire completed restore receipt")
			}
		})
	}
}
