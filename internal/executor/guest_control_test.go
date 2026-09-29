package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

type guestControlTestMachine struct {
	stream  vm.Stream
	openErr error
	closed  bool
}

func (m *guestControlTestMachine) Stream() vm.Stream { return nil }
func (m *guestControlTestMachine) OpenStream(context.Context) (vm.Stream, error) {
	if m.openErr != nil {
		return nil, m.openErr
	}
	return m.stream, nil
}
func (m *guestControlTestMachine) Wait(context.Context) error { return nil }
func (m *guestControlTestMachine) Close(context.Context) error {
	m.closed = true
	return nil
}

// guestControlFailingStream accepts writeLimit bytes, fails later writes and
// returns EOF from reads.
type guestControlFailingStream struct {
	mu         sync.Mutex
	writeLimit int
	written    int
	closes     int
}

var errGuestControlTestWrite = errors.New("guest control test write failed")

func (s *guestControlFailingStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.written+len(p) > s.writeLimit {
		return 0, errGuestControlTestWrite
	}
	s.written += len(p)
	return len(p), nil
}

func (s *guestControlFailingStream) Read([]byte) (int, error) { return 0, io.EOF }

func (s *guestControlFailingStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closes++
	return nil
}

func (s *guestControlFailingStream) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes
}

func TestGuestControlExchangeFramesHeaderAndRequest(t *testing.T) {
	for _, readWithContext := range []bool{false, true} {
		for _, closeOnCancel := range []guestControlCloseOnCancel{guestControlCloseOnCancelNone, guestControlCloseOnCancelAsync, guestControlCloseOnCancelAwait} {
			client, server := net.Pipe()
			header := wire.StreamHeader{Type: wire.StreamTypeComputerRestoreVerify, ComputerID: "computer", ComputerInstanceID: "instance", CheckpointID: "checkpoint", RunID: "run", OperationID: "operation"}
			request := &computerv0.VerifyComputerRestoreRequest{Identity: &computerv0.ComputerRestoreIdentity{ComputerId: "computer", CheckpointId: "checkpoint", WriterGeneration: 4}}
			done := make(chan error, 1)
			go func() {
				defer server.Close()
				got, size, err := wire.ReadStreamFrameHeader(server)
				if err != nil {
					done <- err
					return
				}
				if got != header || size != 0 {
					done <- errors.New("stream header changed")
					return
				}
				var received computerv0.VerifyComputerRestoreRequest
				if err := frameio.ReadProtoFrame(server, &received); err != nil {
					done <- err
					return
				}
				if !proto.Equal(&received, request) {
					done <- errors.New("request frame changed")
					return
				}
				if err := frameio.WriteProtoFrame(server, &computerv0.VerifyComputerRestoreResponse{Identity: received.Identity}); err != nil {
					done <- err
					return
				}
				// The exchange closes its stream once the response is read.
				_, err = server.Read(make([]byte, 1))
				if !errors.Is(err, io.EOF) {
					done <- errors.New("stream stayed open after the response")
					return
				}
				done <- nil
			}()
			machine := &guestControlTestMachine{stream: client}
			var response computerv0.VerifyComputerRestoreResponse
			err := guestControl{machine: machine}.exchange(t.Context(), guestControlExchange{header: header, request: request, response: &response, closeOnCancel: closeOnCancel, readWithContext: readWithContext})
			if err != nil {
				t.Fatalf("exchange: %v", err)
			}
			if !proto.Equal(response.Identity, request.Identity) {
				t.Fatalf("response=%v", &response)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("guest did not observe the complete exchange")
			}
			if machine.closed {
				t.Fatal("exchange closed the machine")
			}
		}
	}
}

