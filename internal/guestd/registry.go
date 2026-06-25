package guestd

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

var resumeAttachTimeout = 30 * time.Second

type waitingRunRegistry struct {
	mu    sync.Mutex
	slots map[string]*waitingRunSlot
}

type waitingRunSlot struct {
	checkpointID string
	attached     chan io.ReadWriter
}

type waitingRunRegistration struct {
	registry  *waitingRunRegistry
	runWaitID string
	slot      *waitingRunSlot
}

func newWaitingRunRegistry() *waitingRunRegistry {
	return &waitingRunRegistry{slots: map[string]*waitingRunSlot{}}
}

func (r *waitingRunRegistry) register(runWaitID string, checkpointID string) waitingRunRegistration {
	slot := &waitingRunSlot{
		checkpointID: checkpointID,
		attached:     make(chan io.ReadWriter, 1),
	}
	r.mu.Lock()
	r.slots[runWaitID] = slot
	r.mu.Unlock()
	return waitingRunRegistration{registry: r, runWaitID: runWaitID, slot: slot}
}

func (r *waitingRunRegistry) attach(runWaitID string, checkpointID string, stream io.ReadWriter) error {
	r.mu.Lock()
	slot := r.slots[runWaitID]
	r.mu.Unlock()
	if slot == nil {
		return fmt.Errorf("no waiting run slot matched run wait %s checkpoint %s", runWaitID, checkpointID)
	}
	if slot.checkpointID != checkpointID {
		return fmt.Errorf("resume attach checkpoint %s did not match expected %s", checkpointID, slot.checkpointID)
	}
	select {
	case slot.attached <- stream:
		return nil
	default:
		return fmt.Errorf("run wait %s already has an attached resume stream", runWaitID)
	}
}

func (r waitingRunRegistration) wait(ctx context.Context) (io.ReadWriter, error) {
	select {
	case stream := <-r.slot.attached:
		return stream, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r waitingRunRegistration) unregister() {
	r.registry.mu.Lock()
	if r.registry.slots[r.runWaitID] == r.slot {
		delete(r.registry.slots, r.runWaitID)
	}
	r.registry.mu.Unlock()
}
