package guestd

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

func TestPreparationCompletionWaitReleasesLifecycleAndRetainsExecution(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		t.Run(map[bool]string{false: "completion", true: "cancel-observation"}[cancelWait], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				registry, entry, request := preparationGuestFixture(t)
				request.ExpiresAtUnixNano = time.Now().Add(time.Hour).UnixNano()
				request.Start = &computerv0.PreparationStart{TemplateDefinitionId: "repo", LogLimits: &computerv0.PreparationLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 4}}
				finish := make(chan struct{})
				var runs atomic.Int32
				run := func(_ *computerMountEntry, ctx context.Context, _ *computerv0.PreparationStart, output *preparationOutput) (error, error) {
					runs.Add(1)
					select {
					case <-finish:
						output.close(true)
						return nil, nil
					case <-ctx.Done():
						return ctx.Err(), nil
					}
				}
				if _, err := registry.controlPreparation(t.Context(), request, run); err != nil {
					t.Fatal(err)
				}
				request.Start = nil
				request.WaitForCompletion = true
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				result := make(chan *computerv0.PreparationControlResponse, 1)
				failed := make(chan error, 1)
				go func() {
					response, err := registry.controlPreparation(ctx, request, run)
					result <- response
					failed <- err
				}()
				synctest.Wait()
				// A concurrent renewal takes both lifecycle gates. The observing wait must
				// not retain either gate or prevent extending the actual execution lease.
				renewal := proto.Clone(request).(*computerv0.PreparationControlRequest)
				renewal.WaitForCompletion = false
				renewal.RenewOnly = true
				renewal.ExpiresAtUnixNano = time.Now().Add(2 * time.Hour).UnixNano()
				if response, err := registry.controlPreparation(t.Context(), renewal, run); err != nil || response.State != "running" {
					t.Fatalf("renewal blocked by observation: %+v %v", response, err)
				}
				if cancelWait {
					cancel()
					if err := <-failed; !errors.Is(err, context.Canceled) {
						t.Fatal(err)
					}
					<-result
					select {
					case <-entry.preparation.done:
						t.Fatal("observation cancellation stopped authored code")
					default:
					}
					close(finish)
					<-entry.preparation.done
				} else {
					close(finish)
					response := <-result
					if err := <-failed; err != nil || response.State != "succeeded" {
						t.Fatalf("completion=%+v %v", response, err)
					}
				}
				if runs.Load() != 1 {
					t.Fatal("wait replayed authored execution")
				}
			})
		})
	}
}

func TestPreparationCompletionWaitPeerCloseJoinsObservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry, entry, request := preparationGuestFixture(t)
		request.ExpiresAtUnixNano = time.Now().Add(time.Hour).UnixNano()
		request.Start = &computerv0.PreparationStart{TemplateDefinitionId: "repo", LogLimits: &computerv0.PreparationLogLimits{ChunkBytes: 1024, BufferBytes: 2048, BufferRecords: 4}}
		finish := make(chan struct{})
		run := func(_ *computerMountEntry, ctx context.Context, _ *computerv0.PreparationStart, output *preparationOutput) (error, error) {
			select {
			case <-finish:
				output.close(true)
				return nil, nil
			case <-ctx.Done():
				return ctx.Err(), nil
			}
		}
		if _, err := registry.controlPreparation(t.Context(), request, run); err != nil {
			t.Fatal(err)
		}
		request.Start = nil
		request.WaitForCompletion = true
		worker, guest := net.Pipe()
		defer guest.Close()
		done := make(chan error, 1)
		go func() { done <- handlePreparationControl(t.Context(), guest, 0, registry) }()
		if err := frameio.WriteProtoFrame(worker, request); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		worker.Close()
		synctest.Wait()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		default:
			t.Fatal("closed peer retained completion observer")
		}
		select {
		case <-entry.preparation.done:
			t.Fatal("closed observer stopped authored preparation")
		default:
		}
		close(finish)
		<-entry.preparation.done
	})
}
