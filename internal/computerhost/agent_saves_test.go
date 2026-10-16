package computerhost

import (
	"context"
	"errors"
	"github.com/aws/smithy-go"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
	"io"
	"reflect"
	"testing"
	"testing/synctest"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type agentSaveTestClient struct {
	AgentSaveClient
	fixture  *saveHostFixture
	save     workerapi.AgentSave
	requests []workerapi.AgentSavePublication
}

func (c *agentSaveTestClient) NextAgentSave(context.Context, workerapi.AgentSaveDiscovery) (workerapi.AgentSavePending, error) {
	return workerapi.AgentSavePending{Save: &c.save, Sequence: 1}, c.fixture.step("discover")
}
func (c *agentSaveTestClient) CaptureAgentSave(_ context.Context, r workerapi.AgentSavePublication) error {
	c.requests = append(c.requests, r)
	return c.fixture.step("record")
}
func (c *agentSaveTestClient) PublishAgentSave(_ context.Context, r workerapi.AgentSavePublication) error {
	c.requests = append(c.requests, r)
	return c.fixture.step("commit")
}

type agentSaveTestMachine struct {
	liveCaptureMachine
	fixture    *saveHostFixture
	captureErr error
	capture    computerSaveCapture
}

func (m *agentSaveTestMachine) CaptureComputer(context.Context) (*vm.ComputerSnapshot, error) {
	if err := m.fixture.step("capture"); err != nil {
		return nil, err
	}
	if m.captureErr != nil {
		return nil, m.captureErr
	}
	capture := m.capture
	if capture == nil {
		capture = saveHostCapture{m.fixture}
	}
	return &vm.ComputerSnapshot{ComputerID: m.fixture.computer, Capture: capture}, nil
}
func agentSaveFixture(t *testing.T, failure string) (*agentComputerSaves, *agentSaveTestClient) {
	t.Helper()
	f := &saveHostFixture{fail: failure, computer: uuid.NewV7().String(), root: testVersionRoot(4096)}
	identity := workerapi.AllocationIdentity{Kind: "computer", OwnerID: f.computer, EnvironmentID: uuid.NewV7().String(), AllocationID: uuid.NewV7().String(), Epoch: 2}
	c := &agentSaveTestClient{fixture: f, save: workerapi.AgentSave{EnvironmentID: identity.EnvironmentID, SaveID: uuid.NewV7().String(), LeaseEpoch: 2}}
	wake := make(chan struct{}, 1)
	wake <- struct{}{}
	return &agentComputerSaves{pinCommitted: func(context.Context) (disk.RetainedVersion, error) { return nil, errors.New("unexpected pressure pin") }, wake: wake, changes: func() disk.VersionChanges { return disk.VersionChanges{Generation: 1} }, client: c, objects: f, identity: identity, machine: &agentSaveTestMachine{fixture: f}}, c
}
func TestAgentSaveRetryRetainsExactCut(t *testing.T) {
	for _, failure := range []string{"record", "objects", "commit", "adopt"} {
		t.Run(failure, func(t *testing.T) {
			s, c := agentSaveFixture(t, failure)
			if err := saveTestStep(s, t.Context()); err == nil {
				t.Fatal("missing injected uncertainty")
			}
			if failure == "record" || failure == "objects" || failure == "commit" {
				for _, step := range c.fixture.steps {
					if step == "adopt" {
						t.Fatal("adopted before exact head publication was confirmed")
					}
				}
			}
			if (s.capture == nil || s.pending == nil) && len(s.maintenance) == 0 {
				t.Fatal("uncertain cut was discarded")
			}
			if err := saveTestStep(s, t.Context()); err != nil {
				t.Fatal(err)
			}
			captures, discoveries, releases := 0, 0, 0
			for _, step := range c.fixture.steps {
				switch step {
				case "capture":
					captures++
				case "discover":
					discoveries++
				case "release":
					releases++
				}
			}
			if captures != 1 || discoveries != 1 || releases != 1 || s.pending != nil {
				t.Fatalf("lost cut ownership: %v", c.fixture.steps)
			}
			for _, r := range c.requests {
				if !reflect.DeepEqual(r, c.requests[0]) {
					t.Fatal("capture receipt changed across uncertain retry")
				}
			}
		})
	}
}
func TestAgentSavePhysicalUncertaintyRetriesSameAdmittedPrefix(t *testing.T) {
	s, c := agentSaveFixture(t, "")
	s.machine.(*agentSaveTestMachine).captureErr = io.ErrUnexpectedEOF
	for range 2 {
		if err := saveTestStep(s, t.Context()); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("capture uncertainty: %v", err)
		}
	}
	if !reflect.DeepEqual(c.fixture.steps, []string{"discover", "capture", "capture"}) {
		t.Fatalf("rediscovered ambiguous prefix: %v", c.fixture.steps)
	}
	if len(c.requests) != 0 {
		t.Fatal("unproved capture reached Control Plane")
	}
}

type retryAgentSaveCapture struct {
	saveHostCapture
	stage    string
	attempts int
}

