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

type runCleanupSession struct {
	serverTestSession
	open func(context.Context) (vm.Stream, error)
}

func (s *runCleanupSession) OpenStream(ctx context.Context) (vm.Stream, error) {
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
			session := &runCleanupSession{open: func(context.Context) (vm.Stream, error) {
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
					if req.RunLeaseId != "lease" || req.WriterGeneration != 3 || req.ChannelToken != "token" || req.AttemptNumber != 2 {
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
			mount := workerapi.ComputerInstanceAssignment{ComputerID: "computer", ComputerInstanceID: "instance", WriterGeneration: 3, GuestdChannelToken: "token"}
			err := (Server{}).reconcileComputerRuns(ctx, session, mount, client)
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
			session := &runCleanupSession{open: func(context.Context) (vm.Stream, error) {
				calls++
				if settled {
					client.pending = false
				}
				return nil, io.ErrUnexpectedEOF
			}}
			err := (Server{}).reconcileComputerRuns(ctx, session, workerapi.ComputerInstanceAssignment{}, client)
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
