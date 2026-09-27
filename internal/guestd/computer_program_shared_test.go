package guestd

import (
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
	"sync"
	"testing"
	"time"
)

func TestProgramsShareInstanceWithIndependentAuthority(t *testing.T) {
	entry, registry, first := testProgramMount(t)
	second := proto.Clone(first).(*computerv0.ComputerRunAuthority)
	second.Fence.RunId, second.Fence.RunLeaseId = "run-2", "lease-2"
	second.Fence.BaseComputerDiskVersionId = "another-pinned-base"
	releases := make([]func(), 2)
	errors := make([]error, 2)
	var wg sync.WaitGroup
	for i, authority := range []*computerv0.ComputerRunAuthority{first, second} {
		wg.Add(1)
		go func() { defer wg.Done(); releases[i], errors[i] = registry.admitProgram(entry, authority, time.Now) }()
	}
	wg.Wait()
	for i, err := range errors {
		if err != nil {
			t.Fatalf("admission %d: %v", i, err)
		}
		defer releases[i]()
	}
	duplicate := proto.Clone(first).(*computerv0.ComputerRunAuthority)
	duplicate.Fence.AttemptNumber++
	duplicate.Fence.RunLeaseId = "replacement-lease"
	if release, err := registry.admitProgram(entry, duplicate, time.Now); err == nil {
		release()
		t.Fatal("overlapping attempt for the same Run admitted")
	}
	request := &computerv0.RenewComputerAuthorityRequest{Previous: first, NewExpiresAtUnixNano: first.Fence.ExpiresAtUnixNano + int64(time.Minute)}
	for range 2 {
		if _, err := registry.renewCurrentComputerRunAuthority(entry, request, time.Now); err != nil {
			t.Fatal(err)
		}
	}
	registry.mu.Lock()
	peerUnchanged := computerRunAuthoritiesEqual(registry.programClaimLocked(entry, second).authority, second)
	registry.mu.Unlock()
	if !peerUnchanged {
		t.Fatal("renewal changed peer authority")
	}
	releases[0]()
	if _, err := registry.renewCurrentComputerRunAuthority(entry, request, time.Now); err == nil {
		t.Fatal("released Program renewed")
	}
	if _, err := registry.renewCurrentComputerRunAuthority(entry, &computerv0.RenewComputerAuthorityRequest{Previous: second, NewExpiresAtUnixNano: second.Fence.ExpiresAtUnixNano + int64(time.Minute)}, time.Now); err != nil {
		t.Fatal(err)
	}
	if entry.baseComputerDiskVersionID != "version-1" || entry.currentWriterGeneration() != 3 {
		t.Fatal("Program changed physical writer or source base")
	}
}

