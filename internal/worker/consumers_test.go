package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestRunConsumerSuppressesStaleTupleUntilDiscoveryNoLongerReturnsIt(t *testing.T) {
	work := workerapi.RunLeaseWork{LeaseID: "0198b960-7818-7a77-9d7d-4ebf163e15b1", LeaseSequence: 7}
	client := &runConsumerTestClient{response: workerapi.RunLeaseDiscoveryResponse{Items: []workerapi.RunLeaseWork{work}}}
	executor := &runConsumerTestExecutor{err: &httpclient.Error{
		StatusCode: http.StatusConflict, Status: "Conflict", Code: "run_start_stale",
		Details: json.RawMessage(`{"point":"runtime"}`),
	}}
	var logs bytes.Buffer
	consumer := NewRunConsumer(&Runner{
		client: client, runLeaseExecutor: executor,
		log: slog.New(slog.NewTextHandler(&logs, nil)),
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
	if !strings.Contains(logs.String(), "code=run_start_stale") ||
		!strings.Contains(logs.String(), `details="{\"point\":\"runtime\"}"`) {
		t.Fatalf("stale authority log omitted Control diagnostics: %s", logs.String())
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

type runConsumerTestClient struct {
	response workerapi.RunLeaseDiscoveryResponse
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
	work  []workerapi.RunLeaseWork
	err   error
}

func (e *runConsumerTestExecutor) ExecuteRunLease(_ context.Context, work workerapi.RunLeaseWork) error {
	e.calls++
	e.work = append(e.work, work)
	return e.err
}

type discoveryRunConsumerClient struct {
	*runConsumerTestClient
	discover func(context.Context) (workerapi.RunLeaseDiscoveryResponse, error)
}

func (c *discoveryRunConsumerClient) DiscoverRunLeases(ctx context.Context) (workerapi.RunLeaseDiscoveryResponse, error) {
	return c.discover(ctx)
}

type runClaimResult struct {
	work Work
	ok   bool
	err  error
}

func claimRunForTest(ctx context.Context, consumer Consumer, results chan<- runClaimResult) {
	work, ok, err := consumer.Claim(ctx)
	results <- runClaimResult{work, ok, err}
}

func TestRunConsumerSharesConcurrentDiscovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		a := workerapi.RunLeaseWork{LeaseID: "lease-a", LeaseSequence: 1}
		b := workerapi.RunLeaseWork{LeaseID: "lease-b", LeaseSequence: 2}
		response := workerapi.RunLeaseDiscoveryResponse{Items: []workerapi.RunLeaseWork{a, a, b}}
		release := make(chan struct{})
		calls := 0
		client := &discoveryRunConsumerClient{discover: func(ctx context.Context) (workerapi.RunLeaseDiscoveryResponse, error) {
			calls++
			select {
			case <-ctx.Done():
				return workerapi.RunLeaseDiscoveryResponse{}, ctx.Err()
			case <-release:
				return response, nil
			}
		}}
		executor := &runConsumerTestExecutor{}
		consumer := NewRunConsumer(&Runner{client: client, runLeaseExecutor: executor})
		results := make(chan runClaimResult, 3)
		for range 3 {
			go claimRunForTest(ctx, consumer, results)
		}
		synctest.Wait()
		if calls != 1 {
			t.Fatalf("concurrent discovery calls = %d, want 1", calls)
		}
		close(release)
		synctest.Wait()
		selected := 0
		for range 3 {
			got := <-results
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.ok {
				selected++
				if err := got.work(ctx); err != nil {
					t.Fatal(err)
				}
			} else if got.work != nil {
				t.Fatal("unclaimed work returned")
			}
		}
		if selected != 2 || executor.calls != 2 {
			t.Fatalf("selected=%d executed=%d, want 2", selected, executor.calls)
		}
		if executor.work[0] == executor.work[1] ||
			(executor.work[0] != a && executor.work[0] != b) ||
			(executor.work[1] != a && executor.work[1] != b) {
			t.Fatalf("executed tuples = %+v", executor.work)
		}
		t.Logf("three concurrent claims: discoveries=%d selected=%d executed=%d", calls, selected, executor.calls)
		response.Items = []workerapi.RunLeaseWork{{LeaseID: "fresh", LeaseSequence: 3}}
		work, ok, err := consumer.Claim(ctx)
		if err != nil || !ok || work == nil || calls != 2 {
			t.Fatalf("fresh claim ok=%t err=%v calls=%d", ok, err, calls)
		}
		if err := work(ctx); err != nil {
			t.Fatal(err)
		}
		if executor.work[2] != response.Items[0] {
			t.Fatalf("fresh tuple = %+v", executor.work[2])
		}
	})
}

func TestRunConsumerSharedDiscoveryCancellation(t *testing.T) {
	for _, initiator := range []bool{false, true} {
		t.Run(fmt.Sprintf("initiator=%t", initiator), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				firstCtx, cancelFirst := context.WithCancel(t.Context())
				defer cancelFirst()
				secondCtx, cancelSecond := context.WithCancel(t.Context())
				defer cancelSecond()
				release := make(chan struct{})
				calls := 0
				client := &discoveryRunConsumerClient{discover: func(ctx context.Context) (workerapi.RunLeaseDiscoveryResponse, error) {
					calls++
					select {
					case <-ctx.Done():
						return workerapi.RunLeaseDiscoveryResponse{}, ctx.Err()
					case <-release:
						return workerapi.RunLeaseDiscoveryResponse{Items: []workerapi.RunLeaseWork{{LeaseID: "a", LeaseSequence: 1}}}, nil
					}
				}}
				consumer := NewRunConsumer(&Runner{client: client, runLeaseExecutor: &runConsumerTestExecutor{}})
				first, second := make(chan runClaimResult, 1), make(chan runClaimResult, 1)
				go claimRunForTest(firstCtx, consumer, first)
				synctest.Wait()
				go claimRunForTest(secondCtx, consumer, second)
				synctest.Wait()
				if initiator {
					cancelFirst()
				} else {
					cancelSecond()
				}
				synctest.Wait()
				got := <-second
				if !errors.Is(got.err, context.Canceled) || got.ok || got.work != nil {
					t.Fatalf("cancelled claim = %+v", got)
				}
				if calls != 1 {
					t.Fatalf("discovery calls = %d", calls)
				}
				close(release)
				synctest.Wait()
				got = <-first
				if initiator {
					if !errors.Is(got.err, context.Canceled) || got.ok {
						t.Fatalf("initiator = %+v", got)
					}
					if _, ok, err := consumer.Claim(t.Context()); err != nil || !ok || calls != 2 {
						t.Fatalf("recovery ok=%t err=%v calls=%d", ok, err, calls)
					}
				} else if got.err != nil || !got.ok {
					t.Fatalf("surviving initiator = %+v", got)
				}
			})
		})
	}
}

