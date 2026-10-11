package guestd

import (
	"context"
	"errors"
	"fmt"
	"time"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

func (r *computerOperationRegistry) installAgentComputer(ctx context.Context, request *agentv1.ComputerSessionInstallation, activate bool, clock *computerAuthorityClock) (*agentv1.ComputerSessionReceipt, error) {
	r.agentCaptureMu.Lock()
	defer r.agentCaptureMu.Unlock()
	if request == nil || request.GetCapture() == nil || request.GetDesiredVersion() <= request.GetCapture().GetDesiredVersion() || request.GetBaseComputerDiskVersionId() == "" {
		return nil, errors.New("computer continuation installation is incomplete")
	}
	r.mu.RLock()
	capture := r.agentCapture
	r.mu.RUnlock()
	if capture == nil && !request.GetSourceAbort() {
		return nil, errComputerCaptureUnusable
	}
	if capture == nil || !proto.Equal(capture.request, request.GetCapture()) {
		return nil, errors.New("computer continuation does not match the retained capture")
	}
	entry := capture.entry
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	if capture.installation != nil && !proto.Equal(capture.installation, request) {
		return nil, errors.New("computer continuation conflicts with its installation receipt")
	}
	if capture.activated {
		return capture.receipt(), nil
	}
	if !request.GetSourceAbort() && !capture.activationStarted {
		if err := r.verifyAgentComputerCaptureLocked(capture); err != nil {
			return nil, err
		}
	}
	if capture.installation == nil {
		if clock == nil {
			return nil, errors.New("computer installation requires a fresh authority clock challenge")
		}
		observedClock := clock
		clock = clock.notBefore(entry.authorityClock.Load())
		if err := validateAgentComputerEnvelope(request.GetEnvelope(), clock.now()); err != nil {
			return capture.receipt(), err
		}
		if activate {
			return nil, errors.New("computer continuation grants are not installed")
		}
		if err := r.installAgentComputerGrants(capture, request, clock, observedClock); err != nil {
			return capture.receipt(), err
		}
	}
	if activate {
		if err := r.activateAgentComputer(ctx, capture); err != nil {
			return capture.receipt(), err
		}
	}
	return capture.receipt(), nil
}

func (r *computerOperationRegistry) installAgentComputerGrants(capture *agentComputerCapture, request *agentv1.ComputerSessionInstallation, clock, observedClock *computerAuthorityClock) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, target := capture.entry, request.GetEnvelope()
	if r.agentCapture != capture || r.entries[entry.computerInstanceID] != entry || entry.retired || entry.stopping || len(r.agentStarting) != 0 || len(request.GetGrants()) != len(capture.members) {
		return errors.New("computer continuation membership or filesystem changed")
	}
	entry.processesMu.Lock()
	unavailable := entry.recoveryRequired || entry.processAdmissions != 0
	entry.processesMu.Unlock()
	if unavailable || entry.hasUnreleasedCommands() {
		return errors.New("computer continuation has unresolved physical work")
	}
	source := capture.request.GetEnvelope()
	if target.GetComputerId() != source.GetComputerId() {
		return errors.New("computer continuation changed logical identity")
	}
	if request.GetSourceAbort() {
		if target.GetComputerInstanceId() != source.GetComputerInstanceId() || target.GetWriterGeneration() != source.GetWriterGeneration() || target.GetChannelCredential() != source.GetChannelCredential() || request.GetBaseComputerDiskVersionId() != entry.baseComputerDiskVersionID {
			return errors.New("source abort changed its retained filesystem or physical owner")
		}
	} else if target.GetComputerInstanceId() == source.GetComputerInstanceId() || target.GetWriterGeneration() <= source.GetWriterGeneration() || target.GetChannelCredential() == source.GetChannelCredential() {
		return errors.New("restore does not advance physical Computer ownership")
	}
	candidate := &computerMountEntry{computerID: target.GetComputerId(), computerInstanceID: target.GetComputerInstanceId(), channelCredential: target.GetChannelCredential(), writerGeneration: int64(target.GetWriterGeneration())}
	grants := make(map[string]*agentv1.SessionGrant, len(request.GetGrants()))
	var owner *agentv1.SessionGrant
	for _, grant := range request.GetGrants() {
		if err := validateSessionGrant(candidate, grant, clock.now()); err != nil {
			return err
		}
		if owner != nil && (grant.GetWorkerHostId() != owner.GetWorkerHostId() || grant.GetComputerLeaseEpoch() != owner.GetComputerLeaseEpoch()) {
			return errors.New("continuation grants disagree on the Computer owner")
		}
		owner = grant
		id := grant.GetIdentity().GetSessionId()
		if grants[id] != nil {
			return errors.New("computer continuation duplicates a Session grant")
		}
		grants[id] = grant
	}
	stopped := make(map[string]*agentv1.SessionIdentity, len(request.GetStoppedSessions()))
	for _, identity := range request.GetStoppedSessions() {
		id := identity.GetSessionId()
		if id == "" || stopped[id] != nil || !proto.Equal(identity, grants[id].GetIdentity()) {
			return errors.New("computer continuation stop does not name an exact captured member")
		}
		stopped[id] = identity
	}
	// Terminal relays also read the shared entry while delivering their tail.
	all := make([]*agentRelay, 0, len(r.agentSessions))
	for _, relay := range r.agentSessions {
		all = append(all, relay)
	}
	unlock := lockAgentCaptureMembers(all)
	defer unlock()
	for _, relay := range capture.members {
		session := relay.session
		old := session.grant
		next := grants[old.GetIdentity().GetSessionId()]
		if session.terminal || session.checkpointID != capture.request.GetCheckpointId() || !proto.Equal(old.GetIdentity(), next.GetIdentity()) || next.GetAuthorityGeneration() < old.GetAuthorityGeneration() {
			return errors.New("computer continuation changed or lost a captured Session")
		}
		if request.GetSourceAbort() {
			if !sameSessionGrantOwner(old, next) || next.GetExpiresAtUnixNano() < old.GetExpiresAtUnixNano() {
				return errors.New("source abort regressed or moved Session authority")
			}
		} else if next.GetComputerLeaseEpoch() <= old.GetComputerLeaseEpoch() {
			return errors.New("restore did not advance the Session Computer lease")
		}
	}
	// Validation precedes all changes. Holding every relay and Session lock keeps
	// expiry checks, native operations and reattachment outside a partial install.
	// A restored kernel retains its old wall clock. Synchronize it before any
	// workload resumes so TLS and application timestamps use current host time.
	// Lease deadlines keep their forward-only authority clock, but wall time must
	// use the fresh observation even when the host has corrected its clock back.
	if r.setWallClock == nil {
		return errors.New("guest wall clock synchronization is unavailable")
	}
	if err := r.setWallClock(observedClock.now()); err != nil {
		return fmt.Errorf("synchronize guest wall clock: %w", err)
	}
	entry.authorityClock.Store(clock)
	delete(r.entries, entry.computerInstanceID)
	entry.computerInstanceID = target.GetComputerInstanceId()
	entry.channelCredential = target.GetChannelCredential()
	entry.baseComputerDiskVersionID = request.GetBaseComputerDiskVersionId()
	entry.setWriterGeneration(target.GetWriterGeneration())
	r.entries[entry.computerInstanceID] = entry
	capture.stopped = make(map[*agentRelay]bool, len(stopped))
	for _, relay := range capture.members {
		capture.stopped[relay] = stopped[relay.session.grant.GetIdentity().GetSessionId()] != nil
		relay.session.grant = proto.Clone(grants[relay.session.grant.GetIdentity().GetSessionId()]).(*agentv1.SessionGrant)
		if relay.connection != nil {
			_ = relay.connection.Close()
			relay.connection = nil
		}
		relay.notifyLocked()
	}
	capture.installation = proto.Clone(request).(*agentv1.ComputerSessionInstallation)
	return nil
}