func TestProgramRenewalRejectsChangedReceiptWithoutChangingPeer(t *testing.T) {
	entry, registry, authority := testProgramMount(t)
	release, err := registry.admitProgram(entry, authority, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, change := range []func(*computerv0.ComputerRunAuthority){
		func(a *computerv0.ComputerRunAuthority) { a.Fence.RunLeaseId = "other" },
		func(a *computerv0.ComputerRunAuthority) { a.WriteCapability = "other" },
		func(a *computerv0.ComputerRunAuthority) { a.Fence.WriterGeneration++ },
		func(a *computerv0.ComputerRunAuthority) { a.Fence.BaseComputerDiskVersionId = "other" },
	} {
		altered := proto.Clone(authority).(*computerv0.ComputerRunAuthority)
		change(altered)
		if _, err := registry.renewCurrentComputerRunAuthority(entry, &computerv0.RenewComputerAuthorityRequest{Previous: altered, NewExpiresAtUnixNano: authority.Fence.ExpiresAtUnixNano + int64(time.Minute)}, time.Now); err == nil {
			t.Fatal("changed receipt renewed")
		}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	claim := registry.programClaimLocked(entry, authority)
	if !computerRunAuthoritiesEqual(claim.authority, authority) || claim.previousExpiry != 0 {
		t.Fatal("failed renewal changed claim")
	}
}

func TestProgramRenewalRejectsExpiryAfterLockWait(t *testing.T) {
	for _, name := range []string{"physical", "registry"} {
		t.Run(name, func(t *testing.T) {
			entry, registry, authority := testProgramMount(t)
			deadline := time.Now().Add(200 * time.Millisecond)
			authority.Fence.ExpiresAtUnixNano = deadline.UnixNano()
			release, err := registry.admitProgram(entry, authority, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			var unlock func()
			if name == "physical" {
				entry.finalizationMu.Lock()
				unlock = entry.finalizationMu.Unlock
			} else {
				registry.mu.Lock()
				unlock = registry.mu.Unlock
			}
			locked := true
			defer func() {
				if locked {
					unlock()
				}
			}()
			done := make(chan error, 1)
			started := make(chan struct{})
			go func() {
				close(started)
				_, err := registry.renewCurrentComputerRunAuthority(entry, &computerv0.RenewComputerAuthorityRequest{Previous: authority, NewExpiresAtUnixNano: deadline.Add(time.Minute).UnixNano()}, time.Now)
				done <- err
			}()
			<-started
			timer := time.NewTimer(time.Until(deadline) + time.Millisecond)
			defer timer.Stop()
			select {
			case err := <-done:
				t.Fatalf("renewal bypassed lock: %v", err)
			case <-timer.C:
			}
			unlock()
			locked = false
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("expired claim renewed")
				}
			case <-time.After(time.Second):
				t.Fatal("renewal stuck")
			}
			registry.mu.Lock()
			defer registry.mu.Unlock()
			claim := registry.programClaimLocked(entry, authority)
			if !computerRunAuthoritiesEqual(claim.authority, authority) || claim.previousExpiry != 0 {
				t.Fatal("expiry rejection changed claim")
			}
		})
	}
}

func TestCaptureSealsTwoAdmittedPrograms(t *testing.T) {
	entry, registry, first := testProgramMount(t)
	second := proto.Clone(first).(*computerv0.ComputerRunAuthority)
	second.Fence.RunId = "run-2"
	second.Fence.RunLeaseId = "lease-2"
	request := &computerv0.FreezeComputerRequest{ComputerId: entry.computerID, ComputerInstanceId: entry.computerInstanceID, WriterGeneration: 3, CheckpointId: "checkpoint", DesiredVersion: 1, MembershipRevision: 2}
	for _, a := range []*computerv0.ComputerRunAuthority{first, second} {
		release, err := registry.admitProgram(entry, a, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		request.Runs = append(request.Runs, &computerv0.ComputerCaptureRun{RunId: a.Fence.RunId, AttemptNumber: a.Fence.AttemptNumber, RunLeaseId: a.Fence.RunLeaseId, RunWaitId: "wait-" + a.Fence.RunId})
	}
	if err := registry.sealComputerCapture(request, time.Now); err != nil {
		t.Fatal(err)
	}
	next := proto.Clone(first).(*computerv0.ComputerRunAuthority)
	next.Fence.RunId = "run-3"
	if release, err := registry.admitProgram(entry, next, time.Now); err == nil {
		release()
		t.Fatal("Program admitted after capture sealed")
	}
}

func TestExpiredProgramDoesNotFenceLivePeer(t *testing.T) {
	entry, registry, first := testProgramMount(t)
	now := time.Now()
	first.Fence.ExpiresAtUnixNano = now.Add(time.Minute).UnixNano()
	second := proto.Clone(first).(*computerv0.ComputerRunAuthority)
	second.Fence.RunId = "run-2"
	second.Fence.RunLeaseId = "lease-2"
	second.Fence.ExpiresAtUnixNano = now.Add(3 * time.Minute).UnixNano()
	for _, a := range []*computerv0.ComputerRunAuthority{first, second} {
		release, err := registry.admitProgram(entry, a, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	clock := func() time.Time { return now.Add(2 * time.Minute) }
	if _, err := registry.renewCurrentComputerRunAuthority(entry, &computerv0.RenewComputerAuthorityRequest{Previous: first, NewExpiresAtUnixNano: now.Add(4 * time.Minute).UnixNano()}, clock); err == nil {
		t.Fatal("expired Program renewed")
	}
	if _, err := registry.renewCurrentComputerRunAuthority(entry, &computerv0.RenewComputerAuthorityRequest{Previous: second, NewExpiresAtUnixNano: now.Add(4 * time.Minute).UnixNano()}, clock); err != nil {
		t.Fatal(err)
	}
}

func TestProgramAdmissionSamplesExpiryAfterPhysicalLocks(t *testing.T) {
	entry, registry, authority := testProgramMount(t)
	clock := func() time.Time {
		if entry.lifecycleMu.TryLock() {
			entry.lifecycleMu.Unlock()
			t.Fatal("clock sampled before lifecycle lock")
		}
		if entry.finalizationMu.TryLock() {
			entry.finalizationMu.Unlock()
			t.Fatal("clock sampled before control lock")
		}
		if registry.mu.TryLock() {
			registry.mu.Unlock()
			t.Fatal("clock sampled before claim lock")
		}
		return time.Unix(0, authority.Fence.ExpiresAtUnixNano)
	}
	if release, err := registry.admitProgram(entry, authority, clock); err == nil {
		release()
		t.Fatal("expired Program admitted")
	}
	if len(registry.programClaims) != 0 {
		t.Fatal("rejected admission retained claim")
	}
}

func TestRestoredProgramGrantOnlyChangesMatchingClaim(t *testing.T) {
	entry, registry, first := testProgramMount(t)
	second := proto.Clone(first).(*computerv0.ComputerRunAuthority)
	second.Fence.RunId = "run-2"
	second.Fence.RunLeaseId = "lease-2"
	for _, a := range []*computerv0.ComputerRunAuthority{first, second} {
		release, err := registry.admitProgram(entry, a, time.Now)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	// Model the physical rebind; complete-set activation remains a separate operation.
	registry.mu.Lock()
	delete(registry.entries, entry.computerInstanceID)
	entry.computerInstanceID = "restored-instance"
	entry.channelToken = "restored-channel"
	entry.setWriterGeneration(4)
	registry.entries[entry.computerInstanceID] = entry
	registry.mu.Unlock()
	grant := proto.Clone(first).(*computerv0.ComputerRunAuthority)
	grant.ChannelToken = entry.channelToken
	grant.Fence.ComputerInstanceId = entry.computerInstanceID
	grant.Fence.WriterGeneration = 4
	grant.Fence.RunLeaseId = "restored-lease"
	grant.Fence.LeaseSequence++
	entry.lifecycleMu.Lock()
	defer entry.lifecycleMu.Unlock()
	entry.finalizationMu.Lock()
	defer entry.finalizationMu.Unlock()
	for range 2 {
		if err := registry.installResumedProgramAuthorityLocked(entry, grant, time.Now); err != nil {
			t.Fatal(err)
		}
	}
	registry.mu.Lock()
	firstCurrent := computerRunAuthoritiesEqual(registry.programClaimLocked(entry, grant).authority, grant)
	secondCurrent := computerRunAuthoritiesEqual(registry.programClaimLocked(entry, second).authority, second)
	registry.mu.Unlock()
	if !firstCurrent || !secondCurrent {
		t.Fatal("restored grant replaced peer or failed exact replay")
	}
	altered := proto.Clone(grant).(*computerv0.ComputerRunAuthority)
	altered.Fence.RunLeaseId = "substitution"
	altered.Fence.LeaseSequence++
	if err := registry.installResumedProgramAuthorityLocked(entry, altered, time.Now); err == nil {
		t.Fatal("same-Instance grant replaced installed authority")
	}
}
