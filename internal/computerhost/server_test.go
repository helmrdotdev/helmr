package computerhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"io"
	"net"
	"net/http"

	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"

	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"

	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestServerDispatchesBasicExec(t *testing.T) {
	clientStream, guestStream := net.Pipe()
	defer guestStream.Close()
	machine := &serverTestMachine{operation: clientStream}
	secretValue := []byte("secret-value")
	exec := workerapi.ComputerCommand{
		CommandID:          "process-1",
		ComputerID:         "computer-1",
		ComputerInstanceID: "instance-1",
		RequestFingerprint: strings.Repeat("a", 64),
		Request:            json.RawMessage(`{"command":["sh","-c","printf ok"],"cwd":"/workspace","env":{},"timeout_ms":1000}`),
		Stdin:              []byte("input"),
		Secrets:            []workerapi.SecretDelivery{{Env: &workerapi.SecretEnv{Name: "TOKEN"}, Value: secretValue}},
		WriterGeneration:   4,
		ExpiresAt:          time.Now().Add(time.Minute),
	}
	guestDone := make(chan error, 1)
	go func() {
		header, _, err := wire.ReadStreamFrameHeader(guestStream)
		if err != nil {
			guestDone <- err
			return
		}
		if header.Type != wire.StreamTypeComputerBasicExec ||
			header.OperationID != exec.CommandID {
			guestDone <- fmt.Errorf("unexpected header: %+v", header)
			return
		}
		var request computerv0.ComputerBasicExecRequest
		if err := frameio.ReadProtoFrame(guestStream, &request); err != nil {
			guestDone <- err
			return
		}
		if request.GetEnvelope().GetComputerInstanceId() != exec.ComputerInstanceID || request.GetEnvelope().GetWriterGeneration() != exec.WriterGeneration ||
			request.GetEnvelope().GetChannelCredential() != "channel-credential" ||
			string(request.GetStdin()) != "input" ||
			len(request.GetSecrets()) != 1 ||
			request.GetSecrets()[0].GetPlacementKind() != "env" ||
			request.GetSecrets()[0].GetPlacementTarget() != "TOKEN" ||
			string(request.GetSecrets()[0].GetValue()) != "secret-value" {
			guestDone <- fmt.Errorf("unexpected BasicExec request: %+v", &request)
			return
		}
		defer guestStream.Close()
		for _, stream := range []string{"stdout", "stderr"} {
			chunk := outputChunk(stream, 1, "data")
			chunk.Content = []byte(stream)
			if err := frameio.WriteProtoFrame(guestStream, outputEvent(chunk)); err != nil {
				guestDone <- err
				return
			}
			var ack computerv0.CommandOutputAck
			if err := frameio.ReadProtoFrame(guestStream, &ack); err != nil {
				guestDone <- err
				return
			}
		}
		for _, stream := range []string{"stdout", "stderr"} {
			if err := frameio.WriteProtoFrame(guestStream, outputEvent(outputChunk(stream, 2, "end"))); err != nil {
				guestDone <- err
				return
			}
			var ack computerv0.CommandOutputAck
			if err := frameio.ReadProtoFrame(guestStream, &ack); err != nil {
				guestDone <- err
				return
			}
		}
		guestDone <- frameio.WriteProtoFrame(guestStream, &computerv0.ComputerBasicExecEvent{Event: &computerv0.ComputerBasicExecEvent_Result{Result: &computerv0.ComputerBasicExecResult{
			ExitCode: 7, Outcome: "exited", RequestFingerprint: exec.RequestFingerprint,
			Stdout: &computerv0.CommandOutputBoundary{ThroughSequence: 2, Complete: true}, Stderr: &computerv0.CommandOutputBoundary{ThroughSequence: 2, Complete: true},
		}}})
	}()
	client := &serverTestClient{}
	completion, err := (commandService{}).dispatchComputerBasicExec(
		context.Background(),
		machine,
		commandAuthority{
			EnvironmentID: "org-1", ComputerID: "computer-1", ComputerInstanceID: "instance-1", WriterGeneration: 4,
			GuestChannelCredential: "channel-credential",
		},
		exec, client,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-guestDone; err != nil {
		t.Fatal(err)
	}
	if completion.CommandID != exec.CommandID ||
		completion.ComputerInstanceID != "instance-1" || completion.WriterGeneration != exec.WriterGeneration ||
		completion.ExitCode == nil ||
		*completion.ExitCode != 7 ||
		len(client.commandLogs) != 4 ||
		string(client.commandLogs[0].Content) != "stdout" ||
		string(client.commandLogs[1].Content) != "stderr" ||
		completion.Outcome != "exited" {
		t.Fatalf("completion = %+v", completion)
	}
	for _, value := range secretValue {
		if value != 0 {
			t.Fatal("Secret plaintext was not cleared after dispatch")
		}
	}
}

func TestServerRejectsMismatchedBasicExecClaim(t *testing.T) {
	_, err := (commandService{}).dispatchComputerBasicExec(
		context.Background(),
		&serverTestMachine{},
		commandAuthority{
			ComputerID:             "computer-1",
			GuestChannelCredential: "channel-credential",
		},
		workerapi.ComputerCommand{
			CommandID: "process-1", ComputerInstanceID: "instance-2",
			ComputerID: "computer-1", RequestFingerprint: strings.Repeat("a", 64),
			WriterGeneration: 1,
			ExpiresAt:        time.Now().Add(time.Minute),
		}, &serverTestClient{},
	)
	var protocolError *computerBasicExecProtocolError
	if !errors.As(err, &protocolError) {
		t.Fatalf("error = %v, want protocol error", err)
	}
}

func TestServerRejectsGuestAuthorityOutcomes(t *testing.T) {
	for _, outcome := range []string{
		"computer_command_fenced",
		"computer_command_expired",
		"computer_command_invalid",
		"computer_command_fingerprint_conflict",
		"computer_command_unavailable",
		"future_outcome",
		"",
	} {
		t.Run(outcome, func(t *testing.T) {
			var protocolError *computerBasicExecProtocolError
			if err := validateComputerBasicCommandOutcome(outcome); !errors.As(err, &protocolError) {
				t.Fatalf("error = %v, want protocol error", err)
			}
		})
	}
}

func TestServerCompletionStopsOnNonRetryableError(t *testing.T) {
	client := &serverTestClient{
		completeErrors: []error{serverHTTPError(http.StatusBadRequest)},
	}
	err := (commandService{
		CompleteErrorBackoff: time.Nanosecond,
	}).completeComputerBasicExec(
		context.Background(),
		client,
		workerapi.ComputerCommandCompleteRequest{},
	)
	if err == nil {
		t.Fatal("non-retryable completion error was ignored")
	}
	if len(client.execCompletions) != 1 {
		t.Fatalf("completion attempts = %d, want 1", len(client.execCompletions))
	}
}

func TestServerCompletionRetriesServerError(t *testing.T) {
	client := &serverTestClient{
		completeErrors: []error{
			serverHTTPError(http.StatusServiceUnavailable),
		},
	}
	err := (commandService{
		CompleteErrorBackoff: time.Nanosecond,
	}).completeComputerBasicExec(
		context.Background(),
		client,
		workerapi.ComputerCommandCompleteRequest{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(client.execCompletions) != 2 {
		t.Fatalf("completion attempts = %d, want 2", len(client.execCompletions))
	}
}

type serverTestMachine struct {
	unusedCheckpoint
	mu        sync.Mutex
	operation io.ReadWriteCloser
	streams   []io.ReadWriteCloser
	opened    []io.ReadWriteCloser
	exit      <-chan error
	closeErr  error
	closed    int
	captures  int
}

func (s *serverTestMachine) Stream() vm.Stream {
	return nil
}

func (s *serverTestMachine) OpenStream(context.Context) (vm.Stream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed > 0 {
		return nil, errors.New("test machine is closed")
	}
	if len(s.streams) > 0 {
		stream := s.streams[0]
		s.streams = s.streams[1:]
		s.opened = append(s.opened, stream)
		return testVMStream(stream), nil
	}
	s.opened = append(s.opened, s.operation)
	return testVMStream(s.operation), nil
}

func (s *serverTestMachine) Close(context.Context) error {
	s.mu.Lock()
	s.closed++
	operation := s.operation
	opened := append([]io.ReadWriteCloser(nil), s.opened...)
	streams := append([]io.ReadWriteCloser(nil), s.streams...)
	closeErr := s.closeErr
	s.mu.Unlock()
	if operation != nil {
		_ = operation.Close()
	}
	for _, stream := range opened {
		if stream != nil {
			_ = stream.Close()
		}
	}
	for _, stream := range streams {
		_ = stream.Close()
	}
	return closeErr
}

func (s *serverTestMachine) CaptureComputer(context.Context) (*vm.ComputerSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captures++
	return nil, errTestLiveCapture
}

func (s *serverTestMachine) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *serverTestMachine) openedStreams() []io.ReadWriteCloser {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]io.ReadWriteCloser(nil), s.opened...)
}