func (r *computerOperationRegistry) activateAgentComputer(ctx context.Context, capture *agentComputerCapture) error {
	validate := func(requireStopped bool) error {
		if len(capture.controls) != len(capture.members) {
			return errors.New("current Computer controls must precede activation")
		}
		for _, relay := range capture.members {
			if control := capture.controls[relay]; control == nil || control.GetAuthorityGeneration() != relay.session.grant.GetAuthorityGeneration() {
				return errors.New("session authority changed after continuation controls")
			}
			if capture.stopped[relay] {
				if requireStopped && !relay.session.physicalClosed {
					return errors.New("cancelled captured Session has not physically stopped")
				}
				continue
			}
			if err := relay.session.authorizeLocked(); err != nil {
				return err
			}
			if relay.connection == nil {
				return errors.New("all Session upstream connections must be installed before Computer activation")
			}
			if !capture.activationStarted && !capture.installation.GetSourceAbort() && (!relay.captureFrozen || !relay.captureVerified) {
				return errors.New("restored Session has no verified freeze")
			}
		}
		return nil
	}
	r.mu.Lock()
	unlock := lockAgentCaptureMembers(capture.members)
	if r.agentCapture != capture || capture.installation == nil || capture.activated {
		unlock()
		r.mu.Unlock()
		return errors.New("computer continuation is not ready to activate")
	}
	if err := validate(false); err != nil {
		unlock()
		r.mu.Unlock()
		return err
	}
	// This latch survives failed activation. A retry operates on these current
	// processes; it never authorizes reloading the old snapshot.
	capture.activationStarted = true
	capture.frozen = false
	unlock()
	r.mu.Unlock()
	activationErr := stopAgentComputerMembers(capture)
	// Stopping several members shares one bounded cleanup interval. Compute
	// surviving authority headroom afterwards; expired grants require renewal
	// and a retry on these same physical processes.
	deadline := time.Now().Add(10 * time.Second)
	for _, relay := range capture.members {
		if capture.stopped[relay] {
			continue
		}
		relay.session.mu.Lock()
		expires := time.Now().Add(time.Unix(0, relay.session.grant.GetExpiresAtUnixNano()).Sub(relay.session.clock()))
		relay.session.mu.Unlock()
		if expires.Before(deadline) {
			deadline = expires
		}
	}
	thawCtx, cancelThaw := context.WithDeadline(ctx, deadline)
	defer cancelThaw()
	for _, relay := range capture.members {
		if activationErr != nil {
			break
		}
		if capture.stopped[relay] {
			continue
		}
		// Only the transitioning member's lock is held. Peers retain independent
		// expiry enforcement while this cgroup operation waits for the kernel.
		relay.mu.Lock()
		relay.session.mu.Lock()
		err := relay.session.authorizeLocked()
		if err == nil && capture.controls[relay].GetAuthorityGeneration() != relay.session.grant.GetAuthorityGeneration() {
			err = errors.New("session authority changed after continuation controls")
		}
		relay.session.mu.Unlock()
		if err == nil && relay.connection == nil {
			err = errors.New("session upstream disconnected before thaw")
		}
		if err == nil {
			// A failed reply may still have enabled execution. Neither kind of
			// remembered freeze may suppress the watchdog after that attempt.
			relay.captureFrozen, relay.captureVerified, relay.expiryFrozen = false, false, false
			err = relay.session.process.thaw(thawCtx)
		}
		if err == nil {
			relay.captureFrozen, relay.captureVerified, relay.expiryFrozen = false, false, false
		}
		relay.notifyLocked()
		relay.mu.Unlock()
		if err != nil {
			activationErr = err
			break
		}
	}
	if activationErr == nil {
		r.mu.Lock()
		unlock = lockAgentCaptureMembers(capture.members)
		activationErr = validate(true)
		if activationErr == nil {
			for _, relay := range capture.members {
				if relay.expiryFrozen {
					activationErr = errors.New("session lease expired during Computer activation")
					break
				}
			}
		}
		if activationErr == nil {
			for _, relay := range capture.members {
				relay.session.checkpointID = ""
				relay.session.checkpointReady = nil
				relay.notifyLocked()
			}
			capture.frozen = false
			capture.activated = true
		}
		unlock()
		r.mu.Unlock()
	}
	if activationErr == nil {
		return nil
	}
	results := make(chan error, len(capture.members))
	for _, member := range capture.members {
		go func() {
			if capture.stopped[member] {
				results <- nil
				return
			}
			member.mu.Lock()
			defer member.mu.Unlock()
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := member.session.process.freeze(stopCtx)
			member.captureFrozen = err == nil
			if err != nil {
				member.expiryFrozen = false
			}
			member.captureVerified = false
			// A failed refreeze must wake a formerly parked expiry watchdog.
			member.notifyLocked()
			results <- err
		}()
	}
	failures := []error{activationErr}
	for range capture.members {
		failures = append(failures, <-results)
	}
	return errors.Join(failures...)
}

