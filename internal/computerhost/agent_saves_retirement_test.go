package computerhost

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type retirementSaveCapture struct {
	saveHostCapture
	ready bool
}

func (c *retirementSaveCapture) ReadyForRetirement() bool { return c.ready }

type retirementSaveClient struct {
	*agentSaveTestClient
	discoveries []workerapi.AgentSaveDiscovery
	lose        bool
}

func (c *retirementSaveClient) NextAgentSave(_ context.Context, r workerapi.AgentSaveDiscovery) (workerapi.AgentSavePending, error) {
	if r.Retirement != nil {
		floor := *r.Retirement
		r.Retirement = &floor
	}
	c.discoveries = append(c.discoveries, r)
	if c.lose {
		c.lose = false
		return workerapi.AgentSavePending{}, errors.New("lost discovery response")
	}
	return workerapi.AgentSavePending{}, nil
}
func TestAgentSaveRetirementWaitsForAdoptionAndRetriesExactFloor(t *testing.T) {
	s, base := agentSaveFixture(t, "")
	capture := &retirementSaveCapture{saveHostCapture: saveHostCapture{base.fixture}}
	s.machine.(*agentSaveTestMachine).capture = capture
	if err := s.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.prepareRetirement()
	if s.retirement != nil {
		t.Fatal("unadopted publication announced")
	}
	// No discovery hint may race this deterministic maintenance completion.
	s.wake = nil
	client := &retirementSaveClient{agentSaveTestClient: base}
	s.client = client
	if err := s.maintain(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.prepareRetirement()
	if s.retirement != nil {
		t.Fatal("busy readiness announced")
	}
	capture.ready = true
	client.lose = true
	if err := s.step(t.Context()); err == nil {
		t.Fatal("lost ACK not exercised")
	}
	if err := s.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(client.discoveries) != 2 || client.discoveries[0].Retirement == nil || !reflect.DeepEqual(client.discoveries[0].Retirement, client.discoveries[1].Retirement) {
		t.Fatalf("uncertain exact floor changed: %+v", client.discoveries)
	}
	if err := s.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(client.discoveries) != 3 || client.discoveries[2].Retirement != nil {
		t.Fatal("acknowledged floor was sent again")
	}
	if got := client.discoveries[0].Retirement; got.SaveID != base.save.SaveID || got.Root != capture.Root() {
		t.Fatal("floor differs from adopted save")
	}
}

func TestAgentSaveCanceledAdoptionAndCachedDiscoveryKeepUnacknowledgedFloor(t *testing.T) {
	saves, base := agentSaveFixture(t, "")
	wake := make(chan struct{}, 1)
	saves.wake = wake
	client := &preemptSaveClient{agentSaveTestClient: base, next: &base.save}
	saves.client = client
	capture := &preemptSaveCapture{saveHostCapture: saveHostCapture{base.fixture}, entered: make(chan struct{})}
	saves.machine.(*agentSaveTestMachine).capture = capture
	if err := saves.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	oldFloor := &workerapi.AgentSaveRetirement{SaveID: uuid.NewV7().String(), Root: capture.Root()}
	saves.retirement = oldFloor
	done := make(chan error, 1)
	go func() { done <- saves.maintain(t.Context()) }()
	<-capture.entered
	next := base.save
	next.SaveID = uuid.NewV7().String()
	client.next = &next
	wake <- struct{}{}
	if err := <-done; !errors.Is(err, errSavePreempted) {
		t.Fatalf("adoption was not canceled by durable next save: %v", err)
	}
	saves.prepareRetirement()
	if saves.retirementCandidate != nil || saves.retirement != oldFloor {
		t.Fatal("canceled adoption announced a candidate or discarded uncertain floor")
	}
	if saves.discovered == nil {
		t.Fatal("preemption did not retain discovered request")
	}
	saves.machine.(*agentSaveTestMachine).capture = saveHostCapture{base.fixture}
	if err := saves.step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if saves.discovered != nil || saves.retirement != oldFloor {
		t.Fatal("cached discovery consumption acknowledged an unsent floor")
	}
	saves.release()
}
