package guestd

import (
	"context"
	"errors"
	"sort"
	"time"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

// One record owns both source abort and destination restoration. Retaining the
// receipt after activation fences retries and prevents reuse of an older cut.
type agentComputerCapture struct {
	request           *agentv1.ComputerSessionCapture
	entry             *computerMountEntry
	members           []*agentRelay
	installation      *agentv1.ComputerSessionInstallation
	stopped           map[*agentRelay]bool
	controls          map[*agentRelay]*agentv1.SessionContinuationControl
	controlsDigest    []byte
	frozen            bool
	activationStarted bool
	activated         bool
}

func (r *computerOperationRegistry) captureSealedLocked() bool {
	return (r.agentCapture != nil && !r.agentCapture.activated)
}

var errComputerCaptureUnusable = errors.New("retained Computer capture cannot continue")

var errComputerAuthorityExpired = errors.New("computer continuation authority expired")

func validateAgentComputerEnvelope(envelope *computerv0.ComputerOperationEnvelope, now time.Time) error {
	if envelope == nil || envelope.GetOperationId() == "" || envelope.GetComputerId() == "" || envelope.GetComputerInstanceId() == "" || envelope.GetWriterGeneration() == 0 || envelope.GetWriterGeneration() > uint64(^uint64(0)>>1) || envelope.GetChannelCredential() == "" {
		return errors.New("computer continuation authority is incomplete")
	}
	if envelope.GetOperationExpiresAtUnixNano() <= now.UnixNano() {
		return errComputerAuthorityExpired
	}
	return nil
}

func (r *computerOperationRegistry) captureAgentComputer(ctx context.Context, request *agentv1.ComputerSessionCapture) (*agentv1.ComputerSessionReceipt, error) {
	r.agentCaptureMu.Lock()
	defer r.agentCaptureMu.Unlock()
	if request == nil || request.GetCheckpointId() == "" || len(request.GetCheckpointId()) > 128 || request.GetDesiredVersion() <= 0 || request.GetMembershipRevision() < 0 {
		return nil, errors.New("computer capture identity is incomplete")
	}
	envelope := request.GetEnvelope()
	entry, release, ok := r.acquireExact(envelope.GetComputerInstanceId(), envelope.GetComputerId(), envelope.GetChannelCredential(), envelope.GetWriterGeneration())
	if !ok {
		return nil, errors.New("computer capture does not own the mounted filesystem")
	}
	defer release()
	if err := validateAgentComputerEnvelope(request.GetEnvelope(), entry.authorityNow()); err != nil {
		if retained := r.agentCapture; retained != nil && proto.Equal(retained.request, request) {
			return retained.receipt(), err
		}
		return nil, err
	}
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	capture, err := r.sealAgentComputerCapture(entry, request)
	if err != nil {
		return nil, err
	}
	if capture.installation != nil || capture.activationStarted {
		return capture.receipt(), errors.New("computer capture has already entered continuation")
	}
	if !capture.frozen {
		for _, relay := range capture.members {
			if err := relay.awaitCheckpoint(ctx); err != nil {
				return capture.receipt(), err
			}
		}
		for _, relay := range capture.members {
			if err := relay.freezeForCapture(ctx); err != nil {
				return capture.receipt(), err
			}
		}
		if err := r.writeback.flush(ctx); err != nil {
			return capture.receipt(), err
		}
		capture.frozen = true
	}
	if err := r.verifyAgentComputerCaptureLocked(capture); err != nil {
		return capture.receipt(), err
	}
	return capture.receipt(), nil
}

func (r *computerOperationRegistry) sealAgentComputerCapture(entry *computerMountEntry, request *agentv1.ComputerSessionCapture) (*agentComputerCapture, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if previous := r.agentCapture; previous != nil {
		if proto.Equal(previous.request, request) {
			return previous, nil
		}
		if !previous.activated || request.GetDesiredVersion() <= previous.installation.GetDesiredVersion() || request.GetCheckpointId() == previous.request.GetCheckpointId() {
			return nil, errors.New("computer capture does not follow its previous continuation")
		}
	}
	if r.materializations != 0 || len(r.agentStarting) != 0 || r.preparedRuntime != nil || len(r.entries) != 1 || r.entries[entry.computerInstanceID] != entry || entry.retired || entry.stopping {
		return nil, errors.New("computer cannot seal its complete process set")
	}
	entry.processesMu.Lock()
	busy := entry.processAdmissions != 0 || entry.recoveryRequired
	entry.processesMu.Unlock()
	if busy || entry.hasUnreleasedCommands() {
		return nil, errors.New("computer has unfinished commands or recovery")
	}
	expected := make(map[string]*agentv1.SessionIdentity, len(request.GetSessions()))
	for _, identity := range request.GetSessions() {
		if identity.GetSessionId() == "" || identity.GetProcessEpoch() <= 0 || expected[identity.GetSessionId()] != nil {
			return nil, errors.New("computer capture contains incomplete or duplicate Session identity")
		}
		expected[identity.GetSessionId()] = identity
	}
	all := make([]*agentRelay, 0, len(r.agentSessions))
	ids := make([]string, 0, len(r.agentSessions))
	for id := range r.agentSessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		all = append(all, r.agentSessions[id])
	}
	unlock, locked := tryLockAgentCaptureMembers(all)
	if !locked {
		return nil, errors.New("computer Session transport is busy")
	}
	defer unlock()
	members := make([]*agentRelay, 0, len(expected))
	var owner *agentv1.SessionGrant
	for _, relay := range all {
		session := relay.session
		if session.physicalClosed {
			select {
			case <-relay.finished:
			default:
				return nil, errors.New("terminal Session cleanup has not finished")
			}
			if len(relay.outbox) != 0 || relay.writeBytes.Load() != 0 {
				return nil, errors.New("terminal Session receipts are not drained")
			}
			continue
		}
		identity := expected[session.grant.GetIdentity().GetSessionId()]
		if session.entry != entry || identity == nil || !proto.Equal(identity, session.grant.GetIdentity()) {
			return nil, errors.New("computer capture membership differs from resident Sessions")
		}
		if owner != nil && (session.grant.GetWorkerHostId() != owner.GetWorkerHostId() || session.grant.GetComputerLeaseEpoch() != owner.GetComputerLeaseEpoch()) {
			return nil, errors.New("resident Sessions have inconsistent Computer ownership")
		}
		owner = session.grant
		if err := relay.canCaptureLocked(request.GetCheckpointId()); err != nil {
			return nil, err
		}
		delete(expected, identity.GetSessionId())
		members = append(members, relay)
	}
	if len(expected) != 0 {
		return nil, errors.New("computer capture contains an absent Session")
	}
	capture := &agentComputerCapture{request: proto.Clone(request).(*agentv1.ComputerSessionCapture), entry: entry, members: members}
	r.agentCapture = capture
	// No member can accept a host command between validation and sealing peers.
	for _, relay := range members {
		relay.sealCaptureLocked(request.GetCheckpointId())
	}
	return capture, nil
}

