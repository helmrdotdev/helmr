package computerhost

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

type runCleanupClient struct {
	serverTestClient
	pending      bool
	queries      int
	acknowledged int
	cancel       context.CancelFunc
	transient    bool
}

func (c *runCleanupClient) GetComputerRunCleanup(context.Context, workerapi.ComputerRunCleanupRequest) (workerapi.ComputerRunCleanupResponse, error) {
	c.queries++
	if !c.pending {
		if c.cancel != nil {
			c.cancel()
		}
		return workerapi.ComputerRunCleanupResponse{}, nil
	}
	return workerapi.ComputerRunCleanupResponse{Run: &workerapi.ComputerRunCleanup{RunID: "run", RunLeaseID: "lease", AttemptNumber: 2}}, nil
}
func (c *runCleanupClient) ReconcileComputerRun(context.Context, workerapi.ComputerRunReconcileRequest) error {
	c.acknowledged++
	if c.transient && c.acknowledged == 1 {
		return errors.New("lost CP reply")
	}
	c.pending = false
	return nil
}

type runCleanupMachine struct {
	serverTestMachine
	open func(context.Context) (vm.Stream, error)
}

func (s *runCleanupMachine) OpenStream(ctx context.Context) (vm.Stream, error) {
	return s.open(ctx)
}

func TestComputerRunCleanupRetriesLostReplies(t *testing.T) {
	for _, loss := range []string{"guest", "control plane"} {
		t.Run(loss, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			client := &runCleanupClient{pending: true, cancel: cancel, transient: loss == "control plane"}
			calls := 0
			results := make(chan error, 3)
			machine := &runCleanupMachine{open: func(context.Context) (vm.Stream, error) {
				calls++
				attempt := calls
				host, guest := net.Pipe()
				go func() {
					defer guest.Close()
					header, _, err := wire.ReadStreamFrameHeader(guest)
					if err != nil {
						results <- err
						return
					}
					if header.Type != wire.StreamTypeComputerRunCleanup {
						results <- errors.New("wrong stream")
						return
					}
					var req computerv0.ComputerRunCleanupRequest
					if err = frameio.ReadProtoFrame(guest, &req); err != nil {
						results <- err
						return
					}
					if req.RunLeaseId != "lease" || req.WriterGeneration != 3 || req.ChannelCredential != "token" || req.AttemptNumber != 2 {
						results <- errors.New("wrong identity")
						return
					}
					if loss == "guest" && attempt == 1 {
						results <- nil
						return
					}
					results <- frameio.WriteProtoFrame(guest, &computerv0.ComputerRunCleanupResponse{Reconciled: true})
				}()
				return testVMStream(host), nil
			}}
			mount := workerapi.ComputerInstanceAssignment{ComputerID: "computer", ComputerInstanceID: "instance", WriterGeneration: 3, GuestChannelCredential: "token"}
			err := (Server{}).reconcileComputerRuns(ctx, machine, mount, nil, client)
			if !errors.Is(err, context.Canceled) || calls != 2 || client.pending {
				t.Fatalf("err=%v calls=%d pending=%v", err, calls, client.pending)
			}
			for range calls {
				if err := <-results; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestComputerRunCleanupRevalidatesBeforePhysicalFallback(t *testing.T) {
	for _, settled := range []bool{false, true} {
		t.Run(map[bool]string{false: "unproven", true: "concurrently reconciled"}[settled], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			client := &runCleanupClient{pending: true, cancel: cancel}
			calls := 0
			machine := &runCleanupMachine{open: func(context.Context) (vm.Stream, error) {
				calls++
				if settled {
					client.pending = false
				}
				return nil, io.ErrUnexpectedEOF
			}}
			err := (Server{}).reconcileComputerRuns(ctx, machine, workerapi.ComputerInstanceAssignment{}, nil, client)
			if settled {
				if !errors.Is(err, context.Canceled) || calls != 1 {
					t.Fatalf("settled member failed Computer: %v (%d calls)", err, calls)
				}
			} else {
				var failure computerMountFailure
				if !errors.As(err, &failure) || calls != 3 || client.queries != 4 {
					t.Fatalf("unproven scope not fenced after revalidation: %v calls=%d queries=%d", err, calls, client.queries)
				}
			}
			if client.acknowledged != 0 {
				t.Fatal("unproven scope acknowledged")
			}
		})
	}
}

func TestComputerRunCleanupWaitsForCaptureOwner(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		t.Run(map[bool]string{false: "capture already held", true: "capture starts during RPC"}[inFlight], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ref := preparedMachineRef{id: "instance", epoch: 1}
				claim := &machineClaim{gen: 1, entry: preparedMachineEntry{target: workerapi.InstanceReconcileTarget{DesiredVersion: 1}}}
				p := &PreparedMachines{claims: map[preparedMachineRef]*machineClaim{ref: claim}}
				checkout := &machineCheckout{machines: p, ref: ref, gen: 1}
				if !inFlight {
					claim.checkpointer = &computerCheckpointer{}
				}
				calls := 0
				machine := &runCleanupMachine{open: func(context.Context) (vm.Stream, error) {
					calls++
					if inFlight && calls == 1 {
						p.mu.Lock()
						claim.checkpointer = &computerCheckpointer{}
						p.mu.Unlock()
						time.Sleep(35 * time.Second)
					}
					return nil, io.ErrUnexpectedEOF
				}}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				result := make(chan error, 1)
				go func() {
					result <- (Server{}).reconcileComputerRuns(ctx, machine, workerapi.ComputerInstanceAssignment{}, checkout, &runCleanupClient{pending: true})
				}()
				time.Sleep(130 * time.Second)
				synctest.Wait()
				wantCalls := 0
				if inFlight {
					wantCalls = 1
				}
				if calls != wantCalls || claim.teardown || claim.ownerExited {
					t.Fatalf("held source failed: calls=%d claim=%+v", calls, claim)
				}
				select {
				case err := <-result:
					t.Fatalf("cleanup ended while capture held: %v", err)
				default:
				}
				p.mu.Lock()
				claim.checkpointer = nil
				claim.entry.target.DesiredVersion++
				p.mu.Unlock()
				var failure computerMountFailure
				if err := <-result; !errors.As(err, &failure) || calls != wantCalls+3 || !claim.teardown || claim.ownerExited {
					t.Fatalf("cleanup did not get a fresh failure budget: %v calls=%d claim=%+v", err, calls, claim)
				}
			})
		})
	}
}

