package main

import (
	"context"
	"errors"
	"testing"
)

func TestRetryableWorkerCloserRetriesFailureAndMemoizesOnlySuccess(t *testing.T) {
	calls := 0
	closer := retryableWorkerCloser{close: func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("partial close")
		}
		return nil
	}}
	if err := closer.Close(context.Background()); err == nil {
		t.Fatal("first Close() unexpectedly succeeded")
	}
	if err := closer.Close(context.Background()); err != nil {
		t.Fatalf("retry Close() = %v", err)
	}
	if err := closer.Close(context.Background()); err != nil {
		t.Fatalf("idempotent Close() = %v", err)
	}
	if calls != 2 {
		t.Fatalf("underlying close calls = %d, want 2", calls)
	}
}
