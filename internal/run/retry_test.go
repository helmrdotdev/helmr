package run

import (
	"context"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
)

func TestRetryReadyWorkerUsesBoundedSelector(t *testing.T) {
	store := &retryReadyFixture{}
	worker, err := NewRetryReadyWorker(nil, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ready(context.Background(), 37); err != nil {
		t.Fatal(err)
	}
	if store.limit != 37 || store.calls != 1 {
		t.Fatalf("calls = %d, limit = %d", store.calls, store.limit)
	}
}

func TestRetryReadyWorkerReturnsStoreFailure(t *testing.T) {
	want := errors.New("database unavailable")
	worker, err := NewRetryReadyWorker(nil, &retryReadyFixture{err: want})
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.ready(context.Background(), retryReadyLimit); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestRetryReadyWorkerRequiresStore(t *testing.T) {
	if _, err := NewRetryReadyWorker(nil, nil); err == nil {
		t.Fatal("missing store accepted")
	}
}

type retryReadyFixture struct {
	calls int
	limit int32
	err   error
}

func (f *retryReadyFixture) ReadyRunRetries(_ context.Context, limit int32) ([]db.ReadyRunRetriesRow, error) {
	f.calls++
	f.limit = limit
	return nil, f.err
}
