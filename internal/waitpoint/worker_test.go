package waitpoint

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/sqs"
)

func TestWorkerReceiveLoopBacksOffAfterReceiveError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		subscriber := &errorSubscriber{err: errors.New("receive failed")}
		worker := &Worker{
			subscriber: subscriber,
			log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		}
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() {
			errc <- worker.receiveLoop(ctx)
		}()

		synctest.Wait()
		if calls := subscriber.calls.Load(); calls != 1 {
			t.Fatalf("receive calls = %d, want 1", calls)
		}
		time.Sleep(time.Second - time.Nanosecond)
		synctest.Wait()
		if calls := subscriber.calls.Load(); calls != 1 {
			t.Fatalf("receive calls before backoff = %d, want 1", calls)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if calls := subscriber.calls.Load(); calls != 2 {
			t.Fatalf("receive calls after backoff = %d, want 2", calls)
		}
		cancel()
		synctest.Wait()
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Fatalf("receiveLoop error = %v, want context canceled", err)
		}
	})
}

func TestWorkerReconcileLoopUsesConfiguredCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &reconcileStore{}
		worker := &Worker{
			notifier:       &Notifier{store: store},
			log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
			reconcileEvery: time.Minute,
			batchSize:      1,
		}
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() {
			errc <- worker.reconcileLoop(ctx, true)
		}()

		synctest.Wait()
		if requeueCalls, listCalls := store.requeueCalls.Load(), store.listCalls.Load(); requeueCalls != 1 || listCalls != 1 {
			t.Fatalf("initial reconcile calls = requeue:%d list:%d, want 1/1", requeueCalls, listCalls)
		}
		time.Sleep(time.Minute - time.Nanosecond)
		synctest.Wait()
		if requeueCalls, listCalls := store.requeueCalls.Load(), store.listCalls.Load(); requeueCalls != 1 || listCalls != 1 {
			t.Fatalf("reconcile calls before tick = requeue:%d list:%d, want 1/1", requeueCalls, listCalls)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if requeueCalls, listCalls := store.requeueCalls.Load(), store.listCalls.Load(); requeueCalls != 2 || listCalls != 2 {
			t.Fatalf("reconcile calls after tick = requeue:%d list:%d, want 2/2", requeueCalls, listCalls)
		}
		cancel()
		synctest.Wait()
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Fatalf("reconcileLoop error = %v, want context canceled", err)
		}
	})
}

type errorSubscriber struct {
	err   error
	calls atomic.Int32
}

func (s *errorSubscriber) Receive(context.Context) ([]sqs.ReceivedMessage, error) {
	s.calls.Add(1)
	return nil, s.err
}

func (s *errorSubscriber) Delete(context.Context, sqs.ReceivedMessage) error {
	return nil
}

type reconcileStore struct {
	Store
	requeueCalls atomic.Int32
	listCalls    atomic.Int32
}

func (s *reconcileStore) RequeueStaleSendingWaitpointDeliveries(context.Context, db.RequeueStaleSendingWaitpointDeliveriesParams) error {
	s.requeueCalls.Add(1)
	return nil
}

func (s *reconcileStore) ListDueWaitpointDeliveries(context.Context, int32) ([]db.WaitpointDelivery, error) {
	s.listCalls.Add(1)
	return nil, nil
}
