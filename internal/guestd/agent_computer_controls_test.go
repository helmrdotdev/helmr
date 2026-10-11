package guestd

import (
	"context"
	"errors"
	"testing"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

// These fixtures have no concurrent durable controls. Snapshot their explicit
// current state for the protocol; changed-state tests modify this request below.
func currentAgentComputerTestControls(r *computerOperationRegistry) *agentv1.ComputerSessionControls {
	r.agentCaptureMu.Lock()
	defer r.agentCaptureMu.Unlock()
	r.mu.RLock()
	defer r.mu.RUnlock()
	capture := r.agentCapture
	unlock := lockAgentCaptureMembers(capture.members)
	defer unlock()
	p := capture.installation
	controls := &agentv1.ComputerSessionControls{Envelope: proto.Clone(p.Envelope).(*computerv0.ComputerOperationEnvelope), CheckpointId: p.Capture.CheckpointId, DesiredVersion: p.DesiredVersion}
	controls.Envelope.OperationExpiresAtUnixNano = int64(^uint64(0) >> 1)
	for _, relay := range capture.members {
		grant := relay.session.grant
		controls.Envelope.OperationExpiresAtUnixNano = min(controls.Envelope.OperationExpiresAtUnixNano, grant.GetExpiresAtUnixNano())
		stopped := capture.stopped[relay]
		controls.Sessions = append(controls.Sessions, &agentv1.SessionContinuationControl{Identity: proto.Clone(grant.Identity).(*agentv1.SessionIdentity), AuthorityGeneration: grant.AuthorityGeneration, Held: relay.session.held || stopped, Stopped: stopped})
	}
	return controls
}
func applyAgentComputerTestControls(t *testing.T, r *computerOperationRegistry) {
	t.Helper()
	if _, err := r.applyAgentComputerControls(currentAgentComputerTestControls(r)); err != nil {
		t.Fatal(err)
	}
}

func TestAgentComputerSnapshotHoldSupersedesPendingResume(t *testing.T) {
	r, capture, _ := agentComputerFixture(t)
	session := r.agentSessions["session"].session
	if err := session.send(&agentv1.GuestCommand{Identity: session.grant.Identity, DeliveryId: "old-resume", ControlSequence: 1, Command: &agentv1.GuestCommand_Resume{Resume: &agentv1.SessionResume{}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.captureAgentComputer(t.Context(), capture); err != nil {
		t.Fatal(err)
	}
	p := agentComputerInstallation(r, capture, false)
	if _, err := r.installAgentComputer(t.Context(), p, false, testComputerAuthorityClock()); err != nil {
		t.Fatal(err)
	}
	controls := currentAgentComputerTestControls(r)
	for _, control := range controls.Sessions {
		control.AuthorityGeneration++
		control.Held = true
	}
	if _, err := r.applyAgentComputerControls(controls); err != nil {
		t.Fatal(err)
	}
	guestControlReceipt(t, session, "old-resume", false)
	if !session.held {
		t.Fatal("pre-snapshot Resume cleared current durable hold")
	}
}

func TestAgentComputerCurrentControlsStopLateCancellationBeforeThaw(t *testing.T) {
	r, capture, processes := agentComputerFixture(t)
	if _, err := r.captureAgentComputer(t.Context(), capture); err != nil {
		t.Fatal(err)
	}
	p := agentComputerInstallation(r, capture, false)
	if _, err := r.installAgentComputer(t.Context(), p, false, testComputerAuthorityClock()); err != nil {
		t.Fatal(err)
	}
	for _, grant := range p.Grants {
		attachAgentRelay(t, r.agentSessions[grant.Identity.SessionId], 1, grant)
	}
	if receipt, err := r.installAgentComputer(t.Context(), p, true, nil); err == nil || receipt.GetActivationStarted() {
		t.Fatal("activated without current controls")
	}
	controls := currentAgentComputerTestControls(r)
	for _, control := range controls.Sessions {
		control.AuthorityGeneration++
		control.Held = true
		if control.Identity.SessionId == "peer" {
			control.Stopped = true
		}
	}
	before := proto.Clone(r.agentCapture.installation)
	if _, err := r.applyAgentComputerControls(controls); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(before, r.agentCapture.installation) {
		t.Fatal("current controls rewrote installed request")
	}
	processes[1].thawHook = func(context.Context) error { t.Error("late-cancelled process thawed"); return nil }
	thawed := false
	processes[0].thawHook = func(context.Context) error {
		thawed = true
		if !r.agentSessions["peer"].session.physicalClosed || !r.agentSessions["session"].session.held {
			t.Error("peer thaw preceded stop or hold application")
		}
		return nil
	}
	receipt, err := r.installAgentComputer(t.Context(), p, true, nil)
	if err != nil || !receipt.GetActivated() || !thawed || len(receipt.GetControlsDigest()) != 32 {
		t.Fatalf("current-control activation: %v %v", receipt, err)
	}
}

func TestAgentComputerCurrentControlsRejectInvalidCompleteSet(t *testing.T) {
	for _, kind := range []string{"missing", "duplicate", "epoch", "owner", "generation", "expiry"} {
		t.Run(kind, func(t *testing.T) {
			r, capture, _ := agentComputerFixture(t)
			if _, err := r.captureAgentComputer(t.Context(), capture); err != nil {
				t.Fatal(err)
			}
			p := agentComputerInstallation(r, capture, false)
			if _, err := r.installAgentComputer(t.Context(), p, false, testComputerAuthorityClock()); err != nil {
				t.Fatal(err)
			}
			controls := currentAgentComputerTestControls(r)
			switch kind {
			case "missing":
				controls.Sessions = controls.Sessions[:1]
			case "duplicate":
				controls.Sessions[1] = proto.Clone(controls.Sessions[0]).(*agentv1.SessionContinuationControl)
			case "epoch":
				controls.Sessions[1].Identity.ProcessEpoch++
			case "owner":
				controls.Envelope.ComputerInstanceId = "other"
			case "generation":
				controls.Sessions[1].AuthorityGeneration--
			case "expiry":
				controls.Envelope.OperationExpiresAtUnixNano = 1
			}
			controls.Sessions[0].Held = true
			if _, err := r.applyAgentComputerControls(controls); err == nil {
				t.Fatal("invalid controls accepted")
			}
			if r.agentSessions[controls.Sessions[0].Identity.SessionId].session.held || len(r.agentCapture.controlsDigest) != 0 {
				t.Fatal("invalid set partially applied")
			}
		})
	}
}

func TestAgentComputerCurrentControlsRequireRefreshAfterNewGeneration(t *testing.T) {
	r, capture, _ := agentComputerFixture(t)
	if _, err := r.captureAgentComputer(t.Context(), capture); err != nil {
		t.Fatal(err)
	}
	p := agentComputerInstallation(r, capture, false)
	if _, err := r.installAgentComputer(t.Context(), p, false, testComputerAuthorityClock()); err != nil {
		t.Fatal(err)
	}
	for _, grant := range p.Grants {
		attachAgentRelay(t, r.agentSessions[grant.Identity.SessionId], 1, grant)
	}
	controls := currentAgentComputerTestControls(r)
	if _, err := r.applyAgentComputerControls(controls); err != nil {
		t.Fatal(err)
	}
	changed := proto.Clone(controls).(*agentv1.ComputerSessionControls)
	changed.Sessions[0].Held = true
	if _, err := r.applyAgentComputerControls(changed); err == nil {
		t.Fatal("same generation changed meaning")
	}
	relay := r.agentSessions[controls.Sessions[0].Identity.SessionId]
	fresh := proto.Clone(relay.session.grant).(*agentv1.SessionGrant)
	fresh.AuthorityGeneration++
	if err := relay.session.renew(fresh); err != nil {
		t.Fatal(err)
	}
	if receipt, err := r.installAgentComputer(t.Context(), p, true, nil); err == nil || receipt.GetActivationStarted() {
		t.Fatal("activated with obsolete control snapshot")
	}
	applyAgentComputerTestControls(t, r)
	if _, err := r.installAgentComputer(t.Context(), p, true, nil); err != nil {
		t.Fatal(err)
	}
}

func TestAgentComputerCurrentControlsPreserveGuestLocalHold(t *testing.T) {
	r, capture, _ := agentComputerFixture(t)
	r.agentSessions["session"].session.held = true
	if _, err := r.captureAgentComputer(t.Context(), capture); err != nil {
		t.Fatal(err)
	}
	p := agentComputerInstallation(r, capture, false)
	if _, err := r.installAgentComputer(t.Context(), p, false, testComputerAuthorityClock()); err != nil {
		t.Fatal(err)
	}
	controls := currentAgentComputerTestControls(r)
	for _, control := range controls.Sessions {
		control.Held = false
	}
	if _, err := r.applyAgentComputerControls(controls); err != nil {
		t.Fatal(err)
	}
	if !r.agentSessions["session"].session.held {
		t.Fatal("snapshot released guest-local hold without Resume")
	}
}

func TestAgentComputerCurrentControlsRecheckGenerationBeforeEachThaw(t *testing.T) {
	for _, phase := range []string{"stop", "peer-thaw"} {
		t.Run(phase, func(t *testing.T) {
			r, capture, processes := agentComputerFixture(t)
			if _, err := r.captureAgentComputer(t.Context(), capture); err != nil {
				t.Fatal(err)
			}
			p := agentComputerInstallation(r, capture, false)
			if phase == "stop" {
				p.StoppedSessions = []*agentv1.SessionIdentity{proto.Clone(capture.Sessions[1]).(*agentv1.SessionIdentity)}
			}
			if _, err := r.installAgentComputer(t.Context(), p, false, testComputerAuthorityClock()); err != nil {
				t.Fatal(err)
			}
			for _, grant := range p.Grants {
				attachAgentRelay(t, r.agentSessions[grant.Identity.SessionId], 1, grant)
			}
			applyAgentComputerTestControls(t, r)
			started, release := make(chan struct{}), make(chan struct{})
			if phase == "stop" {
				processes[1].closeStarted, processes[1].closeRelease = started, release
			} else {
				processes[1].thawHook = func(context.Context) error { close(started); <-release; return nil }
			}
			attempts := 0
			processes[0].thawHook = func(context.Context) error { attempts++; return nil }
			done := make(chan error, 1)
			go func() { _, err := r.installAgentComputer(t.Context(), p, true, nil); done <- err }()
			<-started
			relay := r.agentSessions["session"]
			relay.mu.Lock()
			connection, sequence := relay.connection, relay.attachment
			relay.session.mu.Lock()
			grant := proto.Clone(relay.session.grant).(*agentv1.SessionGrant)
			relay.session.mu.Unlock()
			relay.mu.Unlock()
			grant.AuthorityGeneration++
			err := relay.handleHostMessage(connection, sequence, &agentv1.HostSessionMessage{AttachmentSequence: sequence, Message: &agentv1.HostSessionMessage_Renew{Renew: grant}})
			close(release)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-done; err == nil {
				t.Fatal("activation accepted stale controls")
			}
			if attempts != 0 || !processes[0].physicallyFrozen {
				t.Fatal("new-generation member thawed with stale controls")
			}
			processes[1].thawHook = nil
			applyAgentComputerTestControls(t, r)
			if receipt, err := r.installAgentComputer(t.Context(), p, true, nil); err != nil || !receipt.GetActivated() {
				t.Fatalf("refresh did not recover same attempt: %v %v", receipt, err)
			}
		})
	}
}

func TestAgentComputerCurrentControlsStopNewCancellationOnActivationRetry(t *testing.T) {
	r, capture, processes := agentComputerFixture(t)
	if _, err := r.captureAgentComputer(t.Context(), capture); err != nil {
		t.Fatal(err)
	}
	p := agentComputerInstallation(r, capture, false)
	if _, err := r.installAgentComputer(t.Context(), p, false, testComputerAuthorityClock()); err != nil {
		t.Fatal(err)
	}
	for _, grant := range p.Grants {
		attachAgentRelay(t, r.agentSessions[grant.Identity.SessionId], 1, grant)
	}
	applyAgentComputerTestControls(t, r)
	processes[0].thawHook = func(context.Context) error { return errors.New("thaw failed after peer activation") }
	if receipt, err := r.installAgentComputer(t.Context(), p, true, nil); err == nil || !receipt.GetActivationStarted() || receipt.GetActivated() {
		t.Fatalf("did not fail partial activation: %v %v", receipt, err)
	}
	controls := currentAgentComputerTestControls(r)
	for _, control := range controls.Sessions {
		if control.Identity.SessionId == "peer" {
			control.AuthorityGeneration++
			control.Held, control.Stopped = true, true
		}
	}
	if _, err := r.applyAgentComputerControls(controls); err != nil {
		t.Fatal(err)
	}
	processes[1].thawHook = func(context.Context) error { t.Error("newly cancelled member thawed"); return nil }
	processes[0].thawHook = func(context.Context) error {
		if !r.agentSessions["peer"].session.physicalClosed {
			t.Error("peer thaw preceded new cancellation cleanup")
		}
		return nil
	}
	if receipt, err := r.installAgentComputer(t.Context(), p, true, nil); err != nil || !receipt.GetActivated() {
		t.Fatalf("new terminal controls wedged activation retry: %v %v", receipt, err)
	}
}
