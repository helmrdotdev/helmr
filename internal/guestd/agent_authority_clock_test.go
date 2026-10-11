package guestd

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

func TestAgentComputerContinuationSynchronizesWallClockBeforeActivation(t *testing.T) {
	for _, sourceAbort := range []bool{false, true} {
		t.Run(map[bool]string{false: "restore", true: "source-abort"}[sourceAbort], func(t *testing.T) {
			r, capture, processes := agentComputerFixture(t)
			if _, err := r.captureAgentComputer(t.Context(), capture); err != nil {
				t.Fatal(err)
			}
			install := agentComputerInstallation(r, capture, sourceAbort)
			hostTime := time.Now().Add(15 * time.Minute)
			guestTime := time.Now()
			calls := 0
			r.setWallClock = func(now time.Time) error {
				for _, process := range processes {
					if !process.physicallyFrozen {
						t.Fatal("workload ran before clock synchronization")
					}
				}
				guestTime = now
				calls++
				return nil
			}
			clock := &computerAuthorityClock{anchor: time.Now(), authority: hostTime.UnixNano()}
			if _, err := r.installAgentComputer(t.Context(), install, false, clock); err != nil {
				t.Fatal(err)
			}
			if guestTime.Before(hostTime) || guestTime.After(hostTime.Add(time.Second)) {
				t.Fatalf("guest wall clock still differs from current host time: %v", guestTime)
			}
			// Lost installation replies must not step time again on receipt replay.
			if _, err := r.installAgentComputer(t.Context(), install, false, clock); err != nil {
				t.Fatal(err)
			}
			applyAgentComputerTestControls(t, r)
			for _, grant := range install.Grants {
				attachAgentRelay(t, r.agentSessions[grant.GetIdentity().GetSessionId()], 1, grant)
			}
			if _, err := r.installAgentComputer(t.Context(), install, true, nil); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("clock changed %d times during one continuation", calls)
			}
		})
	}
}

func TestAgentComputerClockFailureKeepsCaptureSealedAndRetryable(t *testing.T) {
	r, capture, processes := agentComputerFixture(t)
	if _, err := r.captureAgentComputer(t.Context(), capture); err != nil {
		t.Fatal(err)
	}
	install := agentComputerInstallation(r, capture, false)
	entry := r.entries["instance"]
	original := proto.Clone(r.agentSessions["session"].session.grant)
	failed := errors.New("clock syscall denied")
	r.setWallClock = func(time.Time) error { return failed }
	if _, err := r.installAgentComputer(t.Context(), install, false, testComputerAuthorityClock()); !errors.Is(err, failed) {
		t.Fatalf("lost clock synchronization failure: %v", err)
	}
	if r.agentCapture.installation != nil || entry.authorityClock.Load() != nil || r.entries["instance"] != entry || !proto.Equal(original, r.agentSessions["session"].session.grant) {
		t.Fatal("failed clock synchronization partially installed continuation")
	}
	for _, process := range processes {
		if !process.physicallyFrozen {
			t.Fatal("clock failure released a workload")
		}
	}
	if _, err := r.installAgentComputer(t.Context(), install, true, nil); err == nil {
		t.Fatal("activation bypassed failed clock synchronization")
	}
	r.setWallClock = func(time.Time) error { return nil }
	if _, err := r.installAgentComputer(t.Context(), install, false, testComputerAuthorityClock()); err != nil {
		t.Fatal(err)
	}
}

func TestAgentComputerWallClockDoesNotInheritAuthorityFloor(t *testing.T) {
	r, capture, _ := agentComputerFixture(t)
	if _, err := r.captureAgentComputer(t.Context(), capture); err != nil {
		t.Fatal(err)
	}
	entry := r.entries["instance"]
	previous := &computerAuthorityClock{anchor: time.Now(), authority: time.Now().Add(15 * time.Minute).UnixNano()}
	entry.authorityClock.Store(previous)
	observed := testComputerAuthorityClock()
	var wallTime time.Time
	r.setWallClock = func(now time.Time) error { wallTime = now; return nil }
	if _, err := r.installAgentComputer(t.Context(), agentComputerInstallation(r, capture, false), false, observed); err != nil {
		t.Fatal(err)
	}
	if difference := wallTime.Sub(observed.now()); difference > time.Second || difference < -time.Second {
		t.Fatalf("wall clock inherited old authority instead of fresh host time: %v", difference)
	}
	if entry.authorityNow().Before(previous.now().Add(-time.Millisecond)) {
		t.Fatal("wall clock correction regressed lease authority")
	}
}

func TestAgentComputerAuthorityChallengeRejectsReplayAndCountsDelay(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		guest, host := net.Pipe()
		_ = guest.SetDeadline(time.Now().Add(time.Second))
		_ = host.SetDeadline(time.Now().Add(time.Second))
		done := make(chan error, 1)
		go func() {
			clock, err := observeComputerAuthority(guest)
			if err == nil && clock.now().Before(time.Now().Add(23*time.Hour)) {
				t.Error("host time was not installed")
			}
			done <- err
		}()
		var challenge agentv1.ComputerAuthorityChallenge
		if err := frameio.ReadProtoFrameBounded(host, 256, &challenge); err != nil {
			t.Fatal(err)
		}
		sample := time.Now().Add(24 * time.Hour)
		if mismatch {
			challenge.Nonce[0] ^= 1
		}
		if err := writeAgentTransportFrame(host, &agentv1.ComputerAuthorityObservation{Nonce: challenge.Nonce, AuthorityTimeUnixNano: sample.UnixNano()}); err != nil {
			t.Fatal(err)
		}
		err := <-done
		if (err != nil) != mismatch {
			t.Fatalf("mismatch %v: %v", mismatch, err)
		}
		guest.Close()
		host.Close()
	}
	clock := &computerAuthorityClock{anchor: time.Now().Add(-time.Minute), authority: time.Now().Add(time.Hour).UnixNano()}
	if clock.now().Before(time.Now().Add(time.Hour + 59*time.Second)) {
		t.Fatal("challenge round trip extended authority")
	}
}

