package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
)

func TestRunRetryReadyUsesBoundedSelector(t *testing.T) {
	store := &runRetryReadyFixture{}
	workflow := runRetryReadyWorkflow{store: store}
	if err := workflow.ready(context.Background(), 37); err != nil {
		t.Fatal(err)
	}
	if store.limit != 37 || store.calls != 1 {
		t.Fatalf("calls = %d, limit = %d", store.calls, store.limit)
	}
}

func TestRunRetryReadyReturnsStoreFailure(t *testing.T) {
	want := errors.New("database unavailable")
	workflow := runRetryReadyWorkflow{store: &runRetryReadyFixture{err: want}}
	if err := workflow.ready(context.Background(), runRetryReadyLimit); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

type runRetryReadyFixture struct {
	calls int
	limit int32
	err   error
}

func (f *runRetryReadyFixture) ReadyRunRetries(_ context.Context, params db.ReadyRunRetriesParams) ([]db.ReadyRunRetriesRow, error) {
	f.calls++
	f.limit = params.RowLimit
	return nil, f.err
}
