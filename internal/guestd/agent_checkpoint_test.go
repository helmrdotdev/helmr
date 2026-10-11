package guestd

import (
	"context"
	"errors"
)

// Per-process test harness only. Production continuation always uses the
// complete-set Computer controller and fresh installation boundary.
// prepareCapture seals admission before sending a private checkpoint probe. A
// failed probe retains the seal until explicit abort; it never releases a hold.
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

func (relay *agentRelay) activateAfterCapture(ctx context.Context) error {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	if !relay.captureFrozen || !relay.captureVerified {
		return errors.New("Session has no retained capture")
	}
	relay.session.mu.Lock()
	err := relay.session.authorizeLocked()
	relay.session.mu.Unlock()
	if err != nil {
		return err
	}
	if relay.connection == nil {
		return errors.New("Session upstream must be rebound before thaw")
	}
	if err := relay.session.process.thaw(ctx); err != nil {
		return err
	}
	relay.session.mu.Lock()
	relay.session.checkpointID = ""
	relay.session.checkpointReady = nil
	relay.session.mu.Unlock()
	relay.captureFrozen = false
	relay.captureVerified = false
	relay.expiryFrozen = false
	relay.notifyLocked()
	return nil
}
