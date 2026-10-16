package computerhost

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type coalescedSaveCapture struct {
	saveHostCapture
	generation                       uint64
	releases, adoptions, collections int
	adoptErr, collectErr             error
}

func (c *coalescedSaveCapture) Changes() disk.VersionChanges {
	return disk.VersionChanges{Generation: c.generation}
}
func (c *coalescedSaveCapture) Release() { c.releases++ }
func (c *coalescedSaveCapture) Adopt(context.Context) error {
	c.adoptions++
	return c.adoptErr
}
func (c *coalescedSaveCapture) Collect(context.Context, int) (int64, error) {
	c.collections++
	return 0, c.collectErr
}

func TestAgentSaveMaintenanceKeepsOnlyConfirmedSuccessor(t *testing.T) {
	for _, state := range []string{"unadopted", "ambiguous adoption", "interrupted collection"} {
		t.Run(state, func(t *testing.T) {
			s, client := agentSaveFixture(t, "")
			s.wake = nil
			first := &coalescedSaveCapture{saveHostCapture: saveHostCapture{client.fixture}, generation: 1}
			s.machine.(*agentSaveTestMachine).capture = first
			if err := s.step(t.Context()); err != nil {
				t.Fatal(err)
			}
			if state != "unadopted" {
				if state == "ambiguous adoption" {
					first.adoptErr = errors.New("ambiguous head persistence")
				} else {
					first.collectErr = context.Canceled
				}
				if err := s.maintain(t.Context()); err == nil {
					t.Fatal("maintenance failure was not exercised")
				}
			}
			adoptions, collections := first.adoptions, first.collections
			candidate := s.retirementCandidate
			floor := &workerapi.AgentSaveRetirement{SaveID: uuid.NewV7().String(), Root: first.Root()}
			s.retirement = floor
			second := &coalescedSaveCapture{saveHostCapture: saveHostCapture{client.fixture}, generation: 2}
			s.machine.(*agentSaveTestMachine).capture = second
			client.save.SaveID = uuid.NewV7().String()
			// Use the already-discovered request so this tests replacement without
			// acknowledging the outstanding floor through a discovery response.
			s.discovered = &workerapi.AgentSavePending{Save: &client.save, Sequence: 2}
			client.fixture.fail, client.fixture.failed = "commit", false
			releases := first.releases
			if err := s.step(t.Context()); err == nil {
				t.Fatal("publication uncertainty was not exercised")
			}
			if first.releases != releases || len(s.maintenance) != 1 || s.maintenance[0].capture != first || second.releases != 0 {
				t.Fatal("uncertain publication replaced maintenance")
			}
			if err := s.step(t.Context()); err != nil {
				t.Fatal(err)
			}
			if first.releases != releases+1 || len(s.maintenance) != 1 || s.maintenance[0].capture != second || s.retirementCandidate != candidate || s.retirement != floor {
				t.Fatal("confirmed successor did not replace only local maintenance")
			}
			if state == "interrupted collection" {
				// After the prior floor is acknowledged, the released adopted
				// candidate still supplies its own exact receipt only once.
				s.retirement = nil
				s.prepareRetirement()
				if s.retirement == nil || s.retirement.SaveID != candidate.saveID || s.retirement.Root != first.Root() || s.retirementCandidate != nil {
					t.Fatal("released adopted candidate lost its exact retirement receipt")
				}
				floor = s.retirement
				s.prepareRetirement()
				if s.retirement != floor {
					t.Fatal("retirement receipt was produced twice")
				}
			}
			if err := s.maintain(t.Context()); err != nil {
				t.Fatal(err)
			}
			if first.adoptions != adoptions || first.collections != collections || second.adoptions != 1 || second.collections != 1 || len(s.maintenance) != 0 {
				t.Fatal("superseded maintenance ran again")
			}
			if s.retirement != floor || s.retirementCandidate == nil || s.retirementCandidate.capture != second {
				t.Fatal("new adoption discarded outstanding floor or lost candidate")
			}
		})
	}
}

func TestAgentSaveMaintenanceRemainsBoundedWithoutAdoption(t *testing.T) {
	s, client := agentSaveFixture(t, "")
	var previous *coalescedSaveCapture
	for generation := uint64(1); generation <= 100; generation++ {
		capture := &coalescedSaveCapture{saveHostCapture: saveHostCapture{client.fixture}, generation: generation}
		s.machine.(*agentSaveTestMachine).capture = capture
		client.save.SaveID = uuid.NewV7().String()
		if err := s.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(s.maintenance) != 1 || s.maintenance[0].capture != capture || capture.releases != 0 || capture.adoptions != 0 {
			t.Fatal("maintenance retained more than the latest publication")
		}
		if previous != nil && (previous.releases != 1 || previous.adoptions != 0) {
			t.Fatal("superseded pin retained or adopted")
		}
		previous = capture
	}
	s.release()
	if previous.releases != 1 {
		t.Fatal("final pin was not released on closure")
	}
}
