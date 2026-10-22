package computerhost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type optionalTransferRoundTrip func(*http.Request) (*http.Response, error)

func (f optionalTransferRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOptionalTransferPacesUploadAndDownloadTogether(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, policy, _ := optionalMachines(t)
		policy.Optional.IOBytesPerSecond = 100
		p.ApplyResourcePolicy(policy)
		transport := p.OptionalTransport(optionalTransferRoundTrip(func(request *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			if len(body) != 100 {
				t.Errorf("upload length: %d", len(body))
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
		}))
		started := time.Now()
		request, err := http.NewRequestWithContext(t.Context(), "PUT", "https://storage.invalid/object", bytes.NewReader(make([]byte, 100)))
		if err != nil {
			t.Fatal(err)
		}
		response, err := transport.RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if len(body) != 100 || time.Since(started) < 2*time.Second || time.Since(started) > 3*time.Second {
			t.Fatalf("aggregate transfer: bytes=%d elapsed=%s", len(body), time.Since(started))
		}
	})
}

func TestOptionalConcurrentTransfersShareOneRate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, policy, _ := optionalMachines(t)
		policy.Optional.IOBytesPerSecond = 100
		p.ApplyResourcePolicy(policy)
		started := time.Now()
		var jobs sync.WaitGroup
		for range 10 {
			jobs.Go(func() {
				body := &optionalTransferBody{ReadCloser: io.NopCloser(bytes.NewReader(make([]byte, 10))), ctx: t.Context(), machines: p}
				if _, err := io.ReadAll(body); err != nil {
					t.Error(err)
				}
			})
		}
		jobs.Wait()
		if time.Since(started) < time.Second {
			t.Fatal("parallel requests multiplied optional bandwidth")
		}
	})
}

func TestOptionalTransferCancellationAndZeroPolicy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, policy, _ := optionalMachines(t)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { _, err := p.waitOptionalTransfer(ctx, 1000); done <- err }()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("transfer cancellation: %v", err)
		}
		policy.Revision++
		policy.Optional.IOBytesPerSecond = 0
		p.ApplyResourcePolicy(policy)
		if _, err := p.waitOptionalTransfer(t.Context(), 1); !errors.Is(err, errOptionalReclaimed) {
			t.Fatalf("zero rate performed transfer: %v", err)
		}
	})
}

func TestOptionalTransferRevokesAllowanceWaitingUnderOldPolicy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, policy, _ := optionalMachines(t)
		done := make(chan error, 1)
		go func() { _, err := p.waitOptionalTransfer(t.Context(), 1000); done <- err }()
		synctest.Wait()
		policy.Revision++
		policy.Optional.IOBytesPerSecond = 0
		p.ApplyResourcePolicy(policy)
		if err := <-done; !errors.Is(err, errOptionalReclaimed) {
			t.Fatalf("old allowance survived zero policy: %v", err)
		}
	})
}

func TestOptionalAsyncReadFenceSurvivesFreshnessRecovery(t *testing.T) {
	p, policy, shape := optionalMachines(t)
	key := instanceReservationKey("seed", 1)
	if err := p.reserveOptionalSeed(key, shape); err != nil {
		t.Fatal(err)
	}
	p.optional.confirmed = time.Now().Add(-3 * workerapi.WorkerObservationInterval)
	// A pager/NBD reader does not return its error directly to the seed owner.
	if _, err := p.waitOptionalTransfer(t.Context(), 1); !errors.Is(err, errOptionalReclaimed) {
		t.Fatal(err)
	}
	p.ApplyResourcePolicy(policy)
	if !p.optionalSeedReclaimRequested(key) || !p.ResourceObservation().ReclaimPending {
		t.Fatal("freshness recovery erased asynchronous reclamation")
	}
}
