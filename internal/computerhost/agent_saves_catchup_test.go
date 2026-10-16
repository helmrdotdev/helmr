package computerhost

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type catchupSaveClient struct {
	*agentSaveTestClient
	mu          sync.Mutex
	next        *workerapi.AgentSave
	discoveries []workerapi.AgentSaveDiscovery
}

func (c *catchupSaveClient) NextAgentSave(_ context.Context, request workerapi.AgentSaveDiscovery) (workerapi.AgentSavePending, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.discoveries = append(c.discoveries, request)
	save := c.next
	c.next = nil
	return workerapi.AgentSavePending{Save: save, Sequence: 1}, nil
}

func TestAgentSaveMissedHintFindsPendingWhileClean(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saves, base := agentSaveFixture(t, "")
		client := &catchupSaveClient{agentSaveTestClient: base, next: &base.save}
		saves.client = client
		saves.wake = make(chan struct{})
		saves.changes = func() disk.VersionChanges { return disk.VersionChanges{} }
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- saves.run(ctx) }()
		time.Sleep(saveDiscoveryCatchupEvery + time.Second)
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		defer saves.release()
		if len(client.requests) != 2 {
			t.Fatalf("missed hint did not publish pending save: %v", base.fixture.steps)
		}
		for _, request := range client.discoveries {
			if request.BackgroundDue {
				t.Fatal("clean catch-up admitted optional save")
			}
		}
	})
}

func TestAgentSaveMaintenanceCatchupOnlyPreemptsForPending(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "pending"}[pending], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				saves, base := agentSaveFixture(t, "")
				client := &catchupSaveClient{agentSaveTestClient: base}
				if pending {
					client.next = &base.save
				}
				saves.client, saves.wake = client, make(chan struct{})
				saves.changes = func() disk.VersionChanges { return disk.VersionChanges{} }
				capture := &hintMaintenanceCapture{phase: "adopt", saveHostCapture: saveHostCapture{base.fixture}, entered: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
				saves.maintenance = []*saveMaintenance{{capture: capture, saveID: base.save.SaveID}}
				defer saves.release()
				done := make(chan error, 1)
				go func() { done <- saves.maintain(t.Context()) }()
				<-capture.entered
				time.Sleep(saveDiscoveryCatchupEvery + time.Second)
				if !pending {
					close(capture.release)
				}
				err := <-done
				if pending && !errors.Is(err, errSavePreempted) {
					t.Fatal("pending work did not preempt", err)
				}
				if !pending && err != nil {
					t.Fatal(err)
				}
				if capture.attempts.Load() != 1 || len(client.discoveries) == 0 || client.discoveries[0].BackgroundDue {
					t.Fatal("maintenance catch-up restarted or admitted background work")
				}
			})
		})
	}
}
