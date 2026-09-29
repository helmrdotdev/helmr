package computerhost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"testing/synctest"
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

// guestControlBlockingCloseStream accepts writes and blocks reads until its
// first Close starts. Every Close then blocks until releaseClose is closed.
type guestControlBlockingCloseStream struct {
	once         sync.Once
	closeStarted chan struct{}
	releaseClose chan struct{}
}

func newGuestControlBlockingCloseStream() *guestControlBlockingCloseStream {
	return &guestControlBlockingCloseStream{closeStarted: make(chan struct{}), releaseClose: make(chan struct{})}
}

func (s *guestControlBlockingCloseStream) Write(p []byte) (int, error) { return len(p), nil }

func (s *guestControlBlockingCloseStream) Read([]byte) (int, error) {
	<-s.closeStarted
	return 0, io.ErrClosedPipe
}

func (s *guestControlBlockingCloseStream) Close() error {
	s.once.Do(func() { close(s.closeStarted) })
	<-s.releaseClose
	return nil
}

var guestControlTestCancellations = []guestControlCancellation{
	guestControlCancelReadOnly,
	guestControlCancelCloseStream,
	guestControlCancelCloseStreamAndRead,
	guestControlCancelAwaitStreamClose,
}

func TestGuestControlExchangeFramesHeaderAndRequest(t *testing.T) {
	for _, cancellation := range guestControlTestCancellations {
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
		_, err := guestControl{machine: machine}.exchange(t.Context(), guestControlExchange{header: header, request: request, response: &response, cancellation: cancellation})
		if err != nil {
			t.Fatalf("cancellation %d: exchange: %v", cancellation, err)
		}
		if !proto.Equal(response.Identity, request.Identity) {
			t.Fatalf("cancellation %d: response=%v", cancellation, &response)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("cancellation %d: %v", cancellation, err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("cancellation %d: guest did not observe the complete exchange", cancellation)
		}
		if machine.closed {
			t.Fatal("exchange closed the machine")
		}
	}
}

func guestControlTestHeaderLength(t *testing.T, header wire.StreamHeader) int {
	t.Helper()
	var buffer bytes.Buffer
	if err := wire.WriteStreamFrameHeader(&buffer, header, 0); err != nil {
		t.Fatal(err)
	}
	return buffer.Len()
}

func TestGuestControlExchangeReportsFailedStepAndClosesStream(t *testing.T) {
	header := wire.StreamHeader{Type: wire.StreamTypeComputerRunCleanup, RunID: "run"}
	openErr := errors.New("open failed")
	for _, test := range []struct {
		name       string
		openErr    error
		writeLimit int
		step       guestControlStep
		cause      error
		closes     int
	}{
		{name: "open", openErr: openErr, step: guestControlOpen, cause: openErr},
		{name: "header", writeLimit: 0, step: guestControlWriteHeader, cause: errGuestControlTestWrite, closes: 1},
		{name: "request", writeLimit: guestControlTestHeaderLength(t, header), step: guestControlWriteRequest, cause: errGuestControlTestWrite, closes: 1},
		{name: "response", writeLimit: 1 << 20, step: guestControlReadResponse, cause: io.EOF, closes: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream := &guestControlFailingStream{writeLimit: test.writeLimit}
			machine := &guestControlTestMachine{stream: testVMStream(stream), openErr: test.openErr}
			step, err := guestControl{machine: machine}.exchange(t.Context(), guestControlExchange{header: header, request: &computerv0.ComputerRunCleanupRequest{ComputerId: "computer", RunId: "run"}, response: &computerv0.ComputerRunCleanupResponse{}})
			if err != test.cause || step != test.step {
				t.Fatalf("step=%d err=%v, want step=%d err=%v", step, err, test.step, test.cause)
			}
			if got := stream.closeCount(); got != test.closes {
				t.Fatalf("stream closes = %d, want %d", got, test.closes)
			}
		})
	}
}

