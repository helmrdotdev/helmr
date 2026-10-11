package guestd

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

func TestAgentComputerContinuationRequiresExactStoppedMembers(t *testing.T) {
	for _, bad := range []string{"duplicate", "absent", "epoch", "empty"} {
		t.Run(bad, func(t *testing.T) {
			r, capture, _ := agentComputerFixture(t)
			if _, err := r.captureAgentComputer(t.Context(), capture); err != nil {
				t.Fatal(err)
			}
			installation := agentComputerInstallation(r, capture, false)
			installation.StoppedSessions = []*agentv1.SessionIdentity{proto.Clone(capture.Sessions[0]).(*agentv1.SessionIdentity)}
			switch bad {
			case "duplicate":
				installation.StoppedSessions = append(installation.StoppedSessions, proto.Clone(capture.Sessions[0]).(*agentv1.SessionIdentity))
			case "absent":
				installation.StoppedSessions[0].SessionId = "absent"
			case "epoch":
				installation.StoppedSessions[0].ProcessEpoch++
			case "empty":
				installation.StoppedSessions[0] = nil
			}
			if _, err := r.installAgentComputer(t.Context(), installation, false, testComputerAuthorityClock()); err == nil {
				t.Fatal("invalid stopped membership installed")
			}
			if r.agentCapture.installation != nil || r.entries["instance"] == nil {
				t.Fatal("invalid stop partially installed")
			}
		})
	}
}

