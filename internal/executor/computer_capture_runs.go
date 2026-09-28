package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// ComputerCaptureRuns joins the resident wait owners with the physical capture
// owner. Its zero value is usable. A capture seals the Instance until its source
// has been excluded; a failed capture is never automatically reopened.
type ComputerCaptureRuns struct {
	mu     sync.Mutex
	waits  map[string]*computerCaptureWait
	sealed map[preparedRuntimeRef]bool
}

type computerCaptureWait struct {
	lease    workerapi.RunLeaseAssignment
	waitID   string
	requests chan *computerMemberPause
	closed   chan struct{}
	pause    *computerMemberPause
}

type computerMemberPause struct {
	ctx      context.Context
	abort    context.CancelCauseFunc
	target   workerapi.RuntimeReconcileTarget
	member   workerapi.RuntimeCaptureRun
	ready    chan error
	finished chan struct{}
	result   error
}

func (r *ComputerCaptureRuns) register(lease workerapi.RunLeaseAssignment, waitID string) (*computerCaptureWait, func() error, error) {
	if lease.ID == "" || lease.RunID == "" || lease.ComputerInstanceID == "" || lease.WorkerEpoch <= 0 || waitID == "" {
		return nil, nil, errors.New("capture wait identity is incomplete")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	ref := preparedRuntimeRef{id: lease.ComputerInstanceID, epoch: lease.WorkerEpoch}
	if r.sealed[ref] || r.waits[lease.RunID] != nil {
		return nil, nil, errors.New("capture wait is sealed or already registered")
	}
	if r.waits == nil {
		r.waits = make(map[string]*computerCaptureWait)
	}
	entry := &computerCaptureWait{lease: lease, waitID: waitID, requests: make(chan *computerMemberPause, 1), closed: make(chan struct{})}
	r.waits[lease.RunID] = entry
	var once sync.Once
	var detachErr error
	return entry, func() error {
		once.Do(func() {
			r.mu.Lock()
			pause := entry.pause
			if pause == nil {
				if r.waits[lease.RunID] == entry {
					delete(r.waits, lease.RunID)
				}
				close(entry.closed)
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
			detachErr = pause.result
			r.mu.Lock()
			if r.waits[lease.RunID] == entry {
				delete(r.waits, lease.RunID)
			}
			close(entry.closed)
			r.mu.Unlock()
		})
		return detachErr
	}, nil
}

// Capture waits for every member's pause proof before invoking capture. That
// callback owns whole-Computer verification and publication. The exclusion
// callback must prove source shutdown and runs after any dispatched pause, on
// success or failure. Returning nil releases members as detached, never
// as successfully completed Runs. Errors remain errors for every paused member.
func (r *ComputerCaptureRuns) Capture(ctx context.Context, target workerapi.RuntimeReconcileTarget, capture, exclude func(context.Context) error) (retErr error) {
	if _, err := computerFreezeRequest(target); err != nil {
		return err
	}
	if capture == nil || exclude == nil {
		return errors.New("physical capture and exclusion owners are required")
	}
	captureCtx, abort := context.WithCancelCause(ctx)
	defer abort(nil)
	r.mu.Lock()
	ref := preparedRuntimeRef{id: target.ID, epoch: target.WorkerEpoch}
	if r.sealed[ref] {
		r.mu.Unlock()
		return errors.New("computer capture already has an owner")
	}
	requests := make([]*computerMemberPause, 0, len(target.Capture.Runs))
	entries := make([]*computerCaptureWait, 0, len(target.Capture.Runs))
	for _, member := range target.Capture.Runs {
		entry := r.waits[member.RunID]
		if entry == nil || entry.lease.ID != member.RunLeaseID || entry.lease.AttemptNumber != member.AttemptNumber || entry.waitID != member.RunWaitID || entry.lease.ComputerInstanceID != target.ID || entry.lease.WorkerEpoch != target.WorkerEpoch || entry.lease.ComputerID != target.Source.ComputerID || entry.lease.WriterGeneration != target.Source.WriterGeneration {
			r.mu.Unlock()
			return errors.New("computer capture member is not waiting on the exact local grant")
		}
		entries = append(entries, entry)
		requests = append(requests, &computerMemberPause{ctx: captureCtx, abort: abort, target: target, member: member, ready: make(chan error, 1), finished: make(chan struct{})})
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
		r.sealed = make(map[preparedRuntimeRef]bool)
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
			retErr = errors.Join(retErr, &checkpointSourceReleaseError{err: err})
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