func TestGuestControlExchangeReportsFailedStepAndClosesStream(t *testing.T) {
	header := wire.StreamHeader{Type: wire.StreamTypeComputerRunCleanup, RunID: "run"}
	var headerBytes bytes.Buffer
	if err := wire.WriteStreamFrameHeader(&headerBytes, header, 0); err != nil {
		t.Fatal(err)
	}
	request := &computerv0.ComputerRunCleanupRequest{ComputerId: "computer", RunId: "run"}
	for _, test := range []struct {
		name       string
		openErr    error
		writeLimit int
		step       guestControlStep
		cause      error
	}{
		{name: "open", openErr: errors.New("open failed"), step: guestControlOpen},
		{name: "header", writeLimit: 0, step: guestControlWriteHeader, cause: errGuestControlTestWrite},
		{name: "request", writeLimit: headerBytes.Len(), step: guestControlWriteRequest, cause: errGuestControlTestWrite},
		{name: "response", writeLimit: 1 << 20, step: guestControlReadResponse, cause: io.EOF},
	} {
		t.Run(test.name, func(t *testing.T) {
			cause := test.cause
			if test.openErr != nil {
				cause = test.openErr
			}
			run := func(wrap func(guestControlStep, error) error) (*guestControlFailingStream, error) {
				stream := &guestControlFailingStream{writeLimit: test.writeLimit}
				machine := &guestControlTestMachine{stream: testVMStream(stream), openErr: test.openErr}
				err := guestControl{machine: machine}.exchange(t.Context(), guestControlExchange{header: header, request: request, response: &computerv0.ComputerRunCleanupResponse{}, wrap: wrap})
				return stream, err
			}
			wantCloses := 1
			if test.openErr != nil {
				wantCloses = 0
			}

			stream, err := run(nil)
			if err != cause {
				t.Fatalf("unwrapped error = %v, want %v", err, cause)
			}
			if got := stream.closeCount(); got != wantCloses {
				t.Fatalf("stream closes = %d, want %d", got, wantCloses)
			}

			var steps []guestControlStep
			stream, err = run(func(step guestControlStep, err error) error {
				steps = append(steps, step)
				return errors.Join(errComputerControlTransport, err)
			})
			if !errors.Is(err, errComputerControlTransport) || !errors.Is(err, cause) {
				t.Fatalf("wrapped error = %v", err)
			}
			if len(steps) != 1 || steps[0] != test.step {
				t.Fatalf("steps = %v, want [%v]", steps, test.step)
			}
			if got := stream.closeCount(); got != wantCloses {
				t.Fatalf("stream closes = %d, want %d", got, wantCloses)
			}
		})
	}
}

func TestGuestControlExchangeCancellation(t *testing.T) {
	for _, test := range []struct {
		name          string
		closeOnCancel guestControlCloseOnCancel
		readRequest   bool
		want          error
	}{
		// A context-aware response read reports cancellation itself.
		{name: "response read", readRequest: true, want: context.Canceled},
		{name: "async close during request write", closeOnCancel: guestControlCloseOnCancelAsync},
		{name: "awaited close during request write", closeOnCancel: guestControlCloseOnCancelAwait},
		{name: "awaited close during response read", closeOnCancel: guestControlCloseOnCancelAwait, readRequest: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			machine := &guestControlTestMachine{stream: client}
			done := make(chan error, 1)
			go func() {
				done <- guestControl{machine: machine}.exchange(ctx, guestControlExchange{
					header:          wire.StreamHeader{Type: wire.StreamTypeComputerFreeze, ComputerID: "computer"},
					request:         &computerv0.FreezeComputerRequest{ComputerId: "computer"},
					response:        &computerv0.FreezeComputerResponse{},
					closeOnCancel:   test.closeOnCancel,
					readWithContext: test.closeOnCancel == guestControlCloseOnCancelNone,
				})
			}()
			if _, _, err := wire.ReadStreamFrameHeader(server); err != nil {
				t.Fatal(err)
			}
			if test.readRequest {
				var request computerv0.FreezeComputerRequest
				if err := frameio.ReadProtoFrame(server, &request); err != nil {
					t.Fatal(err)
				}
			}
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("cancelled exchange succeeded")
				}
				if test.want != nil && !errors.Is(err, test.want) {
					t.Fatalf("error = %v, want %v", err, test.want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("blocked stream did not cancel")
			}
			if _, err := server.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatalf("stream remained open after cancellation: %v", err)
			}
			if machine.closed {
				t.Fatal("cancellation closed the machine")
			}
		})
	}
}
