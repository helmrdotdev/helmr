package guestd

import (
	"crypto/sha256"
	"errors"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

// applyAgentComputerControls updates supervisor authority while customer command
// admission is still sealed. Terminal members are stopped by activation before
// any surviving peer thaws; ordinary delivery acknowledgements follow activation.
func (r *computerOperationRegistry) applyAgentComputerControls(request *agentv1.ComputerSessionControls) (*agentv1.ComputerSessionReceipt, error) {
	if request == nil || proto.Size(request) > maxAgentFrameBytes {
		return nil, errors.New("computer controls are absent or oversized")
	}
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(request)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	r.agentCaptureMu.Lock()
	defer r.agentCaptureMu.Unlock()
	r.mu.RLock()
	capture := r.agentCapture
	r.mu.RUnlock()
	if capture == nil {
		return nil, errors.New("computer controls have no retained capture")
	}
	entry := capture.entry
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	unlock := lockAgentCaptureMembers(capture.members)
	defer unlock()
	if r.agentCapture != capture || capture.installation == nil || capture.activated || entry.retired || entry.stopping {
		return nil, errors.New("computer controls require the installed current attempt before activation")
	}
	envelope := request.GetEnvelope()
	installed := capture.installation.GetEnvelope()
	if request.GetCheckpointId() != capture.request.GetCheckpointId() || request.GetDesiredVersion() != capture.installation.GetDesiredVersion() || envelope.GetOperationId() != installed.GetOperationId() || envelope.GetComputerId() != installed.GetComputerId() || envelope.GetComputerInstanceId() != installed.GetComputerInstanceId() || envelope.GetWriterGeneration() != installed.GetWriterGeneration() || envelope.GetChannelCredential() != installed.GetChannelCredential() {
		return nil, errors.New("computer controls changed the installed physical owner")
	}
	if err := validateAgentComputerEnvelope(envelope, entry.authorityNow()); err != nil {
		return nil, err
	}
	if len(request.GetSessions()) != len(capture.members) {
		return nil, errors.New("computer controls require every captured member")
	}
	controls := make(map[string]*agentv1.SessionContinuationControl, len(capture.members))
	for _, control := range request.GetSessions() {
		id := control.GetIdentity().GetSessionId()
		if id == "" || controls[id] != nil || control.GetAuthorityGeneration() <= 0 || (control.GetStopped() && !control.GetHeld()) {
			return nil, errors.New("computer control membership is invalid")
		}
		controls[id] = control
	}
	grants := make(map[*agentRelay]*agentv1.SessionGrant, len(capture.members))
	for _, relay := range capture.members {
		session := relay.session
		control := controls[session.grant.GetIdentity().GetSessionId()]
		if control == nil || !proto.Equal(control.GetIdentity(), session.grant.GetIdentity()) || control.GetAuthorityGeneration() < session.grant.GetAuthorityGeneration() || ((capture.stopped[relay] || session.terminal) && !control.GetStopped()) {
			return nil, errors.New("computer controls regressed or replaced a captured member")
		}
		if previous := capture.controls[relay]; previous != nil && previous.GetAuthorityGeneration() == control.GetAuthorityGeneration() && (previous.GetHeld() != control.GetHeld() || previous.GetStopped() != control.GetStopped()) {
			return nil, errors.New("computer control generation changed its meaning")
		}
		grant := proto.Clone(session.grant).(*agentv1.SessionGrant)
		grant.AuthorityGeneration = control.GetAuthorityGeneration()
		// An ordinary renewal of the same owner can finish after this snapshot was
		// read. Retain its later expiry rather than making concurrent renewal regress.
		grant.ExpiresAtUnixNano = max(grant.GetExpiresAtUnixNano(), envelope.GetOperationExpiresAtUnixNano())
		if err := validateSessionGrant(entry, grant, session.clock()); err != nil {
			return nil, err
		}
		grants[relay] = grant
	}
	// Validate the whole set before changing any Session.
	capture.controls = make(map[*agentRelay]*agentv1.SessionContinuationControl, len(capture.members))
	for _, relay := range capture.members {
		control := controls[relay.session.grant.GetIdentity().GetSessionId()]
		capture.controls[relay] = proto.Clone(control).(*agentv1.SessionContinuationControl)
		capture.stopped[relay] = control.GetStopped()
		relay.session.grant = grants[relay]
		// A snapshot cannot release a guest-local hold (including a runtime
		// failure not yet recorded upstream). Only sequenced Resume may do so.
		if control.GetHeld() {
			relay.session.holdLocked()
		}
		relay.notifyLocked()
	}
	capture.controlsDigest = digest[:]
	return capture.receipt(), nil
}
