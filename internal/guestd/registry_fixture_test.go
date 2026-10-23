package guestd

import (
	"context"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
)

// Test-only registry, exec and cgroup helpers that production code does not call.

func (r *computerOperationRegistry) retire(computerInstanceID string, entry *computerMountEntry) {
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	r.retireLocked(computerInstanceID, entry)
}

// Caller holds the entry lifecycle/finalization locks.
func (r *computerOperationRegistry) retireLocked(computerInstanceID string, entry *computerMountEntry) {
	r.mu.Lock()
	current := r.entries[computerInstanceID]
	if current != entry {
		r.mu.Unlock()
		return
	}
	delete(r.entries, computerInstanceID)
	entry.retired = true
	var cleanup func()
	if entry.active == 0 {
		cleanup = entry.cleanup
		entry.cleanup = nil
	}
	r.mu.Unlock()
	if cleanup != nil {
		cleanup()
	}
}

func (r *computerOperationRegistry) runComputerBasicExec(ctx context.Context, entry *computerMountEntry, request *computerv0.ComputerBasicExecRequest) *computerv0.ComputerBasicExecResult {
	execution, code, err := r.startComputerBasicExec(ctx, entry, request)
	if err != nil {
		return computerBasicCommandFailure(request.GetEnvelope().GetRequestFingerprint(), code, err)
	}
	select {
	case <-execution.done:
		return execution.result
	case <-ctx.Done():
		return computerBasicCommandFailure(request.GetEnvelope().GetRequestFingerprint(), "computer_command_result_uncertain", ctx.Err())
	}
}
