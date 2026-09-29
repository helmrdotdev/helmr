package executor

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type cancellationClient struct {
	computerMaterializerTestClient
	command    workerapi.ComputerCommand
	grant      workerapi.ComputerCommandCancellation
	attached   bool
	launchRead <-chan struct{}
	finish     context.CancelFunc
}

func (c *cancellationClient) ClaimComputerCommand(ctx context.Context, r workerapi.ComputerCommandClaimRequest) (workerapi.ComputerCommandClaimResponse, error) {
	if c.attached && !slices.Contains(r.ActiveCommandIDs, c.command.CommandID) {
		return workerapi.ComputerCommandClaimResponse{Command: &c.command}, nil
	}
	if c.attached {
		select {
		case <-c.launchRead:
		case <-ctx.Done():
			return workerapi.ComputerCommandClaimResponse{}, ctx.Err()
		}
	}
	if !slices.Contains(r.ActiveCancellationIDs, c.grant.CommandID) {
		return workerapi.ComputerCommandClaimResponse{Cancellation: &c.grant}, nil
	}
	return workerapi.ComputerCommandClaimResponse{}, nil
}
func (c *cancellationClient) CompleteComputerCommand(_ context.Context, r workerapi.ComputerCommandCompleteRequest) error {
	c.commandMu.Lock()
	defer c.commandMu.Unlock()
	if r.Outcome != "computer_command_cancelled" || len(c.commandLogs) != 1 {
		return errors.New("completion preceded output or had wrong outcome")
	}
	c.execCompletions = append(c.execCompletions, r)
	c.finish()
	return nil
}

type cancellationSession struct {
	computerMaterializerTestSession
	canceled       chan struct{}
	launchRead     chan struct{}
	canceledOnce   sync.Once
	launchOnce     sync.Once
	handlers       sync.WaitGroup
	mu             sync.Mutex
	cancelAttempts int
	dropFirstAck   bool
}

func (s *cancellationSession) OpenStream(ctx context.Context) (vm.Stream, error) {
	host, guest := net.Pipe()
	s.handlers.Add(1)
	go func() {
		defer s.handlers.Done()
		defer guest.Close()
		stop := context.AfterFunc(ctx, func() { guest.Close() })
		defer stop()
		h, _, err := wire.ReadStreamFrameHeader(guest)
		if err != nil {
			return
		}
		switch h.Type {
		case wire.StreamTypeComputerCommandCancel:
			var r computerv0.ComputerCommandCancelRequest
			if frameio.ReadProtoFrame(guest, &r) != nil {
				return
			}
			s.canceledOnce.Do(func() { close(s.canceled) })
			s.mu.Lock()
			s.cancelAttempts++
			drop := s.dropFirstAck && s.cancelAttempts == 1
			s.mu.Unlock()
			if !drop {
				_ = frameio.WriteProtoFrame(guest, &computerv0.ComputerCommandCancelResponse{Accepted: true})
			}
		case wire.StreamTypeComputerBasicExec:
			var r computerv0.ComputerBasicExecRequest
			if frameio.ReadProtoFrame(guest, &r) != nil {
				return
			}
			s.launchOnce.Do(func() { close(s.launchRead) })
			select {
			case <-s.canceled:
			case <-ctx.Done():
				return
			}
			_ = frameio.WriteProtoFrame(guest, &computerv0.ComputerBasicExecEvent{Event: &computerv0.ComputerBasicExecEvent_Output{Output: &computerv0.CommandOutputChunk{Stream: "stdout", Content: []byte("retained output"), ObservedAtUnixNano: time.Now().UnixNano()}}})
			_ = frameio.WriteProtoFrame(guest, &computerv0.ComputerBasicExecEvent{Event: &computerv0.ComputerBasicExecEvent_Result{Result: &computerv0.ComputerBasicExecResult{Outcome: "computer_command_cancelled", RequestFingerprint: r.Envelope.RequestFingerprint, ErrorJson: `{"code":"computer_command_cancelled"}`}}})
		}
	}()
	return testVMStream(host), nil
}

func TestCommandCancellationDrainsOutputBeforeCompletion(t *testing.T) {
	for _, mode := range []string{"attached", "detached", "lost_ack"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			session := &cancellationSession{canceled: make(chan struct{}), launchRead: make(chan struct{}), dropFirstAck: mode == "lost_ack"}
			mount := workerapi.ComputerInstanceAssignment{OrgID: "org", ComputerID: "computer", ComputerInstanceID: "instance", WriterGeneration: 2, GuestdChannelToken: "token"}
			command := workerapi.ComputerCommand{CommandID: "target", ComputerID: mount.ComputerID, ComputerInstanceID: mount.ComputerInstanceID, WriterGeneration: 2, RequestFingerprint: "fingerprint", ExpiresAt: time.Now().Add(time.Minute), Request: json.RawMessage(`{"command":["sleep","30"]}`)}
			client := &cancellationClient{command: command, grant: workerapi.ComputerCommandCancellation{CommandID: command.CommandID, ComputerID: command.ComputerID, ComputerInstanceID: command.ComputerInstanceID, WriterGeneration: command.WriterGeneration, RequestFingerprint: command.RequestFingerprint, ExpiresAt: command.ExpiresAt}, attached: mode == "attached", launchRead: session.launchRead, finish: cancel}
			m := ComputerMaterializer{PollEvery: time.Millisecond, ClaimErrorBackoff: time.Millisecond}
			renewal := m.startRenewalLoop(ctx, workerapi.ComputerInstanceRenewRequest{}, client, time.Hour)
			err := m.serveComputerMount(ctx, renewal, newInstanceMount(session), mount, client, nil)
			session.handlers.Wait()
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if len(client.execCompletions) != 1 || session.closeCount() != 0 {
				t.Fatalf("completions=%d closes=%d", len(client.execCompletions), session.closeCount())
			}
			if mode == "lost_ack" && session.cancelAttempts != 2 {
				t.Fatalf("cancel attempts=%d", session.cancelAttempts)
			}
		})
	}
}
