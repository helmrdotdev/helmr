package computerhost

import (
	"context"
	"errors"
	"fmt"
	"sync"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// CaptureRuns joins the resident wait owners with the physical capture
// owner. Its zero value is usable. A capture seals the Instance until its source
// has been excluded or an abort has durably resumed the same source.
type CaptureRuns struct {
	mu     sync.Mutex
	waits  map[string]*CaptureWait
	sealed map[preparedMachineRef]bool
}

// CaptureWait is one resident wait registered for capture. Its owner receives
// dispatched member pauses from Pauses and must call Detach when the wait
// ends; later calls return the first result.
type CaptureMemberResume func(context.Context, workerapi.CaptureAbortMember, bool) (*computerv0.ComputerRunAuthority, error)

type CaptureWait struct {
	resume   CaptureMemberResume
	runs     *CaptureRuns
	lease    workerapi.RunLeaseAssignment
	waitID   string
	requests chan *MemberPause
	closed   chan struct{}
	pause    *MemberPause

	detachOnce sync.Once
	detachErr  error
}

// MemberPause is one dispatched request to pause a waiting member before the
// physical capture. Its receiver must Settle it exactly once.
type MemberPause struct {
	ctx      context.Context
	abort    context.CancelCauseFunc
	target   workerapi.RuntimeReconcileTarget
	member   workerapi.RuntimeCaptureRun
	ready    chan error
	finished chan struct{}
	result   error
	resumed  bool
}

// Context bounds the member's pause work; it ends when the capture is aborted.
func (p *MemberPause) Context() context.Context { return p.ctx }

// Target is the capture operation the member is paused for.
func (p *MemberPause) Target() workerapi.RuntimeReconcileTarget { return p.target }

// Member is the exact local grant being paused.
func (p *MemberPause) Member() workerapi.RuntimeCaptureRun { return p.member }

// Abort cancels the capture with cause.
func (p *MemberPause) Abort(cause error) { p.abort(cause) }

// Settle reports the member's pause result and then stays joined until the
// physical owner has finished capture and source exclusion, even if the
// caller has since been cancelled. It returns the member's error joined with
// the capture result; nil means the member was captured and released.
func (p *MemberPause) Settle(err error) error {
	p.ready <- err
	<-p.finished
	if p.resumed {
		return nil
	}
	return errors.Join(err, p.result)
}

// Register records a resident wait for capture. At most one wait per Run may be
// registered, and none on an Instance whose capture has started.
func (r *CaptureRuns) Register(lease workerapi.RunLeaseAssignment, waitID string, resume CaptureMemberResume) (*CaptureWait, error) {
	if lease.ID == "" || lease.RunID == "" || lease.ComputerInstanceID == "" || lease.WorkerEpoch <= 0 || waitID == "" {
		return nil, errors.New("capture wait identity is incomplete")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ref := preparedMachineRef{id: lease.ComputerInstanceID, epoch: lease.WorkerEpoch}
	if r.sealed[ref] || r.waits[lease.RunID] != nil {
		return nil, errors.New("capture wait is sealed or already registered")
	}
	if r.waits == nil {
		r.waits = make(map[string]*CaptureWait)
	}
	entry := &CaptureWait{resume: resume, runs: r, lease: lease, waitID: waitID, requests: make(chan *MemberPause, 1), closed: make(chan struct{})}
	r.waits[lease.RunID] = entry
	return entry, nil
}

// Pauses delivers at most one member pause dispatched to this wait.
func (w *CaptureWait) Pauses() <-chan *MemberPause { return w.requests }

// Detach unregisters the wait. If a pause was dispatched, Detach aborts it
// unless it has finished and joins the physical owner, returning its result.
func (w *CaptureWait) Detach() error {
	w.detachOnce.Do(func() {
		r := w.runs
		r.mu.Lock()
		pause := w.pause
		if pause == nil {
			if r.waits[w.lease.RunID] == w {
				delete(r.waits, w.lease.RunID)
			}
			close(w.closed)
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		// Dispatch and unregister are serialized by the registry lock. A waiter
		// that selects a different event still joins the committed physical owner.
		select {
		case <-pause.finished:
		default:
			pause.abort(errors.New("computer capture member left its wait"))
			<-pause.finished
		}
		w.detachErr = pause.result
		r.mu.Lock()
		if r.waits[w.lease.RunID] == w {
			delete(r.waits, w.lease.RunID)
		}
		close(w.closed)
		r.mu.Unlock()
	})
	return w.detachErr
}

// capture waits for every member's pause proof before invoking capture. That
// callback owns whole-Computer verification and publication. The exclusion
// callback must prove source shutdown and runs after any dispatched pause, on
// success or failure. Returning nil releases members as detached, never
// as successfully completed Runs. Errors remain errors for every paused member.
func (r *CaptureRuns) capture(ctx context.Context, target workerapi.RuntimeReconcileTarget, capture, exclude func(context.Context) error) (retErr error) {
	if _, err := computerFreezeRequest(target); err != nil {
		return err
	}
	if capture == nil || exclude == nil {
		return errors.New("physical capture and exclusion owners are required")
	}
	captureCtx, abort := context.WithCancelCause(ctx)
	defer abort(nil)
	r.mu.Lock()
	ref := preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}
	if r.sealed[ref] {
		r.mu.Unlock()
		return errors.New("computer capture already has an owner")
	}
	requests := make([]*MemberPause, 0, len(target.Capture.Runs))
	entries := make([]*CaptureWait, 0, len(target.Capture.Runs))
	for _, member := range target.Capture.Runs {
		entry := r.waits[member.RunID]
		if entry == nil || entry.lease.ID != member.RunLeaseID || entry.lease.AttemptNumber != member.AttemptNumber || entry.waitID != member.RunWaitID || entry.lease.ComputerInstanceID != target.ID || entry.lease.WorkerEpoch != target.WorkerEpoch || entry.lease.ComputerID != target.Source.ComputerID || entry.lease.WriterGeneration != target.Source.WriterGeneration {
			r.mu.Unlock()
			return errors.New("computer capture member is not waiting on the exact local grant")
		}
		entries = append(entries, entry)
		requests = append(requests, &MemberPause{ctx: captureCtx, abort: abort, target: target, member: member, ready: make(chan error, 1), finished: make(chan struct{})})
	}
	for _, entry := range r.waits {
		if entry.lease.ComputerInstanceID == target.ID && entry.lease.WorkerEpoch == target.WorkerEpoch {
			found := false
			for _, selected := range entries {
				if selected == entry {
					found = true
					break
				}
			}
			if !found {
				r.mu.Unlock()
				return errors.New("computer capture omits a resident wait")
			}
		}
	}
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return err
	}
	if r.sealed == nil {
		r.sealed = make(map[preparedMachineRef]bool)
	}
	r.sealed[ref] = true
	for i, entry := range entries {
		entry.pause = requests[i]
		entry.requests <- requests[i]
	}
	r.mu.Unlock()
	retErr = errors.New("computer capture interrupted")
	defer func() {
		abort(retErr)
		cleanupCtx := context.WithoutCancel(ctx)
		if err := exclude(cleanupCtx); err != nil {
			retErr = errors.Join(retErr, &SourceReleaseError{Err: err})
		}
		r.mu.Lock()
		if !r.sealed[ref] {
			delete(r.sealed, ref)
		}
		r.mu.Unlock()
		for _, request := range requests {
			request.result = retErr
			close(request.finished)
		}
	}()
	for i, request := range requests {
		select {
		case err := <-request.ready:
			if err != nil {
				return fmt.Errorf("pause computer member: %w", err)
			}
		case <-entries[i].closed:
			return errors.New("computer capture member left its wait")
		case <-captureCtx.Done():
			return context.Cause(captureCtx)
		}
	}
	err := capture(captureCtx)
	return errors.Join(err, context.Cause(captureCtx))
}

// Resumed is valid after Settle and distinguishes a same-source abort from
// successful capture and detachment.
func (p *MemberPause) Resumed() bool { return p.resumed }

func (r *CaptureRuns) prepareAbortMembers(ctx context.Context, target workerapi.RuntimeReconcileTarget, response workerapi.CaptureAbortResponse, restoreRenewal bool) ([]*computerv0.ComputerCaptureAbortMember, error) {
	r.mu.Lock()
	entries := make(map[string]*CaptureWait, len(target.Capture.Runs))
	expected := make(map[string]workerapi.RuntimeCaptureRun, len(target.Capture.Runs))
	for _, member := range target.Capture.Runs {
		entries[member.RunID] = r.waits[member.RunID]
		expected[member.RunID] = member
	}
	r.mu.Unlock()
	result := make([]*computerv0.ComputerCaptureAbortMember, 0, len(response.Members))
	for _, member := range response.Members {
		sealed, ok := expected[member.RunID]
		if !ok || sealed.RunLeaseID != member.Lease.ID || sealed.AttemptNumber != member.AttemptNumber || sealed.RunWaitID != member.RunWaitID {
			return nil, errors.New("capture abort grant differs from sealed member")
		}
		delete(expected, member.RunID)
		m := &computerv0.ComputerCaptureAbortMember{Member: &computerv0.ComputerCaptureRun{RunId: member.RunID, AttemptNumber: uint32(member.AttemptNumber), RunWaitId: member.RunWaitID, RunLeaseId: member.Lease.ID}, Cancelled: member.Cancelled}
		if !member.Cancelled {
			entry := entries[member.RunID]
			if entry == nil || entry.pause == nil || entry.pause.target.Capture.CheckpointID != target.Capture.CheckpointID || entry.lease.ID != member.Lease.ID || entry.resume == nil {
				return nil, errors.New("capture abort member renewal owner is missing")
			}
			var err error
			m.Authority, err = entry.resume(ctx, member, restoreRenewal)
			if err != nil {
				return nil, err
			}
			if m.Authority.GetWriteCapability() != response.WriteCapability || m.Authority.GetFence().GetWorkerHostId() != response.WorkerHostID {
				return nil, errors.New("capture abort source grant differs from Control Plane")
			}
		}
		result = append(result, m)
	}
	if len(expected) != 0 {
		return nil, errors.New("capture abort omitted a member")
	}
	return result, nil
}

func (r *CaptureRuns) markCaptureResumed(target workerapi.RuntimeReconcileTarget, members []workerapi.CaptureAbortMember) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sealed[preparedMachineRef{id: target.ID, epoch: target.WorkerEpoch}] = false
	for _, member := range members {
		if entry := r.waits[member.RunID]; entry != nil && entry.pause != nil && !member.Cancelled {
			entry.pause.resumed = true
		}
	}
}