func (c *retryAgentSaveCapture) outage(stage string) error {
	if stage != c.stage {
		return nil
	}
	c.attempts++
	if c.attempts == 1 {
		return errors.Join(cas.ErrUnavailable, &smithy.GenericAPIError{Code: "SlowDown", Message: "503 provider unavailable"})
	}
	return nil
}
func (c *retryAgentSaveCapture) Publish(ctx context.Context, publication disk.ContinuationPublication) error {
	if err := c.outage("upload"); err != nil {
		return err
	}
	return c.saveHostCapture.Publish(ctx, publication)
}
func (c *retryAgentSaveCapture) Adopt(ctx context.Context) error {
	if err := c.outage("adopt"); err != nil {
		return err
	}
	return c.saveHostCapture.Adopt(ctx)
}
func (c *retryAgentSaveCapture) Collect(ctx context.Context, limit int) (int64, error) {
	if err := c.outage("collect"); err != nil {
		return 0, err
	}
	return c.saveHostCapture.Collect(ctx, limit)
}

type retryAgentSaveClient struct {
	*agentSaveTestClient
	cancel     context.CancelFunc
	discovered int
	settled    func() bool
}

func (c *retryAgentSaveClient) NextAgentSave(ctx context.Context, identity workerapi.AgentSaveDiscovery) (workerapi.AgentSavePending, error) {
	c.discovered++
	if c.discovered > 1 {
		if c.settled == nil || c.settled() {
			c.cancel()
			return workerapi.AgentSavePending{}, context.Canceled
		}
		return workerapi.AgentSavePending{}, nil
	}
	return c.agentSaveTestClient.NextAgentSave(ctx, identity)
}
func TestAgentSaveStorageOutageRetriesWhileOwnerLives(t *testing.T) {
	for _, stage := range []string{"upload", "adopt", "collect"} {
		t.Run(stage, func(t *testing.T) {
			s, c := agentSaveFixture(t, "")
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			capture := &retryAgentSaveCapture{saveHostCapture: saveHostCapture{c.fixture}, stage: stage}
			s.machine.(*agentSaveTestMachine).capture = capture
			s.client = &retryAgentSaveClient{agentSaveTestClient: c, cancel: cancel, settled: func() bool { return len(s.maintenance) == 0 }}
			if err := s.run(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("save loop stopped during storage outage: %v", err)
			}
			if capture.attempts != 2 || s.pending != nil {
				t.Fatalf("cut was not retried to completion: attempts=%d pending=%+v", capture.attempts, s.pending)
			}
			captures, commits := 0, 0
			for _, step := range c.fixture.steps {
				if step == "capture" {
					captures++
				}
				if step == "commit" {
					commits++
				}
			}
			if captures != 1 || commits != 1 {
				t.Fatalf("storage outage repeated disk capture/publication: %v", c.fixture.steps)
			}
		})
	}
}

func TestAgentSaveLoopDoesNotRetryAnUncertainCheckpoint(t *testing.T) {
	s, _ := agentSaveFixture(t, "")
	s.changes = func() disk.VersionChanges { return disk.VersionChanges{} }
	s.client = &periodicAgentSaveClient{}
	attempts := 0
	s.idle = func(context.Context) error { attempts++; return io.ErrUnexpectedEOF }
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := s.run(ctx); !errors.Is(err, io.ErrUnexpectedEOF) || attempts != 1 {
		t.Fatalf("checkpoint retry escaped its owner: %v attempts=%d", err, attempts)
	}
}

type periodicAgentSaveClient struct {
	*agentSaveTestClient
	discoveries       []workerapi.AgentSaveDiscovery
	decline, required bool
	onDiscovery       func()
}

func (c *periodicAgentSaveClient) NextAgentSave(ctx context.Context, r workerapi.AgentSaveDiscovery) (workerapi.AgentSavePending, error) {
	c.discoveries = append(c.discoveries, r)
	if c.onDiscovery != nil {
		c.onDiscovery()
	}
	if c.required || (r.BackgroundDue && !c.decline) {
		return c.agentSaveTestClient.NextAgentSave(ctx, r)
	}
	return workerapi.AgentSavePending{}, nil
}
func TestAgentSaveDeclinedBackgroundBacksOffWithoutClaimingPreservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saves, base := agentSaveFixture(t, "")
		client := &periodicAgentSaveClient{agentSaveTestClient: base, decline: true}
		saves.client = client
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		client.onDiscovery = func() {
			if len(client.discoveries) == 3 {
				cancel()
			}
		}
		idle := 0
		saves.idle = func(context.Context) error { idle++; return nil }
		if err := saves.run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if idle == 0 || len(client.discoveries) != 3 || saves.savedGeneration != 0 || len(base.requests) != 0 {
			t.Fatalf("lost idle qualification, invented preservation or admission spin: idle=%d discovery=%d", idle, len(client.discoveries))
		}
		for i, want := range []bool{true, false, true} {
			if client.discoveries[i].BackgroundDue != want {
				t.Fatalf("discovery %d due=%t", i, client.discoveries[i].BackgroundDue)
			}
		}
	})
}

func saveTestStep(s *agentComputerSaves, ctx context.Context) error {
	wake := s.wake
	s.wake = nil
	defer func() { s.wake = wake }()
	if len(s.maintenance) > 0 {
		return s.maintain(ctx)
	}
	if err := s.step(ctx); err != nil {
		return err
	}
	for len(s.maintenance) > 0 {
		if err := s.maintain(ctx); err != nil {
			return err
		}
	}
	return nil
}
