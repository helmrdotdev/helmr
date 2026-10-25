package computerhost

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
)

func TestPreparedMachinesMemoizesSuccessfulRuntimeVerification(t *testing.T) {
	descriptor := artifact.RuntimeDescriptor{
		Architecture:    definition.ArchitectureX8664,
		Digest:          "sha256:runtime",
		FormatVersion:   artifact.RuntimeDescriptorFormatVersion,
		MediaType:       artifact.RuntimeArtifactMediaType,
		RuntimeContract: definition.RuntimeContract,
		SizeBytes:       42,
	}
	index := artifact.RuntimeIndex{
		Architecture:    descriptor.Architecture,
		RuntimeContract: descriptor.RuntimeContract,
	}
	machines := &PreparedMachines{}
	calls := 0
	verify := func() (artifact.RuntimeIndex, error) {
		calls++
		return index, nil
	}

	got, hit, err := machines.verifyRuntime(descriptor, verify)
	if err != nil || hit || got != index {
		t.Fatalf("first verification = (%+v, %t, %v)", got, hit, err)
	}
	got, hit, err = machines.verifyRuntime(descriptor, verify)
	if err != nil || !hit || got != index || calls != 1 {
		t.Fatalf("memoized verification = (%+v, %t, %v), calls = %d", got, hit, err, calls)
	}

	other := descriptor
	other.SizeBytes++
	if _, hit, err := machines.verifyRuntime(other, verify); err != nil || hit || calls != 2 {
		t.Fatalf("different descriptor = (hit %t, %v), calls = %d", hit, err, calls)
	}

	canceled := descriptor
	canceled.Digest = "sha256:canceled"
	for range 2 {
		if _, hit, err := machines.verifyRuntime(canceled, func() (artifact.RuntimeIndex, error) {
			return artifact.RuntimeIndex{}, context.Canceled
		}); err != context.Canceled || hit {
			t.Fatalf("canceled verification = (hit %t, %v)", hit, err)
		}
	}

	if _, hit, err := (&PreparedMachines{}).verifyRuntime(descriptor, verify); err != nil || hit || calls != 3 {
		t.Fatalf("new machines verification = (hit %t, %v), calls = %d", hit, err, calls)
	}
}

func TestPreparedMachinesConcurrentRuntimeVerification(t *testing.T) {
	for _, distinct := range []bool{true, false} {
		t.Run(map[bool]string{true: "distinct descriptors overlap", false: "same descriptor results"}[distinct], func(t *testing.T) {
			const workers = 8
			machines := &PreparedMachines{}
			index := artifact.RuntimeIndex{RuntimeContract: definition.RuntimeContract}
			entered := make(chan struct{}, workers)
			release := make(chan struct{})
			var releaseOnce sync.Once
			var wg sync.WaitGroup
			defer func() { releaseOnce.Do(func() { close(release) }); wg.Wait() }()
			for i := range workers {
				descriptor := artifact.RuntimeDescriptor{Digest: "sha256:runtime"}
				if distinct {
					descriptor.Digest = fmt.Sprintf("sha256:runtime-%d", i)
				}
				wg.Go(func() {
					got, _, err := machines.verifyRuntime(descriptor, func() (artifact.RuntimeIndex, error) {
						entered <- struct{}{}
						<-release
						return index, nil
					})
					if err != nil || got != index {
						t.Errorf("verification = %+v, %v", got, err)
					}
				})
			}
			wantEntered := 1
			if distinct {
				wantEntered = workers
			}
			timer := time.NewTimer(5 * time.Second)
			defer timer.Stop()
			for range wantEntered {
				select {
				case <-entered:
				case <-timer.C:
					t.Fatal("independent verification blocked by shared lock")
				}
			}
			releaseOnce.Do(func() { close(release) })
			wg.Wait()
		})
	}
}