func TestAgentComputerInstallationUsesAuthorityTimeAndReplayKeepsAnchor(t *testing.T) {
	r, capture, _ := agentComputerFixture(t)
	if _, err := r.captureAgentComputer(t.Context(), capture); err != nil {
		t.Fatal(err)
	}
	install := agentComputerInstallation(r, capture, false)
	entry := r.entries["instance"]
	original := proto.Clone(r.agentSessions["session"].session.grant)
	future := &computerAuthorityClock{anchor: time.Now(), authority: time.Now().Add(24 * time.Hour).UnixNano()}
	if _, err := r.installAgentComputer(t.Context(), install, false, future); err == nil {
		t.Fatal("stale guest wall clock revived expired authority")
	}
	if entry.authorityClock.Load() != nil || !proto.Equal(original, r.agentSessions["session"].session.grant) {
		t.Fatal("rejected installation changed authority")
	}
	if _, err := r.installAgentComputer(t.Context(), install, false, nil); err == nil {
		t.Fatal("accepted unchallenged installation")
	}
	clock := testComputerAuthorityClock()
	if _, err := r.installAgentComputer(t.Context(), install, false, clock); err != nil {
		t.Fatal(err)
	}
	applyAgentComputerTestControls(t, r)
	installed := entry.authorityClock.Load()
	if _, err := r.installAgentComputer(t.Context(), install, false, future); err != nil {
		t.Fatal(err)
	}
	applyAgentComputerTestControls(t, r)
	if entry.authorityClock.Load() != installed {
		t.Fatal("receipt replay reset expiry anchor")
	}
	// Every retained member and later admission uses the same Computer clock.
	entry.authorityClock.Store(future)
	for _, relay := range r.agentSessions {
		if relay.session.authorizeLocked() == nil {
			t.Fatal("member ignored Computer authority clock")
		}
	}
	if !entry.authorityNow().After(time.Now().Add(23 * time.Hour)) {
		t.Fatal("Computer clock was not retained")
	}
}

func TestAgentComputerAuthorityRecalibrationNeverMovesBackwards(t *testing.T) {
	old := &computerAuthorityClock{anchor: time.Now(), authority: time.Now().Add(time.Hour).UnixNano()}
	next := testComputerAuthorityClock().notBefore(old)
	if next.now().Before(old.now().Add(-time.Millisecond)) {
		t.Fatal("recalibration extended an existing lease")
	}
}

func TestAgentComputerAuthorityBoundsWritesAndCommands(t *testing.T) {
	session, _ := agentSessionFixture(t)
	future := &computerAuthorityClock{anchor: time.Now(), authority: time.Now().Add(24 * time.Hour).UnixNano()}
	session.entry.authorityClock.Store(future)
	session.clock = session.entry.authorityNow
	session.grant.ExpiresAtUnixNano = future.now().Add(50 * time.Millisecond).UnixNano()
	relay := newAgentRelay(t.Context(), session, nil)
	if remaining := time.Until(relay.writeDeadline()); remaining <= 0 || remaining > 60*time.Millisecond {
		t.Fatalf("write deadline ignored authoritative TTL: %v", remaining)
	}
	entry := &computerMountEntry{}
	registry := testComputerBasicExecRegistry(t, entry)
	entry.authorityClock.Store(future)
	request := testComputerBasicExecRequest("expired", "fingerprint")
	if _, _, err := registry.startComputerBasicExec(t.Context(), entry, request); err == nil {
		t.Fatal("expired command was launched")
	}
	if err := registry.cancelCommand(t.Context(), request.Envelope); err == nil {
		t.Fatal("expired cancellation was installed")
	}
	if len(entry.commands) != 0 {
		t.Fatal("expired authority changed command state")
	}
}

func TestAgentComputerAuthorityRejectsSlowChallenge(t *testing.T) {
	nonce := []byte("challenge")
	observation := &agentv1.ComputerAuthorityObservation{Nonce: nonce, AuthorityTimeUnixNano: time.Now().UnixNano()}
	if _, err := computerAuthorityObservation(nonce, observation, time.Now().Add(-3*time.Second)); err == nil {
		t.Fatal("accepted a permanently skewed authority clock")
	}
}

func TestAgentComputerExpiredCaptureReplayRetainsReceipt(t *testing.T) {
	r, capture, _ := agentComputerFixture(t)
	if _, err := r.captureAgentComputer(t.Context(), capture); err != nil {
		t.Fatal(err)
	}
	r.agentCapture.entry.authorityClock.Store(&computerAuthorityClock{anchor: time.Now(), authority: time.Now().Add(24 * time.Hour).UnixNano()})
	receipt, err := r.captureAgentComputer(t.Context(), capture)
	if err == nil || !receipt.GetFrozen() || receipt.GetCheckpointId() != capture.CheckpointId {
		t.Fatalf("expired replay lost retained seal: %v %v", receipt, err)
	}
}