func TestComputerRunCleanupDiscardsFailureAcrossCompletedAbort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ref := preparedMachineRef{id: "instance", epoch: 1}
		claim := &machineClaim{gen: 1, entry: preparedMachineEntry{target: workerapi.InstanceReconcileTarget{DesiredVersion: 1}}}
		p := &PreparedMachines{claims: map[preparedMachineRef]*machineClaim{ref: claim}}
		checkout := &machineCheckout{machines: p, ref: ref, gen: 1}
		calls := 0
		machine := &runCleanupMachine{open: func(context.Context) (vm.Stream, error) {
			calls++
			if calls == 3 {
				p.mu.Lock()
				claim.entry.target.DesiredVersion++
				p.mu.Unlock()
			}
			return nil, io.ErrUnexpectedEOF
		}}
		var failure computerMountFailure
		err := (Server{}).reconcileComputerRuns(t.Context(), machine, workerapi.ComputerInstanceAssignment{}, checkout, &runCleanupClient{pending: true})
		if !errors.As(err, &failure) || calls != 6 || !claim.teardown || claim.ownerExited {
			t.Fatalf("stale failure fenced resumed source: %v calls=%d claim=%+v", err, calls, claim)
		}
	})
}

func TestComputerRunCleanupCannotCommitFailureDuringCapture(t *testing.T) {
	ref := preparedMachineRef{id: "instance", epoch: 1}
	claim := &machineClaim{gen: 1, entry: preparedMachineEntry{target: workerapi.InstanceReconcileTarget{DesiredVersion: 1}}}
	p := &PreparedMachines{claims: map[preparedMachineRef]*machineClaim{ref: claim}}
	checkout := &machineCheckout{machines: p, ref: ref, gen: 1}
	version, held := checkout.runCleanupHeld(0, false)
	if held {
		t.Fatal("idle source held")
	}
	claim.checkpointer = &computerCheckpointer{}
	if _, held := checkout.runCleanupHeld(version, true); !held || claim.teardown || claim.ownerExited {
		t.Fatal("cleanup committed failure after capture acquired ownership")
	}
}