// Lock order is registry, all relay locks, then all Session locks. The ordered
// member slice is retained through installation and activation.
func lockAgentCaptureMembers(members []*agentRelay) func() {
	for _, relay := range members {
		relay.mu.Lock()
	}
	for _, relay := range members {
		relay.session.mu.Lock()
	}
	return func() {
		for i := len(members) - 1; i >= 0; i-- {
			members[i].session.mu.Unlock()
		}
		for i := len(members) - 1; i >= 0; i-- {
			members[i].mu.Unlock()
		}
	}
}

// Caller serializes continuation operations and holds the entry lifecycle locks.
// This is a verification, never a new freeze of potentially resumed execution.
func (r *computerOperationRegistry) verifyAgentComputerCaptureLocked(capture *agentComputerCapture) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.agentCapture != capture || !capture.frozen || capture.activationStarted || capture.activated || capture.entry.retired || capture.entry.stopping || r.entries[capture.entry.computerInstanceID] != capture.entry || len(r.agentStarting) != 0 {
		return errors.Join(errComputerCaptureUnusable, errors.New("computer has no reusable frozen capture"))
	}
	unlock := lockAgentCaptureMembers(capture.members)
	defer unlock()
	for _, relay := range capture.members {
		session := relay.session
		if session.terminal || session.checkpointID != capture.request.GetCheckpointId() || !relay.captureFrozen || !relay.captureVerified || relay.writeBytes.Load() != 0 {
			return errors.Join(errComputerCaptureUnusable, errors.New("computer capture member is not frozen"))
		}
		if err := session.process.verifyFrozen(); err != nil {
			return err
		}
		var scopes []nativeScopeEvidence
		if err := decodeAgentJSON(session.checkpointReady.GetScopesJson(), &scopes); err != nil {
			return err
		}
		if err := session.process.converge(scopes); err != nil {
			return err
		}
	}
	return nil
}

func (capture *agentComputerCapture) receipt() *agentv1.ComputerSessionReceipt {
	version := capture.request.GetDesiredVersion()
	if capture.installation != nil {
		version = capture.installation.GetDesiredVersion()
	}
	return &agentv1.ComputerSessionReceipt{CheckpointId: capture.request.GetCheckpointId(), DesiredVersion: version, Frozen: capture.frozen, Installed: capture.installation != nil, ActivationStarted: capture.activationStarted, Activated: capture.activated, ControlsDigest: append([]byte(nil), capture.controlsDigest...)}
}

// Capture is opportunistic: a blocked host write or physical transition must not
// stall the Computer registry merely to discover that a member is busy.
func tryLockAgentCaptureMembers(members []*agentRelay) (func(), bool) {
	relays, sessions := 0, 0
	unlock := func() {
		for i := sessions - 1; i >= 0; i-- {
			members[i].session.mu.Unlock()
		}
		for i := relays - 1; i >= 0; i-- {
			members[i].mu.Unlock()
		}
	}
	for _, relay := range members {
		if !relay.mu.TryLock() {
			unlock()
			return nil, false
		}
		relays++
	}
	for _, relay := range members {
		if !relay.session.mu.TryLock() {
			unlock()
			return nil, false
		}
		sessions++
	}
	return unlock, true
}
