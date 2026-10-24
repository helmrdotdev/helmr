package guestd

import "context"

// Relay primitive test setup: seal admission and await the private probe.
// Lifecycle tests use the complete-Computer controller for capture and continuation.
func (relay *agentRelay) prepareCapture(ctx context.Context, checkpointID string) error {
	relay.mu.Lock()
	relay.session.mu.Lock()
	err := relay.canCaptureLocked(checkpointID)
	if err == nil {
		relay.sealCaptureLocked(checkpointID)
	}
	relay.session.mu.Unlock()
	relay.mu.Unlock()
	if err != nil {
		return err
	}
	return relay.awaitCheckpoint(ctx)
}
