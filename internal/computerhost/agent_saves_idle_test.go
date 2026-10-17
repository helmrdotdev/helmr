package computerhost

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestAgentSaveIdleDoesNoSaveIO(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saves, client := agentSaveFixture(t, "")
		saves.client = &periodicAgentSaveClient{agentSaveTestClient: client}
		saves.wake = make(chan struct{})
		saves.changes = func() disk.VersionChanges { return disk.VersionChanges{} }
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		visits := 0
		saves.idle = func(context.Context) error {
			visits++
			if visits == 6 {
				cancel()
			}
			return nil
		}
		if err := saves.run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if len(client.fixture.steps) != 0 || len(client.requests) != 0 {
			t.Fatalf("idle did save IO: %v", client.fixture.steps)
		}
	})
}

func TestAgentSavePressureAdmitsEvenBeforeRejectedWriteChangesGeneration(t *testing.T) {
	saves, base := agentSaveFixture(t, "")
	client := &periodicAgentSaveClient{agentSaveTestClient: base}
	saves.client = client
	saves.changes = func() disk.VersionChanges { return disk.VersionChanges{PressureSince: time.Now()} }
	if err := saves.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(client.discoveries) != 1 || !client.discoveries[0].BackgroundDue || saves.pending != nil || saves.maintenance == nil {
		t.Fatal("pressure could not acquire object-retention authority before dirty admission")
	}
	saves.release()
}

func TestAgentSaveIdleBypassesDirtyQuietWaitAndKeepsDeadlineAfterRefusal(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(fmt.Sprint(refused), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				saves, base := agentSaveFixture(t, "")
				saves.client = &periodicAgentSaveClient{agentSaveTestClient: base}
				saves.wake = nil
				first := time.Now()
				saves.changes = func() disk.VersionChanges {
					return disk.VersionChanges{Generation: 1, ChangedAt: first, LastChangedAt: first}
				}
				visits := 0
				saves.idle = func(context.Context) error {
					visits++
					if visits == 1 {
						if !time.Now().Before(first.Add(autosaveQuiet)) || len(base.requests) != 0 {
							t.Fatal("idle preservation waited for the optional dirty save")
						}
						if refused {
							return nil
						}
					} else if len(base.requests) != 2 || !time.Now().Equal(first.Add(autosaveQuiet)) {
						t.Fatal("refused checkpoint changed the original autosave deadline")
					}
					return errComputerHibernated
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				if err := saves.run(ctx); !errors.Is(err, errComputerHibernated) {
					t.Fatal(err)
				}
				saves.release()
			})
		})
	}
}

type idleProofSaveClient struct {
	*accountingSaveClient
	published bool
}

func (c *idleProofSaveClient) NextAgentSave(ctx context.Context, request workerapi.AgentSaveDiscovery) (workerapi.AgentSavePending, error) {
	if c.published {
		return workerapi.AgentSavePending{}, nil
	}
	return c.accountingSaveClient.NextAgentSave(ctx, request)
}
func (c *idleProofSaveClient) PublishAgentSave(ctx context.Context, request workerapi.AgentSavePublication) error {
	if err := c.agentSaveTestClient.PublishAgentSave(ctx, request); err != nil {
		return err
	}
	c.published = true
	return nil
}

func TestAgentSaveIdleWaitsForScheduledCompletionProof(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saves, base := agentSaveFixture(t, "")
		first := time.Now()
		client := &idleProofSaveClient{accountingSaveClient: &accountingSaveClient{agentSaveTestClient: base, since: first}}
		saves.client = client
		saves.wake = nil
		saves.changes = func() disk.VersionChanges { return disk.VersionChanges{} }
		saves.idle = func(context.Context) error {
			if !client.published || saves.maintenance != nil || len(base.requests) != 2 || !time.Now().Equal(first.Add(autosaveQuiet)) {
				t.Fatal("idle bypassed scheduled completion proof or delayed after its maintenance")
			}
			return errComputerHibernated
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		if err := saves.run(ctx); !errors.Is(err, errComputerHibernated) {
			t.Fatal(err)
		}
		saves.release()
	})
}
