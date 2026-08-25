package executor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

func TestPreparedRuntimePoolMemoizesSuccessfulRuntimeVerification(t *testing.T) {
	descriptor := deployment.RuntimeDescriptor{
		Architecture:    deployment.ArchitectureX8664,
		Digest:          "sha256:runtime",
		FormatVersion:   deployment.RuntimeDescriptorFormatVersion,
		MediaType:       deployment.RuntimeArtifactMediaType,
		RuntimeContract: deployment.RuntimeContract,
		SizeBytes:       42,
	}
	index := deployment.RuntimeIndex{
		Architecture:    descriptor.Architecture,
		RuntimeContract: descriptor.RuntimeContract,
	}
	pool := &PreparedRuntimePool{}
	calls := 0
	verify := func() (deployment.RuntimeIndex, error) {
		calls++
		return index, nil
	}

	got, hit, err := pool.verifyRuntime(descriptor, verify)
	if err != nil || hit || got != index {
		t.Fatalf("first verification = (%+v, %t, %v)", got, hit, err)
	}
	got, hit, err = pool.verifyRuntime(descriptor, verify)
	if err != nil || !hit || got != index || calls != 1 {
		t.Fatalf("memoized verification = (%+v, %t, %v), calls = %d", got, hit, err, calls)
	}

	other := descriptor
	other.SizeBytes++
	if _, hit, err := pool.verifyRuntime(other, verify); err != nil || hit || calls != 2 {
		t.Fatalf("different descriptor = (hit %t, %v), calls = %d", hit, err, calls)
	}

	canceled := descriptor
	canceled.Digest = "sha256:canceled"
	for range 2 {
		if _, hit, err := pool.verifyRuntime(canceled, func() (deployment.RuntimeIndex, error) {
			return deployment.RuntimeIndex{}, context.Canceled
		}); err != context.Canceled || hit {
			t.Fatalf("canceled verification = (hit %t, %v)", hit, err)
		}
	}

	if _, hit, err := (&PreparedRuntimePool{}).verifyRuntime(descriptor, verify); err != nil || hit || calls != 3 {
		t.Fatalf("new pool verification = (hit %t, %v), calls = %d", hit, err, calls)
	}
}

func TestPreparedRuntimePoolAllowsConcurrentRuntimeVerificationMisses(t *testing.T) {
	const workers = 8
	pool := &PreparedRuntimePool{}
	descriptor := deployment.RuntimeDescriptor{Digest: "sha256:runtime"}
	index := deployment.RuntimeIndex{RuntimeContract: deployment.RuntimeContract}
	entered := make(chan struct{}, workers)
	release := make(chan struct{})
	results := make(chan bool, workers)
	var calls atomic.Int32
	var wg sync.WaitGroup

	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _, err := pool.verifyRuntime(descriptor, func() (deployment.RuntimeIndex, error) {
				calls.Add(1)
				entered <- struct{}{}
				<-release
				return index, nil
			})
			results <- err == nil && got == index
		}()
	}
	for range workers {
		<-entered
	}
	close(release)
	wg.Wait()
	close(results)
	for ok := range results {
		if !ok {
			t.Fatal("concurrent verification failed")
		}
	}
	if calls.Load() != workers {
		t.Fatalf("verification calls = %d, want %d", calls.Load(), workers)
	}
}