func TestRunConsumerSharedDiscoveryRejectsFailedBatch(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalid=%t", invalid), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				release := make(chan struct{})
				calls := 0
				client := &discoveryRunConsumerClient{discover: func(ctx context.Context) (workerapi.RunLeaseDiscoveryResponse, error) {
					calls++
					select {
					case <-ctx.Done():
						return workerapi.RunLeaseDiscoveryResponse{}, ctx.Err()
					case <-release:
					}
					if invalid {
						return workerapi.RunLeaseDiscoveryResponse{Items: []workerapi.RunLeaseWork{{LeaseID: "valid", LeaseSequence: 1}, {LeaseID: "invalid"}}}, nil
					}
					return workerapi.RunLeaseDiscoveryResponse{}, errors.New("unavailable")
				}}
				consumer := NewRunConsumer(&Runner{client: client, runLeaseExecutor: &runConsumerTestExecutor{}}).(*runConsumer)
				stale := workerapi.RunLeaseWork{LeaseID: "stale", LeaseSequence: 1}
				consumer.stale[stale] = struct{}{}
				results := make(chan runClaimResult, 2)
				for range 2 {
					go claimRunForTest(ctx, consumer, results)
				}
				synctest.Wait()
				close(release)
				synctest.Wait()
				for range 2 {
					if got := <-results; got.err == nil || got.ok || got.work != nil {
						t.Fatalf("failed batch result = %+v", got)
					}
				}
				if calls != 1 || len(consumer.active) != 0 || len(consumer.stale) != 1 {
					t.Fatalf("calls=%d active=%v stale=%v", calls, consumer.active, consumer.stale)
				}
				client.discover = func(context.Context) (workerapi.RunLeaseDiscoveryResponse, error) {
					calls++
					return workerapi.RunLeaseDiscoveryResponse{}, nil
				}
				if _, ok, err := consumer.Claim(ctx); err != nil || ok || calls != 2 || len(consumer.stale) != 0 {
					t.Fatalf("recovery ok=%t err=%v calls=%d stale=%v", ok, err, calls, consumer.stale)
				}
			})
		})
	}
}