// The irreversible activation latch precedes any stop. Exact cancelled members
// retain their normal stopped receipts; failure to prove closure prevents peer
// execution. finish's retained result makes both success and failure repeatable.
func stopAgentComputerMembers(capture *agentComputerCapture) error {
	results := make(chan error, len(capture.stopped))
	count := 0
	for _, relay := range capture.members {
		if !capture.stopped[relay] {
			continue
		}
		count++
		go func() {
			relay.mu.Lock()
			relay.shuttingDown = true
			relay.mu.Unlock()
			relay.finish(nil)
			<-relay.finished
			relay.session.mu.Lock()
			closed := relay.session.physicalClosed
			relay.session.mu.Unlock()
			if !closed {
				results <- errors.New("cancelled captured Session could not be physically stopped")
				return
			}
			results <- nil
		}()
	}
	var failures []error
	for range count {
		failures = append(failures, <-results)
	}
	return errors.Join(failures...)
}

var errComputerCaptureAdmissionPending = errors.New("computer capture absent while admission remains open")

var errComputerCaptureAbsentAfterExpiry = errors.New("computer capture absent after guest admission expiry")

// Inspection is side-effect free and survives expiry of the original operation.
// The owned host transport still gates access to this retained receipt.
func (r *computerOperationRegistry) inspectAgentComputer(request *agentv1.ComputerSessionCapture) (*agentv1.ComputerSessionReceipt, error) {
	r.agentCaptureMu.Lock()
	defer r.agentCaptureMu.Unlock()
	if r.agentCapture == nil || !proto.Equal(r.agentCapture.request, request) {
		envelope := request.GetEnvelope()
		entry, release, ok := r.acquireExact(envelope.GetComputerInstanceId(), envelope.GetComputerId(), envelope.GetChannelCredential(), envelope.GetWriterGeneration())
		if !ok {
			return nil, errors.New("computer capture inspection does not own the mounted filesystem")
		}
		defer release()
		// The capture admission clock, not the host or database clock, must
		// exclude a delayed request before absence can cancel its reservation.
		err := validateAgentComputerEnvelope(envelope, entry.authorityNow())
		if errors.Is(err, errComputerAuthorityExpired) {
			return nil, errComputerCaptureAbsentAfterExpiry
		}
		if err != nil {
			return nil, err
		}
		return nil, errComputerCaptureAdmissionPending
	}
	return r.agentCapture.receipt(), nil
}
