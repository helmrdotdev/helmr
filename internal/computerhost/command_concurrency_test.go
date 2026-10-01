package computerhost

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/vm"
	"io"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type parallelCommandClient struct {
	serverTestClient
	commands          []workerapi.ComputerCommand
	finish            context.CancelFunc
	completionStarted chan struct{}
	firstOpened       <-chan struct{}
	releaseFirst      bool
}

func (c *parallelCommandClient) ClaimComputerCommand(ctx context.Context, request workerapi.ComputerCommandClaimRequest) (workerapi.ComputerCommandClaimResponse, error) {
	for _, command := range c.commands {
		if !slices.Contains(request.ActiveCommandIDs, command.CommandID) {
			if command.CommandID == "second" {
				select {
				case <-c.firstOpened:
				case <-ctx.Done():
					return workerapi.ComputerCommandClaimResponse{}, ctx.Err()
				}
			}
			if c.releaseFirst && command.CommandID == "first" {
				return workerapi.ComputerCommandClaimResponse{Release: &workerapi.ComputerCommandRelease{ComputerID: command.ComputerID, RequestFingerprint: command.RequestFingerprint, Completion: workerapi.ComputerCommandCompleteRequest{OrgID: "org", CommandID: command.CommandID, ComputerInstanceID: command.ComputerInstanceID, WriterGeneration: command.WriterGeneration, Outcome: "exited"}}}, nil
			}
			return workerapi.ComputerCommandClaimResponse{Command: &command}, nil
		}
	}
	return workerapi.ComputerCommandClaimResponse{}, nil
}
func (c *parallelCommandClient) CompleteComputerCommand(ctx context.Context, r workerapi.ComputerCommandCompleteRequest) error {
	if r.CommandID == "first" && c.completionStarted != nil {
		close(c.completionStarted)
		<-ctx.Done()
		return ctx.Err()
	}
	c.commandMu.Lock()
	defer c.commandMu.Unlock()
	c.execCompletions = append(c.execCompletions, r)
	c.finish()
	return nil
}

type parallelCommandSession struct {
	*serverTestSession
	firstOpened chan struct{}
	once        sync.Once
}

func (s *parallelCommandSession) OpenStream(ctx context.Context) (vm.Stream, error) {
	stream, err := s.serverTestSession.OpenStream(ctx)
	s.once.Do(func() { close(s.firstOpened) })
	return stream, err
}

func TestCommandsProgressWhilePeerStreamIsBlocked(t *testing.T) {
	for _, name := range []string{"read", "write", "completion", "release"} {
		blockWrite := name == "write"
		blockCompletion := name == "completion"
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			host1, guest1 := net.Pipe()
			defer guest1.Close()
			host2, guest2 := net.Pipe()
			defer guest2.Close()
			physical := &parallelCommandSession{serverTestSession: &serverTestSession{streams: []io.ReadWriteCloser{host1, host2}}, firstOpened: make(chan struct{})}
			mount := workerapi.ComputerInstanceAssignment{OrgID: "org", ComputerID: "computer", ComputerInstanceID: "instance", WriterGeneration: 2, GuestdChannelToken: "token"}
			commands := []workerapi.ComputerCommand{}
			for _, id := range []string{"first", "second"} {
				commands = append(commands, workerapi.ComputerCommand{CommandID: id, ComputerID: mount.ComputerID, ComputerInstanceID: mount.ComputerInstanceID, WriterGeneration: 2, ExpiresAt: time.Now().Add(time.Minute), RequestFingerprint: id, Request: json.RawMessage(`{"command":["true"]}`)})
			}
			firstDone := make(chan error, 1)
			firstRequestRead := make(chan struct{})
			var completionStarted chan struct{}
			if blockCompletion {
				completionStarted = make(chan struct{})
			}
			if !blockWrite {
				go func() {
					_, _, err := wire.ReadStreamFrameHeader(guest1)
					if err == nil {
						if name == "release" {
							var r computerv0.ComputerCommandReleaseRequest
							err = frameio.ReadProtoFrame(guest1, &r)
						} else {
							var r computerv0.ComputerBasicExecRequest
							err = frameio.ReadProtoFrame(guest1, &r)
						}
					}
					if err == nil {
						close(firstRequestRead)
					}
					if err == nil && blockCompletion {
						err = frameio.WriteProtoFrame(guest1, &computerv0.ComputerBasicExecEvent{Event: &computerv0.ComputerBasicExecEvent_Result{Result: &computerv0.ComputerBasicExecResult{Outcome: "exited", RequestFingerprint: "first"}}})
					} else if err == nil {
						var b [1]byte
						_, err = guest1.Read(b[:])
						if errors.Is(err, io.EOF) {
							err = nil
						}
					}
					firstDone <- err
				}()
			}
			secondDone := make(chan error, 1)
			go func() {
				if !blockWrite {
					select {
					case <-firstRequestRead:
					case <-ctx.Done():
						secondDone <- ctx.Err()
						return
					}
				}
				if completionStarted != nil {
					select {
					case <-completionStarted:
					case <-ctx.Done():
						secondDone <- ctx.Err()
						return
					}
				}
				_, _, err := wire.ReadStreamFrameHeader(guest2)
				var r computerv0.ComputerBasicExecRequest
				if err == nil {
					err = frameio.ReadProtoFrame(guest2, &r)
				}
				if err == nil {
					err = frameio.WriteProtoFrame(guest2, &computerv0.ComputerBasicExecEvent{Event: &computerv0.ComputerBasicExecEvent_Result{Result: &computerv0.ComputerBasicExecResult{Outcome: "exited", RequestFingerprint: r.Envelope.RequestFingerprint}}})
				}
				secondDone <- err
			}()
			client := &parallelCommandClient{commands: commands, finish: cancel, completionStarted: completionStarted, firstOpened: physical.firstOpened, releaseFirst: name == "release"}
			m := Server{PollEvery: time.Millisecond}
			renewal := m.startRenewalLoop(ctx, workerapi.ComputerInstanceRenewRequest{}, client, time.Hour, time.Now().Add(time.Hour))
			err := m.serveComputerMount(ctx, renewal, newInstanceMount(physical), nil, mount, client, nil)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("serve: %v", err)
			}
			if len(client.execCompletions) != 1 || client.execCompletions[0].CommandID != "second" {
				t.Fatalf("completions: %+v", client.execCompletions)
			}
			if len(physical.openedStreams()) != 2 || physical.closeCount() != 0 {
				t.Fatal("unexpected duplicate dispatch or Computer close")
			}
			if err := <-secondDone; err != nil {
				t.Fatal(err)
			}
			if !blockWrite {
				if err := <-firstDone; err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