func (s *serverTestMachine) Wait(ctx context.Context) error {
	if s.exit == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case err := <-s.exit:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type serverTestClient struct {
	commandMu       sync.Mutex
	commandLogs     []workerapi.CommandLogAppendRequest
	appendErrors    []error
	cancel          context.CancelFunc
	computerCommand *workerapi.ComputerCommand
	execClaims      []workerapi.ComputerCommandClaimRequest
	execCompletions []workerapi.ComputerCommandCompleteRequest
	completeErrors  []error
	onReady         func()
	readyOnce       sync.Once
}

func (c *serverTestClient) ClaimComputerCommand(_ context.Context, request workerapi.ComputerCommandClaimRequest) (workerapi.ComputerCommandClaimResponse, error) {
	c.commandMu.Lock()
	defer c.commandMu.Unlock()
	c.execClaims = append(c.execClaims, request)
	c.readyOnce.Do(func() {
		if c.onReady != nil {
			c.onReady()
		}
	})
	return workerapi.ComputerCommandClaimResponse{Command: c.computerCommand}, nil
}

func (c *serverTestClient) CompleteComputerCommand(_ context.Context, request workerapi.ComputerCommandCompleteRequest) error {
	c.commandMu.Lock()
	defer c.commandMu.Unlock()
	c.execCompletions = append(c.execCompletions, request)
	if len(c.completeErrors) > 0 {
		err := c.completeErrors[0]
		c.completeErrors = c.completeErrors[1:]
		return err
	}
	c.computerCommand = nil
	if c.cancel != nil {
		c.cancel()
	}
	return nil
}

type serverHTTPError int

func (e serverHTTPError) Error() string {
	return http.StatusText(int(e))
}

func (e serverHTTPError) HTTPStatusCode() int {
	return int(e)
}

func (c *serverTestClient) AppendCommandLog(_ context.Context, request workerapi.CommandLogAppendRequest) (workerapi.DiagnosticLogReceipt, error) {
	c.commandMu.Lock()
	defer c.commandMu.Unlock()
	c.commandLogs = append(c.commandLogs, request)
	if len(c.appendErrors) > 0 {
		err := c.appendErrors[0]
		c.appendErrors = c.appendErrors[1:]
		if err != nil {
			return workerapi.DiagnosticLogReceipt{}, err
		}
	}
	now := time.Now()
	return workerapi.DiagnosticLogReceipt{ThroughSequence: int64(request.ThroughSequence), AcceptedAt: now, ExpiresAt: now.Add(90 * 24 * time.Hour)}, nil
}

func (c *serverTestClient) ReconcileComputerCommand(context.Context, workerapi.ComputerCommandCompleteRequest) error {
	return nil
}