func TestComputerAuthorityRenewalMarksEveryExchangeFailureAsTransport(t *testing.T) {
	fence := &computerv0.ComputerAuthorityFence{RunId: "run", ComputerId: "computer", ComputerInstanceId: "instance", ExpiresAtUnixNano: 1}
	request := &computerv0.RenewComputerAuthorityRequest{Previous: &computerv0.ComputerRunAuthority{Fence: fence}, NewExpiresAtUnixNano: 2}
	header := wire.StreamHeader{Type: wire.StreamTypeComputerAuthorityRenew, RunID: "run", ComputerID: "computer", ComputerInstanceID: "instance"}
	for _, test := range []struct {
		name       string
		openErr    error
		writeLimit int
		want       string
	}{
		{name: "open", openErr: errors.New("open failed"), want: "computer control transport: open computer authority renewal stream: open failed"},
		{name: "header", writeLimit: 0, want: "computer control transport: write computer authority renewal header: guest control test write failed"},
		{name: "request", writeLimit: guestControlTestHeaderLength(t, header), want: "computer control transport: write computer authority renewal request: guest control test write failed"},
		{name: "response", writeLimit: 1 << 20, want: "computer control transport: read computer authority renewal response: EOF"},
	} {
		t.Run(test.name, func(t *testing.T) {
			stream := &guestControlFailingStream{writeLimit: test.writeLimit}
			machine := &guestControlTestMachine{stream: testVMStream(stream), openErr: test.openErr}
			fenceOut, err := guestControl{machine: machine}.renewAuthority(t.Context(), request)
			if fenceOut != nil || !errors.Is(err, ErrControlTransport) || err.Error() != test.want {
				t.Fatalf("renewal = %v, %v; want %q", fenceOut, err, test.want)
			}
		})
	}
}

func TestGuestControlExchangeCancellation(t *testing.T) {
	for _, test := range []struct {
		name         string
		cancellation guestControlCancellation
		readRequest  bool
		want         error
	}{
		{name: "read only during response read", cancellation: guestControlCancelReadOnly, readRequest: true, want: context.Canceled},
		{name: "close stream during request write", cancellation: guestControlCancelCloseStream},
		{name: "close stream during response read", cancellation: guestControlCancelCloseStream, readRequest: true},
		{name: "close stream and read during request write", cancellation: guestControlCancelCloseStreamAndRead},
		{name: "close stream and read during response read", cancellation: guestControlCancelCloseStreamAndRead, readRequest: true},
		{name: "await close during request write", cancellation: guestControlCancelAwaitStreamClose},
		{name: "await close during response read", cancellation: guestControlCancelAwaitStreamClose, readRequest: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer server.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			machine := &guestControlTestMachine{stream: client}
			done := make(chan error, 1)
			go func() {
				_, err := guestControl{machine: machine}.exchange(ctx, guestControlExchange{
					header:       wire.StreamHeader{Type: wire.StreamTypeComputerFreeze, ComputerID: "computer"},
					request:      &computerv0.FreezeComputerRequest{ComputerId: "computer"},
					response:     &computerv0.FreezeComputerResponse{},
					cancellation: test.cancellation,
				})
				done <- err
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

func TestGuestControlReadOnlyCancellationLeavesRequestWriteBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, server := net.Pipe()
		defer server.Close()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := guestControl{machine: &guestControlTestMachine{stream: client}}.exchange(ctx, guestControlExchange{
				header:       wire.StreamHeader{Type: wire.StreamTypeComputerRestoreVerify, ComputerID: "computer"},
				request:      &computerv0.VerifyComputerRestoreRequest{Identity: &computerv0.ComputerRestoreIdentity{ComputerId: "computer"}},
				response:     &computerv0.VerifyComputerRestoreResponse{},
				cancellation: guestControlCancelReadOnly,
			})
			done <- err
		}()
		if _, _, err := wire.ReadStreamFrameHeader(server); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("cancellation cut off the request write: %v", err)
		default:
		}
		var request computerv0.VerifyComputerRestoreRequest
		if err := frameio.ReadProtoFrame(server, &request); err != nil {
			t.Fatalf("request was not delivered: %v", err)
		}
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want %v", err, context.Canceled)
		}
	})
}

func TestGuestControlAwaitedCancellationWaitsForStreamClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stream := newGuestControlBlockingCloseStream()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := guestControl{machine: &guestControlTestMachine{stream: testVMStream(stream)}}.exchange(ctx, guestControlExchange{
				header:       wire.StreamHeader{Type: wire.StreamTypeComputerCommandCancel, OperationID: "command"},
				request:      &computerv0.ComputerCommandCancelRequest{},
				response:     &computerv0.ComputerCommandCancelResponse{},
				cancellation: guestControlCancelAwaitStreamClose,
			})
			done <- err
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		select {
		case <-stream.closeStarted:
		default:
			t.Fatal("cancellation did not close the stream")
		}
		select {
		case err := <-done:
			t.Fatalf("exchange returned before its stream close finished: %v", err)
		default:
		}
		close(stream.releaseClose)
		if err := <-done; err == nil {
			t.Fatal("cancelled exchange succeeded")
		}
	})
}
