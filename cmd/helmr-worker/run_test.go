package main

import (
	"context"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/compute"
)

func TestFitsBuildHostComputeUsesDiskIndependentHostPool(t *testing.T) {
	if !fitsBuildHostCompute(compute.ResourceVector{
		MilliCPU:  2000,
		MemoryMiB: 2048,
		Slots:     1,
	}) {
		t.Fatal("fixed build guest does not fit its exact host compute envelope")
	}
	if fitsBuildHostCompute(compute.ResourceVector{
		MilliCPU:  1999,
		MemoryMiB: 2048,
		Slots:     1,
	}) {
		t.Fatal("undersized host compute envelope was accepted")
	}
}

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
