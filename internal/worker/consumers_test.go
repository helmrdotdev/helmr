package worker

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestRunConsumerSuppressesStaleTupleUntilDiscoveryNoLongerReturnsIt(t *testing.T) {
	work := workerapi.RunLeaseWork{LeaseID: "0198b960-7818-7a77-9d7d-4ebf163e15b1", LeaseSequence: 7}
	client := &runConsumerTestClient{response: workerapi.RunLeaseDiscoveryResponse{Items: []workerapi.RunLeaseWork{work}}}
	executor := &runConsumerTestExecutor{err: &httpclient.Error{StatusCode: http.StatusConflict, Status: "Conflict"}}
	consumer := NewRunConsumer(&Runner{
		client: client, runLeaseExecutor: executor,
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	claimed, ok, err := consumer.Claim(context.Background())
	if err != nil || !ok || claimed == nil {
		t.Fatalf("first claim = (%v, %t, %v), want work", claimed, ok, err)
	}
	if err := claimed(context.Background()); err != nil {
		t.Fatalf("execute stale work: %v", err)
	}
	if executor.calls != 1 {
		t.Fatalf("execute calls = %d, want 1", executor.calls)
	}

	if claimed, ok, err := consumer.Claim(context.Background()); err != nil || ok || claimed != nil {
		t.Fatalf("still-visible stale tuple claim = (%v, %t, %v), want no work", claimed, ok, err)
	}

	client.response.Items = nil
	if _, ok, err := consumer.Claim(context.Background()); err != nil || ok {
		t.Fatalf("absent tuple claim = (_, %t, %v), want no work", ok, err)
	}
	client.response.Items = []workerapi.RunLeaseWork{work}
	if claimed, ok, err := consumer.Claim(context.Background()); err != nil || !ok || claimed == nil {
		t.Fatalf("reappeared tuple claim = (%v, %t, %v), want work", claimed, ok, err)
	}
}

func TestRunConsumerSerializesDiscoveryAcrossExecutionSlots(t *testing.T) {
	client := &serializedRunConsumerTestClient{
		runConsumerTestClient: &runConsumerTestClient{},
		entered:               make(chan int, 2),
		release:               make(chan struct{}, 2),
	}
	consumer := NewRunConsumer(&Runner{
		client: client, runLeaseExecutor: &runConsumerTestExecutor{},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		_, _, _ = consumer.Claim(context.Background())
	}()
	if call := <-client.entered; call != 1 {
		t.Fatalf("first discovery call = %d, want 1", call)
	}

	secondStarted := make(chan struct{})
	secondDone := make(chan struct{})
	go func() {
		close(secondStarted)
		defer close(secondDone)
		_, _, _ = consumer.Claim(context.Background())
	}()
	<-secondStarted
	select {
	case call := <-client.entered:
		t.Fatalf("discovery call %d overlapped the first call", call)
	case <-time.After(50 * time.Millisecond):
	}

	client.release <- struct{}{}
	<-firstDone
	if call := <-client.entered; call != 2 {
		t.Fatalf("second discovery call = %d, want 2", call)
	}
	client.release <- struct{}{}
	<-secondDone
	if maxActive := client.maxActiveDiscoveries(); maxActive != 1 {
		t.Fatalf("maximum concurrent discoveries = %d, want 1", maxActive)
	}
}

type runConsumerTestClient struct {
	response workerapi.RunLeaseDiscoveryResponse
}

type serializedRunConsumerTestClient struct {
	*runConsumerTestClient
	entered chan int
	release chan struct{}
	mu      sync.Mutex
	calls   int
	active  int
	max     int
}

func (c *serializedRunConsumerTestClient) DiscoverRunLeases(context.Context) (workerapi.RunLeaseDiscoveryResponse, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.active++
	if c.active > c.max {
		c.max = c.active
	}
	c.mu.Unlock()
	c.entered <- call
	<-c.release
	c.mu.Lock()
	c.active--
	c.mu.Unlock()
	return workerapi.RunLeaseDiscoveryResponse{}, nil
}

func (c *serializedRunConsumerTestClient) maxActiveDiscoveries() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.max
}

func (c *runConsumerTestClient) DiscoverRunLeases(context.Context) (workerapi.RunLeaseDiscoveryResponse, error) {
	return c.response, nil
}

func (*runConsumerTestClient) ClaimWorkspaceMount(context.Context, workerapi.Capabilities) (workerapi.WorkspaceMountClaimResponse, error) {
	return workerapi.WorkspaceMountClaimResponse{}, nil
}

func (*runConsumerTestClient) RenewWorkspaceMount(context.Context, workerapi.WorkspaceMountRenewRequest) (workerapi.WorkspaceMountResponse, error) {
	return workerapi.WorkspaceMountResponse{}, nil
}

func (*runConsumerTestClient) MarkWorkspaceMountMounted(context.Context, workerapi.WorkspaceMountMountedRequest) (workerapi.WorkspaceMountResponse, error) {
	return workerapi.WorkspaceMountResponse{}, nil
}

func (*runConsumerTestClient) CaptureWorkspaceMount(context.Context, workerapi.WorkspaceMountCaptureRequest) (workerapi.WorkspaceMountCaptureResponse, error) {
	return workerapi.WorkspaceMountCaptureResponse{}, nil
}

func (*runConsumerTestClient) StopWorkspaceMount(context.Context, workerapi.WorkspaceMountStopRequest) (workerapi.WorkspaceMountResponse, error) {
	return workerapi.WorkspaceMountResponse{}, nil
}

func (*runConsumerTestClient) FailWorkspaceMount(context.Context, workerapi.WorkspaceMountFailRequest) (workerapi.WorkspaceMountResponse, error) {
	return workerapi.WorkspaceMountResponse{}, nil
}

func (*runConsumerTestClient) ClaimWorkspaceExec(context.Context, workerapi.WorkspaceExecClaimRequest) (workerapi.WorkspaceExecClaimResponse, error) {
	return workerapi.WorkspaceExecClaimResponse{}, nil
}

func (*runConsumerTestClient) CompleteWorkspaceExec(context.Context, workerapi.WorkspaceExecCompleteRequest) (workerapi.WorkspaceMountResponse, error) {
	return workerapi.WorkspaceMountResponse{}, nil
}

type runConsumerTestExecutor struct {
	calls int
	err   error
}

func (e *runConsumerTestExecutor) ExecuteRunLease(context.Context, workerapi.RunLeaseWork) error {
	e.calls++
	return e.err
}
