package computerhost

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// CaptureRuns joins the resident wait owners with the physical capture
// owner. Its zero value is usable. A capture seals the Instance until its source
// has been excluded; a failed capture is never automatically reopened.
type CaptureRuns struct {
	mu     sync.Mutex
	waits  map[string]*CaptureWait
	sealed map[preparedMachineRef]bool
}

// CaptureWait is one resident wait registered for capture. Its owner receives
// dispatched member pauses from Pauses and must call Detach when the wait
// ends; later calls return the first result.
type CaptureWait struct {
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
	target   workerapi.InstanceReconcileTarget
	member   workerapi.InstanceCaptureRun
	ready    chan error
	finished chan struct{}
	result   error
}

// Context bounds the member's pause work; it ends when the capture is aborted.
func (p *MemberPause) Context() context.Context { return p.ctx }

// Target is the capture operation the member is paused for.
func (p *MemberPause) Target() workerapi.InstanceReconcileTarget { return p.target }

// Member is the exact local grant being paused.
func (p *MemberPause) Member() workerapi.InstanceCaptureRun { return p.member }

// Abort cancels the capture with cause.
func (p *MemberPause) Abort(cause error) { p.abort(cause) }

// Settle reports the member's pause result and then stays joined until the
// physical owner has finished capture and source exclusion, even if the
// caller has since been cancelled. It returns the member's error joined with
// the capture result; nil means the member was captured and released.
func (p *MemberPause) Settle(err error) error {
	p.ready <- err
	<-p.finished
	return errors.Join(err, p.result)
}

// Register records a resident wait for capture. At most one wait per Run may be
// registered, and none on an Instance whose capture has started.
func (r *CaptureRuns) Register(lease workerapi.RunLeaseAssignment, waitID string) (*CaptureWait, error) {
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
	entry := &CaptureWait{runs: r, lease: lease, waitID: waitID, requests: make(chan *MemberPause, 1), closed: make(chan struct{})}
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
func (r *CaptureRuns) capture(ctx context.Context, target workerapi.InstanceReconcileTarget, capture, exclude func(context.Context) error) (retErr error) {
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
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := exclude(cleanupCtx); err != nil {
			retErr = errors.Join(retErr, &SourceReleaseError{Err: err})
		}
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