func TestAgentComputerContinuationStopsCancelledMemberBeforePeerThaw(t *testing.T) {
	for _, kind := range []string{"restore", "source-abort", "stop-failed", "abort-stop-failed"} {
		t.Run(kind, func(t *testing.T) {
			r, capture, processes := agentComputerFixture(t)
			if _, err := r.captureAgentComputer(t.Context(), capture); err != nil {
				t.Fatal(err)
			}
			installation := agentComputerInstallation(r, capture, kind == "source-abort" || kind == "abort-stop-failed")
			installation.StoppedSessions = []*agentv1.SessionIdentity{proto.Clone(capture.Sessions[1]).(*agentv1.SessionIdentity)}
			peer := r.agentSessions["peer"]
			if kind == "stop-failed" || kind == "abort-stop-failed" {
				processes[1].closeErr = errors.New("scope not empty")
			}
			processes[1].thawHook = func(context.Context) error { t.Error("cancelled process thawed"); return nil }
			thawed := false
			processes[0].thawHook = func(context.Context) error {
				thawed = true
				if !peer.session.physicalClosed {
					t.Error("live peer thawed before cancelled member stopped")
				}
				return nil
			}
			if _, err := r.installAgentComputer(t.Context(), installation, false, testComputerAuthorityClock()); err != nil {
				t.Fatal(err)
			}
			applyAgentComputerTestControls(t, r)
			// A stopped member needs no executable attachment to prove closure.
			attachAgentRelay(t, r.agentSessions["session"], 1, installation.Grants[0])
			receipt, err := r.installAgentComputer(t.Context(), installation, true, nil)
			if kind == "stop-failed" || kind == "abort-stop-failed" {
				if err == nil || thawed || !processes[0].physicallyFrozen || !receipt.GetActivationStarted() {
					t.Fatalf("unsafe failed stop: %v %v", receipt, err)
				}
				if _, err := r.installAgentComputer(t.Context(), installation, true, nil); err == nil {
					t.Fatal("failed physical stop became success on retry")
				}
				return
			}
			if err != nil || !receipt.GetActivated() || !thawed {
				t.Fatalf("continuation: %v %v", receipt, err)
			}
			peer.mu.Lock()
			retained := len(peer.outbox) == 1 && peer.outbox[0].GetStopped() != nil
			peer.mu.Unlock()
			if !retained {
				t.Fatal("cancelled member lost its normal stopped receipt")
			}
			if peer.session.checkpointID != "" {
				t.Fatal("stopped member retained capture gate")
			}
			// Its retained transport can still report closure without a process restart.
			go peer.sendLoop()
			host := attachAgentRelay(t, peer, 1, installation.Grants[1])
			var delivered agentv1.GuestSessionMessage
			if err := frameio.ReadProtoFrameBounded(host, maxAgentTransportFrameBytes, &delivered); err != nil {
				t.Fatal(err)
			}
			if delivered.GetStopped() == nil || !proto.Equal(delivered.GetIdentity(), capture.Sessions[1]) {
				t.Fatal("terminal transport lost stopped identity")
			}
			if err := writeAgentTransportFrame(host, &agentv1.HostSessionMessage{AttachmentSequence: 1, Message: &agentv1.HostSessionMessage_Acknowledged{Acknowledged: &agentv1.SessionEventsAcknowledged{ThroughSequence: delivered.GetEventSequence()}}}); err != nil {
				t.Fatal(err)
			}
			waitAgentRelay(t, peer, func() bool { return len(peer.outbox) == 0 })
			if _, err := r.installAgentComputer(t.Context(), installation, true, nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAgentComputerContinuationStopsMembersInParallel(t *testing.T) {
	r, capture, processes := agentComputerFixture(t)
	if _, err := r.captureAgentComputer(t.Context(), capture); err != nil {
		t.Fatal(err)
	}
	installation := agentComputerInstallation(r, capture, false)
	for _, identity := range capture.Sessions {
		installation.StoppedSessions = append(installation.StoppedSessions, proto.Clone(identity).(*agentv1.SessionIdentity))
	}
	release := make(chan struct{})
	for _, process := range processes {
		process.closeStarted = make(chan struct{})
		process.closeRelease = release
		process.thawHook = func(context.Context) error { t.Error("terminal process thawed"); return nil }
	}
	if _, err := r.installAgentComputer(t.Context(), installation, false, testComputerAuthorityClock()); err != nil {
		t.Fatal(err)
	}
	applyAgentComputerTestControls(t, r)
	done := make(chan error, 1)
	go func() { _, err := r.installAgentComputer(t.Context(), installation, true, nil); done <- err }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for _, process := range processes {
		select {
		case <-process.closeStarted:
		case <-ctx.Done():
			close(release)
			<-done
			t.Fatal("member stops were serialized")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, relay := range r.agentCapture.members {
		if !relay.session.physicalClosed || relay.session.checkpointID != "" {
			t.Fatal("incomplete terminal capture accounting")
		}
	}
}

func TestAgentComputerContinuationRetriesAfterStopAndThawFailure(t *testing.T) {
	for _, failure := range []string{"expiry", "thaw"} {
		t.Run(failure, func(t *testing.T) {
			r, capture, processes := agentComputerFixture(t)
			if _, err := r.captureAgentComputer(t.Context(), capture); err != nil {
				t.Fatal(err)
			}
			installation := agentComputerInstallation(r, capture, false)
			installation.StoppedSessions = []*agentv1.SessionIdentity{proto.Clone(capture.Sessions[1]).(*agentv1.SessionIdentity)}
			live := r.agentSessions["session"]
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			live.session.clock = func() time.Time { return time.Unix(0, clock.Load()) }
			if _, err := r.installAgentComputer(t.Context(), installation, false, testComputerAuthorityClock()); err != nil {
				t.Fatal(err)
			}
			applyAgentComputerTestControls(t, r)
			attachAgentRelay(t, live, 1, installation.Grants[0])
			release := make(chan struct{})
			processes[1].closeStarted = make(chan struct{})
			processes[1].closeRelease = release
			failThaw := failure == "thaw"
			thawAttempts := 0
			processes[0].thawHook = func(context.Context) error {
				thawAttempts++
				if failThaw {
					return errors.New("thaw failed")
				}
				return nil
			}
			done := make(chan error, 1)
			go func() { _, err := r.installAgentComputer(t.Context(), installation, true, nil); done <- err }()
			<-processes[1].closeStarted
			if failure == "expiry" {
				clock.Store(installation.Grants[0].ExpiresAtUnixNano + int64(time.Second))
			}
			close(release)
			if err := <-done; err == nil {
				t.Fatal("first activation should fail")
			}
			if failure == "expiry" && thawAttempts != 0 {
				t.Fatal("expired authority reached physical thaw")
			}
			if !r.agentSessions["peer"].session.physicalClosed || !processes[0].physicallyFrozen {
				t.Fatal("failed activation lost stop/freeze accounting")
			}
			failThaw = false
			grant := proto.Clone(installation.Grants[0]).(*agentv1.SessionGrant)
			grant.ExpiresAtUnixNano = max(grant.ExpiresAtUnixNano, clock.Load()) + int64(time.Hour)
			if err := live.session.renew(grant); err != nil {
				t.Fatal(err)
			}
			receipt, err := r.installAgentComputer(t.Context(), installation, true, nil)
			if err != nil || !receipt.GetActivated() {
				t.Fatalf("retry after confirmed stop: %v %v", receipt, err)
			}
			if failure == "expiry" && thawAttempts != 1 {
				t.Fatal("renewed authority did not thaw exactly once")
			}
		})
	}
}
