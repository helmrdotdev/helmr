package executor

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type loopSaveClient struct {
	*saveHostFixture
	begun  chan workerapi.ComputerSaveBeginRequest
	reject atomic.Bool
}

func (c *loopSaveClient) BeginComputerSave(ctx context.Context, r workerapi.ComputerSaveBeginRequest) (workerapi.ComputerSaveBeginResponse, error) {
	c.begun <- r
	if c.reject.CompareAndSwap(true, false) {
		return workerapi.ComputerSaveBeginResponse{}, &httpclient.Error{StatusCode: http.StatusConflict}
	}
	return c.saveHostFixture.BeginComputerSave(ctx, r)
}
func nextSaveRequest(t *testing.T, c *loopSaveClient) workerapi.ComputerSaveBeginRequest {
	t.Helper()
	select {
	case r := <-c.begun:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("save admission did not arrive")
		return workerapi.ComputerSaveBeginRequest{}
	}
}
func TestRuntimeComputerSaveLoopUsesActiveChildAuthority(t *testing.T) {
	s := &runtimeComputerSaves{}
	f := &saveHostFixture{runtime: uuid.NewV7().String(), computer: uuid.NewV7().String()}
	c := &loopSaveClient{saveHostFixture: f, begun: make(chan workerapi.ComputerSaveBeginRequest, 16)}
	_, err := s.attach(f.runtime, f.computer, func() *workerapi.ComputerSaveBeginRequest { return nil })
	if err != nil {
		t.Fatal(err)
	}
	child := workerapi.RunLeaseFence{ID: uuid.NewV7().String(), LeaseSequence: 2}
	detach, err := s.attach(f.runtime, f.computer, func() *workerapi.ComputerSaveBeginRequest { return &workerapi.ComputerSaveBeginRequest{Lease: &child} })
	if err != nil {
		t.Fatal(err)
	}
	capture := func(context.Context) (computerSaveCapture, error) { return saveHostCapture{f}, nil }
	failures := make(chan error, 1)
	result, err := s.run(t.Context(), time.Millisecond, c, f, capture, func(err error) { failures <- err })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.run(t.Context(), time.Millisecond, c, f, capture, func(error) {}); err == nil {
		t.Fatal("duplicate loop started")
	}
	first := nextSaveRequest(t, c)
	if first.Lease == nil || *first.Lease != child {
		t.Fatal("used waiting parent's authority")
	}
	detach()
	detach()
	if err := s.Quiesce(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-result
	select {
	case err := <-failures:
		t.Fatalf("normal close reported failure: %v", err)
	default:
	}
	if request, _, _ := s.authority(); request != nil {
		t.Fatal("stopped owner yielded authority")
	}
}

func TestRuntimeComputerSaveLoopReusesRejectedSequence(t *testing.T) {
	s := &runtimeComputerSaves{}
	f := &saveHostFixture{runtime: uuid.NewV7().String(), computer: uuid.NewV7().String()}
	c := &loopSaveClient{saveHostFixture: f, begun: make(chan workerapi.ComputerSaveBeginRequest, 16)}
	c.reject.Store(true)
	_, err := s.attach(f.runtime, f.computer, func() *workerapi.ComputerSaveBeginRequest {
		return &workerapi.ComputerSaveBeginRequest{OrgID: uuid.NewV7().String(), WorkspaceMountID: uuid.NewV7().String()}
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.run(t.Context(), time.Millisecond, c, f, func(context.Context) (computerSaveCapture, error) { return saveHostCapture{f}, nil }, func(err error) { t.Errorf("loop failure: %v", err) })
	if err != nil {
		t.Fatal(err)
	}
	first, second := nextSaveRequest(t, c), nextSaveRequest(t, c)
	if first.Sequence != 1 || second.Sequence != 1 || first.SaveID == second.SaveID {
		t.Fatalf("rejection consumed sequence: %+v %+v", first, second)
	}
	if err := s.Quiesce(t.Context()); err != nil {
		t.Fatal(err)
	}
	<-result
}

func TestRuntimeComputerSaveLoopSignalsUnrecoverableCapture(t *testing.T) {
	s := &runtimeComputerSaves{}
	f := &saveHostFixture{runtime: uuid.NewV7().String(), computer: uuid.NewV7().String()}
	_, err := s.attach(f.runtime, f.computer, func() *workerapi.ComputerSaveBeginRequest { return &workerapi.ComputerSaveBeginRequest{} })
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("ambiguous VMM pause")
	notified := make(chan error, 1)
	result, err := s.run(t.Context(), time.Millisecond, f, f, func(context.Context) (computerSaveCapture, error) { return nil, failure }, func(err error) { notified <- err })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-notified:
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed save did not stop execution owner")
	}
	if err := <-result; !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := s.Quiesce(t.Context()); !errors.Is(err, failure) {
		t.Fatal("lost unresolved source error")
	}
}

func TestRuntimeComputerSaveLoopObservesFailureWithoutAnotherTick(t *testing.T) {
	s := &runtimeComputerSaves{}
	f := &saveHostFixture{runtime: uuid.NewV7().String(), computer: uuid.NewV7().String()}
	_, err := s.attach(f.runtime, f.computer, func() *workerapi.ComputerSaveBeginRequest { return &workerapi.ComputerSaveBeginRequest{} })
	if err != nil {
		t.Fatal(err)
	}
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	failure := errors.New("resume failed")
	result := make(chan error, 1)
	go func() {
		result <- s.loop(t.Context(), ticks, f, f, func(context.Context) (computerSaveCapture, error) { return nil, failure })
	}()
	select {
	case err := <-result:
		if !errors.Is(err, failure) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failure waited for a second tick")
	}
}
