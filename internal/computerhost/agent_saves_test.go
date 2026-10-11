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
	identity := workerapi.AllocationIdentity{Kind: "computer", OwnerID: f.computer, EnvironmentID: uuid.NewV7().String(), InstanceID: uuid.NewV7().String(), Epoch: 2}
	c := &agentSaveTestClient{fixture: f, save: workerapi.AgentSave{EnvironmentID: identity.EnvironmentID, SaveID: uuid.NewV7().String(), LeaseEpoch: 2}}
	return &agentComputerSaves{client: c, objects: f, identity: identity, every: time.Minute, machine: &agentSaveTestMachine{fixture: f}}, c
}
func TestAgentSaveRetryRetainsExactCut(t *testing.T) {
	for _, failure := range []string{"record", "objects", "commit", "adopt"} {
		t.Run(failure, func(t *testing.T) {
			s, c := agentSaveFixture(t, failure)
			if err := s.step(t.Context()); err == nil {
				t.Fatal("missing injected uncertainty")
			}
			if s.capture == nil || s.pending == nil {
				t.Fatal("uncertain cut was discarded")
			}
			if err := s.step(t.Context()); err != nil {
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
func TestAgentSavePhysicalUncertaintyNeverRecaptures(t *testing.T) {
	s, c := agentSaveFixture(t, "")
	s.machine.(*agentSaveTestMachine).captureErr = io.ErrUnexpectedEOF
	for range 2 {
		if err := s.step(t.Context()); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("capture uncertainty: %v", err)
		}
	}
	if !reflect.DeepEqual(c.fixture.steps, []string{"discover", "capture"}) {
		t.Fatalf("recaptured ambiguous cut: %v", c.fixture.steps)
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
func (c *retryAgentSaveCapture) Adopt(ctx context.Context, limit int) error {
	if err := c.outage("adopt"); err != nil {
		return err
	}
	return c.saveHostCapture.Adopt(ctx, limit)
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
}

func (c *retryAgentSaveClient) NextAgentSave(ctx context.Context, identity workerapi.AgentSaveDiscovery) (workerapi.AgentSavePending, error) {
	c.discovered++
	if c.discovered > 1 {
		c.cancel()
		return workerapi.AgentSavePending{}, context.Canceled
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
			s.client = &retryAgentSaveClient{agentSaveTestClient: c, cancel: cancel}
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
}

func (c *periodicAgentSaveClient) NextAgentSave(ctx context.Context, r workerapi.AgentSaveDiscovery) (workerapi.AgentSavePending, error) {
	c.discoveries = append(c.discoveries, r)
	if c.required || (r.BackgroundDue && !c.decline) {
		return c.agentSaveTestClient.NextAgentSave(ctx, r)
	}
	return workerapi.AgentSavePending{}, nil
}
func TestAgentSavePeriodicTriggerAndPublicationReset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, base := agentSaveFixture(t, "")
		client := &periodicAgentSaveClient{agentSaveTestClient: base}
		s.client = client
		step := func(wantDue bool) {
			t.Helper()
			if err := s.step(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := client.discoveries[len(client.discoveries)-1].BackgroundDue; got != wantDue {
				t.Fatalf("due=%v want=%v", got, wantDue)
			}
		}
		step(false)
		time.Sleep(time.Minute - time.Nanosecond)
		step(false)
		time.Sleep(time.Nanosecond)
		step(true)
		time.Sleep(30 * time.Second)
		client.required = true
		step(false)
		client.required = false
		time.Sleep(30 * time.Second)
		step(false)
		time.Sleep(30 * time.Second)
		step(true)
	})
}
func TestAgentSaveDeclinedBackgroundStillRunsIdleAndThrottlesAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, base := agentSaveFixture(t, "")
		client := &periodicAgentSaveClient{agentSaveTestClient: base, decline: true}
		s.client = client
		s.nextBackground = time.Now()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		idle := 0
		s.idle = func(context.Context) error {
			idle++
			if idle == 6 {
				cancel()
			}
			return nil
		}
		if err := s.run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		expected := []bool{true, false, false, false, true, false}
		if len(client.discoveries) != len(expected) || idle != 6 {
			t.Fatalf("idle=%d discoveries=%d", idle, len(client.discoveries))
		}
		for i, request := range client.discoveries {
			if request.BackgroundDue != expected[i] {
				t.Fatalf("attempt %d due=%v", i, request.BackgroundDue)
			}
		}
	})
}
func TestAgentSaveUncertainPublicationKeepsCutAndResetsOnlyOnAcknowledgement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, base := agentSaveFixture(t, "commit")
		client := &periodicAgentSaveClient{agentSaveTestClient: base}
		s.client = client
		s.nextBackground = time.Now()
		if err := s.step(t.Context()); err == nil {
			t.Fatal("expected uncertain publication")
		}
		original := s.pending.Save.SaveID
		time.Sleep(10 * time.Second)
		if err := s.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(client.discoveries) != 1 || s.pending != nil || !s.nextBackground.Equal(time.Now().Add(s.every)) {
			t.Fatalf("uncertain cut replaced or timer differs: %s %+v", original, s)
		}
		time.Sleep(s.every - time.Nanosecond)
		if err := s.step(t.Context()); err != nil {
			t.Fatal(err)
		}
		if client.discoveries[len(client.discoveries)-1].BackgroundDue {
			t.Fatal("timer used capture time instead of publication acknowledgement")
		}
	})
}