func TestRunConsumerRetirementDoesNotWaitForDiscoveryOrReplayItsSnapshot(t *testing.T) {
	for _, workErr := range []error{nil, errors.New("startup failed")} {
		t.Run(fmt.Sprint(workErr), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				tuple := workerapi.RunLeaseWork{LeaseID: "a", LeaseSequence: 1}
				release := make(chan struct{})
				calls := 0
				client := &discoveryRunConsumerClient{discover: func(ctx context.Context) (workerapi.RunLeaseDiscoveryResponse, error) {
					calls++
					if calls > 1 {
						select {
						case <-ctx.Done():
							return workerapi.RunLeaseDiscoveryResponse{}, ctx.Err()
						case <-release:
						}
					}
					return workerapi.RunLeaseDiscoveryResponse{Items: []workerapi.RunLeaseWork{tuple}}, nil
				}}
				consumer := NewRunConsumer(&Runner{client: client, runLeaseExecutor: &runConsumerTestExecutor{err: workErr}}).(*runConsumer)
				work, ok, err := consumer.Claim(ctx)
				if err != nil || !ok {
					t.Fatalf("initial claim ok=%t err=%v", ok, err)
				}
				results := make(chan runClaimResult, 1)
				go claimRunForTest(ctx, consumer, results)
				synctest.Wait()
				if err := work(ctx); !errors.Is(err, workErr) {
					t.Fatalf("work error = %v, want %v", err, workErr)
				}
				if len(consumer.active) != 0 {
					t.Fatal("retirement blocked by discovery")
				}
				close(release)
				synctest.Wait()
				if got := <-results; got.err != nil || got.ok || got.work != nil {
					t.Fatalf("retired tuple replayed = %+v", got)
				}
			})
		})
	}
}

func TestRunConsumerDeclinesSupersededBatchAfterStalePruning(t *testing.T) {
	tuple := workerapi.RunLeaseWork{LeaseID: "a", LeaseSequence: 1}
	var consumer *runConsumer
	var older *runDiscovery
	first := workerapi.RunLeaseWork{LeaseID: "first", LeaseSequence: 1}
	client := &discoveryRunConsumerClient{discover: func(context.Context) (workerapi.RunLeaseDiscoveryResponse, error) {
		consumer.mu.Lock()
		older = consumer.discovery
		consumer.mu.Unlock()
		return workerapi.RunLeaseDiscoveryResponse{Items: []workerapi.RunLeaseWork{first, tuple}}, nil
	}}
	consumer = NewRunConsumer(&Runner{client: client, runLeaseExecutor: &runConsumerTestExecutor{err: &httpclient.Error{StatusCode: http.StatusConflict}}}).(*runConsumer)
	if got, err := consumer.acquireRunLease(t.Context()); err != nil || got != first {
		t.Fatalf("initial reservation=%+v err=%v", got, err)
	}
	client.discover = func(context.Context) (workerapi.RunLeaseDiscoveryResponse, error) {
		return workerapi.RunLeaseDiscoveryResponse{Items: []workerapi.RunLeaseWork{tuple}}, nil
	}
	work, ok, err := consumer.Claim(t.Context())
	if err != nil || !ok {
		t.Fatalf("newer claim ok=%t err=%v", ok, err)
	}
	if err := work(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(consumer.stale) != 1 {
		t.Fatal("missing stale suppression")
	}
	client.discover = func(context.Context) (workerapi.RunLeaseDiscoveryResponse, error) {
		return workerapi.RunLeaseDiscoveryResponse{}, nil
	}
	if _, _, err := consumer.Claim(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(consumer.stale) != 0 {
		t.Fatal("stale tuple not pruned")
	}
	consumer.mu.Lock()
	defer consumer.mu.Unlock()
	if got, err := consumer.selectRunLeaseLocked(t.Context(), older); err != nil || got.LeaseID != "" {
		t.Fatalf("older batch replay = %+v, %v", got, err)
	}
}

func TestRunConsumerCancellationBeforeSelection(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	client := &discoveryRunConsumerClient{discover: func(context.Context) (workerapi.RunLeaseDiscoveryResponse, error) {
		calls++
		cancel()
		return workerapi.RunLeaseDiscoveryResponse{Items: []workerapi.RunLeaseWork{{LeaseID: "a", LeaseSequence: 1}}}, nil
	}}
	consumer := NewRunConsumer(&Runner{client: client, runLeaseExecutor: &runConsumerTestExecutor{}}).(*runConsumer)
	if work, ok, err := consumer.Claim(ctx); !errors.Is(err, context.Canceled) || ok || work != nil || len(consumer.active) != 0 {
		t.Fatalf("cancelled publication ok=%t err=%v active=%v", ok, err, consumer.active)
	}
	if work, ok, err := consumer.Claim(ctx); !errors.Is(err, context.Canceled) || ok || work != nil || calls != 1 {
		t.Fatalf("cancelled claim ok=%t err=%v calls=%d", ok, err, calls)
	}
}

func TestRunConsumerSharedDiscoveryKeepsIdleAndErrorBackoffDuringDrain(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("fail=%t", fail), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				release := make(chan struct{})
				var calls atomic.Int32
				client := &discoveryRunConsumerClient{discover: func(ctx context.Context) (workerapi.RunLeaseDiscoveryResponse, error) {
					calls.Add(1)
					select {
					case <-ctx.Done():
						return workerapi.RunLeaseDiscoveryResponse{}, ctx.Err()
					case <-release:
					}
					if fail {
						return workerapi.RunLeaseDiscoveryResponse{}, errors.New("unavailable")
					}
					return workerapi.RunLeaseDiscoveryResponse{}, nil
				}}
				consumer := NewRunConsumer(&Runner{client: client, runLeaseExecutor: &runConsumerTestExecutor{}})
				s, err := New(Config{ControlPlane: &testControlPlane{}, PollEvery: 2 * time.Second, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
				if err != nil {
					t.Fatal(err)
				}
				s.state.Store(StateDraining)
				spec := ConsumerSpec{Name: "run", Consumer: consumer, ContinueDuringDrain: true}
				for range 3 {
					go s.consume(ctx, ctx, spec, RecoveryEvidence{}, make(chan error, 1))
				}
				synctest.Wait()
				if calls.Load() != 1 {
					t.Fatalf("initial discoveries=%d, want 1", calls.Load())
				}
				close(release)
				synctest.Wait()
				if calls.Load() != 1 {
					t.Fatalf("retry before backoff: %d", calls.Load())
				}
				time.Sleep(2 * time.Second)
				synctest.Wait()
				if calls.Load() <= 1 || calls.Load() > 4 {
					t.Fatalf("discoveries after one poll interval=%d, want 2..4", calls.Load())
				}
				cancel()
				synctest.Wait()
				stopped := calls.Load()
				time.Sleep(4 * time.Second)
				if calls.Load() != stopped {
					t.Fatalf("discovery after drain claims frozen: %d -> %d", stopped, calls.Load())
				}
			})
		})
	}
}

