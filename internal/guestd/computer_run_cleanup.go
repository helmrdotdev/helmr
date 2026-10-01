package guestd

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
)

func (r *computerOperationRegistry) bindProgramStop(entry *computerMountEntry, a *computerv0.ComputerRunAuthority, stop context.CancelFunc) *managedProgramClaim {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, claim := range r.programClaims {
		if claim.entry == entry && claim.authority.GetFence().GetRunLeaseId() == a.GetFence().GetRunLeaseId() {
			claim.stop = stop
			if claim.stopRequested {
				stop()
			}
			return claim
		}
	}
	return nil
}

func (r *computerOperationRegistry) cleanupProgram(ctx context.Context, request *computerv0.ComputerRunCleanupRequest) error {
	if request.GetRunId() == "" || request.GetRunLeaseId() == "" || request.GetAttemptNumber() == 0 || request.GetWriterGeneration() <= 0 {
		return errors.New("program cleanup identity is incomplete")
	}
	entry, release, ok := r.acquireCommandInstance(request.GetComputerInstanceId(), request.GetComputerId(), request.GetChannelCredential())
	if !ok {
		return errors.New("program cleanup Instance is unavailable")
	}
	defer release()
	entry.lifecycleMu.Lock()
	entry.finalizationMu.Lock()
	r.mu.Lock()
	if r.entries[request.GetComputerInstanceId()] != entry || entry.writerGeneration != request.GetWriterGeneration() || r.captureRequest != nil {
		r.mu.Unlock()
		entry.finalizationMu.Unlock()
		entry.lifecycleMu.Unlock()
		return errors.New("program cleanup physical authority changed")
	}
	claim := entry.programCleanup[request.GetRunLeaseId()]
	if claim == nil {
		for _, candidate := range r.programClaims {
			if candidate.entry == entry && candidate.authority.GetFence().GetRunLeaseId() == request.GetRunLeaseId() {
				claim = candidate
				break
			}
		}
	}
	if claim != nil {
		f := claim.authority.GetFence()
		if f.GetRunId() != request.GetRunId() || f.GetAttemptNumber() != request.GetAttemptNumber() {
			r.mu.Unlock()
			entry.finalizationMu.Unlock()
			entry.lifecycleMu.Unlock()
			return errors.New("program cleanup member identity changed")
		}
		claim.stopRequested = true
		if claim.stop != nil {
			claim.stop()
		}
	} else {
		// Install a tombstone under admission's locks: a delayed launch cannot follow
		// a successful cleanup receipt for a lease that never reached this Guest.
		claim = &managedProgramClaim{entry: entry, authority: &computerv0.ComputerRunAuthority{Fence: &computerv0.ComputerAuthorityFence{RunId: request.GetRunId(), RunLeaseId: request.GetRunLeaseId(), AttemptNumber: request.GetAttemptNumber()}}, done: make(chan struct{})}
		close(claim.done)
		if entry.programCleanup == nil {
			entry.programCleanup = make(map[string]*managedProgramClaim)
		}
		entry.programCleanup[request.GetRunLeaseId()] = claim
	}
	r.mu.Unlock()
	entry.finalizationMu.Unlock()
	entry.lifecycleMu.Unlock()
	select {
	case <-claim.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return claim.cleanupErr
}

func handleComputerRunCleanupConnection(ctx context.Context, conn io.ReadWriter, r *computerOperationRegistry) error {
	var request computerv0.ComputerRunCleanupRequest
	if err := frameio.ReadProtoFrame(conn, &request); err != nil {
		return err
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	response := &computerv0.ComputerRunCleanupResponse{}
	if err := r.cleanupProgram(cleanupCtx, &request); err != nil {
		response.Error = err.Error()
	} else {
		response.Reconciled = true
	}
	return frameio.WriteProtoFrame(conn, response)
}
