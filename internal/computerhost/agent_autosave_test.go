package computerhost

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestAutosaveQuietWindowAndContinuousMutationDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saves, base := agentSaveFixture(t, "")
		client := &periodicAgentSaveClient{agentSaveTestClient: base}
		saves.client = client
		first := time.Now()
		state := disk.VersionChanges{Generation: 1, ChangedAt: first, LastChangedAt: first}
		saves.changes = func() disk.VersionChanges { return state }
		if err := saves.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(base.requests) != 0 || client.discoveries[0].Admit {
			t.Fatal("mutation captured before quiet interval")
		}
		for i := 1; i <= 4; i++ {
			time.Sleep(200 * time.Millisecond)
			state.LastChangedAt = time.Now()
			if err := saves.step(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(base.requests) != 0 {
				t.Fatal("continuous writes were captured before either deadline")
			}
		}
		time.Sleep(200 * time.Millisecond)
		state.LastChangedAt = time.Now()
		if err := saves.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(base.requests) != 2 || !client.discoveries[len(client.discoveries)-1].Admit {
			t.Fatal("new writes extended the fixed maximum")
		}
		saves.release()
	})
}

func TestAutosaveIsEligibleAtQuietDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saves, base := agentSaveFixture(t, "")
		client := &periodicAgentSaveClient{agentSaveTestClient: base}
		saves.client = client
		first := time.Now()
		saves.changes = func() disk.VersionChanges {
			return disk.VersionChanges{Generation: 1, ChangedAt: first, LastChangedAt: first}
		}
		time.Sleep(autosaveQuiet - time.Nanosecond)
		if err := saves.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(base.requests) != 0 {
			t.Fatal("quiet deadline fired early")
		}
		time.Sleep(time.Nanosecond)
		if err := saves.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(base.requests) != 2 {
			t.Fatal("quiet deadline did not publish")
		}
		saves.release()
	})
}

type accountingSaveClient struct {
	*agentSaveTestClient
	since      time.Time
	claimed    bool
	admissions int
}

func (c *accountingSaveClient) NextAgentSave(ctx context.Context, request workerapi.AgentSaveDiscovery) (workerapi.AgentSavePending, error) {
	if c.claimed || request.Admit {
		c.claimed = true
		c.admissions++
		return c.agentSaveTestClient.NextAgentSave(ctx, request)
	}
	age := time.Since(c.since).Milliseconds()
	return workerapi.AgentSavePending{PendingForMillis: &age}, nil
}

func TestAutosaveCleanCompletionWaitsForProofAndDoesNotRestartDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saves, base := agentSaveFixture(t, "")
		client := &accountingSaveClient{agentSaveTestClient: base, since: time.Now()}
		saves.client = client
		saves.changes = func() disk.VersionChanges { return disk.VersionChanges{} }
		if err := saves.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(base.requests) != 0 || saves.accountingDue.IsZero() {
			t.Fatal("clean completion bypassed its proof deadline")
		}
		time.Sleep(autosaveQuiet / 2)
		if err := saves.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(autosaveQuiet / 2)
		if err := saves.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(base.requests) != 2 || client.admissions != 1 {
			t.Fatal("repeated discovery reset the completion deadline")
		}
		saves.release()
	})
}

func TestAutosaveRetryDoesNotResetSuccessorAge(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saves, base := agentSaveFixture(t, "commit")
		state := disk.VersionChanges{Generation: 1}
		saves.changes = func() disk.VersionChanges { return state }
		if err := saves.step(t.Context()); err == nil {
			t.Fatal("publication uncertainty was not injected")
		}
		firstCut := saves.pending.Save.SaveID
		since := time.Now()
		state = disk.VersionChanges{Generation: 2, ChangedAt: since, LastChangedAt: since}
		time.Sleep(2 * autosaveMaxWait)
		if err := saves.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(base.requests) != 3 || base.requests[2].Save.SaveID != firstCut {
			t.Fatal("retry replaced the admitted cut")
		}
		if due := saves.nextSaveTime(state, time.Now()); due.After(time.Now()) || !due.Equal(since.Add(autosaveQuiet)) {
			t.Fatalf("publication reset the successor deadline: %v", due)
		}
		saves.release()
	})
}

func TestAutosaveOlderCompletionCapsNewerMutationWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := agentSaveFixture(t, "")
		s.accountingDue = time.Now().Add(autosaveMaxWait)
		time.Sleep(autosaveMaxWait)
		now := time.Now()
		state := disk.VersionChanges{Generation: 1, ChangedAt: now, LastChangedAt: now}
		if due := s.nextSaveTime(state, now); due.After(now) {
			t.Fatal("new mutation postponed old completion proof")
		}
	})
}

func TestAutosaveMaintenanceObservesDirtyDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saves, base := agentSaveFixture(t, "")
		client := &periodicAgentSaveClient{agentSaveTestClient: base}
		saves.client = client
		saves.wake = nil
		first := time.Now()
		changes := disk.VersionChanges{Generation: 1, ChangedAt: first, LastChangedAt: first}
		saves.changes = func() disk.VersionChanges { return changes }
		capture := &hintMaintenanceCapture{phase: "adopt", saveHostCapture: saveHostCapture{base.fixture}, entered: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
		saves.maintenance = []*saveMaintenance{{capture: capture, saveID: base.save.SaveID}}
		done := make(chan error, 1)
		go func() { done <- saves.maintain(t.Context()) }()
		<-capture.entered
		time.Sleep(autosaveQuiet)
		if err := <-done; !errors.Is(err, errSavePreempted) {
			t.Fatalf("dirty deadline did not preempt maintenance: %v", err)
		}
		if saves.discovered == nil || len(client.discoveries) != 1 || !client.discoveries[0].BackgroundDue {
			t.Fatal("maintenance did not admit the due dirty cut")
		}
		saves.release()
	})
}

func TestAutosaveNewMutationKeepsEstablishedCompletionDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saves, base := agentSaveFixture(t, "")
		client := &accountingSaveClient{agentSaveTestClient: base, since: time.Now()}
		saves.client = client
		var state disk.VersionChanges
		saves.changes = func() disk.VersionChanges { return state }
		if err := saves.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		deadline := saves.accountingDue
		time.Sleep(autosaveQuiet - time.Millisecond)
		state = disk.VersionChanges{Generation: 1, ChangedAt: time.Now(), LastChangedAt: time.Now()}
		if err := saves.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if due := saves.nextSaveTime(state, time.Now()); !due.Equal(deadline) {
			t.Fatalf("new mutation postponed completion: %v != %v", due, deadline)
		}
		time.Sleep(time.Millisecond)
		if err := saves.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(base.requests) != 2 {
			t.Fatal("established completion deadline did not publish")
		}
		saves.release()
	})
}