func TestRunConsumerReservesInitiatorBeforePublishingToWaiters(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		a := workerapi.RunLeaseWork{LeaseID: "first", LeaseSequence: 1}
		b := workerapi.RunLeaseWork{LeaseID: "second", LeaseSequence: 2}
		release := make(chan struct{})
		calls := 0
		client := &discoveryRunConsumerClient{discover: func(ctx context.Context) (workerapi.RunLeaseDiscoveryResponse, error) {
			calls++
			select {
			case <-ctx.Done():
				return workerapi.RunLeaseDiscoveryResponse{}, ctx.Err()
			case <-release:
			}
			return workerapi.RunLeaseDiscoveryResponse{Items: []workerapi.RunLeaseWork{a, b}}, nil
		}}
		consumer := NewRunConsumer(&Runner{client: client, runLeaseExecutor: &runConsumerTestExecutor{}}).(*runConsumer)
		first, joined := make(chan workerapi.RunLeaseWork, 1), make(chan workerapi.RunLeaseWork, 1)
		go func() {
			got, err := consumer.acquireRunLease(ctx)
			if err != nil {
				t.Error(err)
			}
			first <- got
		}()
		synctest.Wait()
		go func() {
			got, err := consumer.acquireRunLease(ctx)
			if err != nil {
				t.Error(err)
			}
			joined <- got
		}()
		synctest.Wait()
		close(release)
		synctest.Wait()
		if got := <-first; got != a {
			t.Fatalf("initiator reservation=%+v, want %+v", got, a)
		}
		if got := <-joined; got != b {
			t.Fatalf("joined reservation=%+v, want %+v", got, b)
		}
		if calls != 1 {
			t.Fatalf("discovery calls=%d, want 1", calls)
		}
		if got, err := consumer.acquireRunLease(ctx); err != nil || got.LeaseID != "" || calls != 2 {
			t.Fatalf("fresh discovery=%+v err=%v calls=%d", got, err, calls)
		}
		if _, ok := consumer.active[a]; !ok {
			t.Fatal("new discovery lost initiator reservation")
		}
		if _, ok := consumer.active[b]; !ok {
			t.Fatal("new discovery lost joined reservation")
		}
	})
}
