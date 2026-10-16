package computerhost

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
	"uuid"
)

type preemptSaveCapture struct {
	saveHostCapture
	entered  chan struct{}
	attempts atomic.Int32
}

func (c *preemptSaveCapture) Adopt(ctx context.Context) error {
	if c.attempts.Add(1) == 1 {
		close(c.entered)
		<-ctx.Done()
		return ctx.Err()
	}
	return c.saveHostCapture.Adopt(ctx)
}

type preemptSaveClient struct {
	*agentSaveTestClient
	next   *workerapi.AgentSave
	probed chan struct{}
}

func (c *preemptSaveClient) NextAgentSave(context.Context, workerapi.AgentSaveDiscovery) (workerapi.AgentSavePending, error) {
	if c.probed != nil {
		defer func() { c.probed <- struct{}{} }()
	}
	if c.next == nil {
		return workerapi.AgentSavePending{}, nil
	}
	save := c.next
	c.next = nil
	return workerapi.AgentSavePending{Save: save, Sequence: 1}, nil
}

func TestAgentSavePreemptsAdoptionForNewCapture(t *testing.T) {
	saves, base := agentSaveFixture(t, "")
	wake := make(chan struct{}, 1)
	saves.wake = wake
	client := &preemptSaveClient{agentSaveTestClient: base, next: &base.save}
	saves.client = client
	first := &preemptSaveCapture{saveHostCapture: saveHostCapture{base.fixture}, entered: make(chan struct{})}
	saves.machine.(*agentSaveTestMachine).capture = first
	if err := saves.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(saves.maintenance) != 1 || saves.pending != nil {
		t.Fatal("published capture was not queued for local maintenance")
	}
	done := make(chan error, 1)
	go func() { done <- saves.step(t.Context()) }()
	<-first.entered
	second := base.save
	second.SaveID = uuid.NewV7().String()
	client.next = &second
	wake <- struct{}{}
	if err := <-done; !errors.Is(err, errSavePreempted) {
		t.Fatal("adoption did not yield to wake", err)
	}
	saves.machine.(*agentSaveTestMachine).capture = saveHostCapture{base.fixture}
	if err := saves.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if first.attempts.Load() != 1 || len(saves.maintenance) != 1 {
		t.Fatal("new save waited behind old adoption")
	}
	for len(saves.maintenance) > 0 {
		if err := saves.maintain(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if first.attempts.Load() != 1 {
		t.Fatal("superseded adoption was retried")
	}
	saves.release()
}

type hintMaintenanceCapture struct {
	phase string
	saveHostCapture
	entered  chan struct{}
	release  chan struct{}
	canceled chan struct{}
	attempts atomic.Int32
}

func (c *hintMaintenanceCapture) wait(ctx context.Context) error {
	c.attempts.Add(1)
	close(c.entered)
	select {
	case <-c.release:
		return nil
	case <-ctx.Done():
		close(c.canceled)
		return ctx.Err()
	}
}
func (c *hintMaintenanceCapture) Adopt(ctx context.Context) error {
	if c.phase == "adopt" {
		if err := c.wait(ctx); err != nil {
			return err
		}
	}
	return c.saveHostCapture.Adopt(ctx)
}
func (c *hintMaintenanceCapture) Collect(ctx context.Context, limit int) (int64, error) {
	if c.phase == "collect" {
		if err := c.wait(ctx); err != nil {
			return 0, err
		}
	}
	return c.saveHostCapture.Collect(ctx, limit)
}

func TestAgentSaveGeneralHintsDoNotRestartMaintenance(t *testing.T) {
	for _, phase := range []string{"adopt", "collect"} {
		t.Run(phase, func(t *testing.T) {
			saves, base := agentSaveFixture(t, "")
			wake := make(chan struct{}, 1)
			saves.wake = wake
			client := &preemptSaveClient{agentSaveTestClient: base, next: &base.save, probed: make(chan struct{}, 16)}
			saves.client = client
			capture := &hintMaintenanceCapture{phase: phase, saveHostCapture: saveHostCapture{base.fixture}, entered: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
			saves.machine.(*agentSaveTestMachine).capture = capture
			if err := saves.step(t.Context()); err != nil {
				t.Fatal(err)
			}
			<-client.probed
			done := make(chan error, 1)
			go func() { done <- saves.step(t.Context()) }()
			<-capture.entered
			<-client.probed
			for range 3 {
				wake <- struct{}{}
				<-client.probed
				select {
				case <-capture.canceled:
					t.Fatal("general hint canceled maintenance without pending save")
				default:
				}
			}
			close(capture.release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if capture.attempts.Load() != 1 || len(saves.maintenance) != 0 || len(base.requests) != 2 {
				t.Fatal("unrelated work starved adoption or caused another capture")
			}
			saves.release()
		})
	}

}
