package computerhost

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func commandOutputFrames(t *testing.T, chunks ...*computerv0.CommandOutputChunk) []byte {
	t.Helper()
	var frames bytes.Buffer
	for _, chunk := range chunks {
		if err := frameio.WriteProtoFrame(&frames, &computerv0.ComputerBasicExecEvent{Event: &computerv0.ComputerBasicExecEvent_Output{Output: chunk}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := frameio.WriteProtoFrame(&frames, &computerv0.ComputerBasicExecEvent{Event: &computerv0.ComputerBasicExecEvent_Result{Result: &computerv0.ComputerBasicExecResult{Outcome: "exited", ExitCode: 7}}}); err != nil {
		t.Fatal(err)
	}
	return frames.Bytes()
}

func TestCommandOutputAppendRetryPreservesIdentity(t *testing.T) {
	chunk := &computerv0.CommandOutputChunk{Stream: "stdout", ObservedAtUnixNano: time.Now().UnixNano(), Content: []byte{0, 255, 128, 10}}
	client := &serverTestClient{appendErrors: []error{serverHTTPError(503), nil}}
	server := Server{CompleteErrorBackoff: time.Nanosecond}
	mount := workerapi.ComputerInstanceAssignment{OrgID: "org", ComputerInstanceID: "instance"}
	command := workerapi.ComputerCommand{CommandID: "command", WriterGeneration: 4}
	frames := commandOutputFrames(t, chunk)
	for range 2 {
		result, err := server.readCommandOutput(t.Context(), io.NopCloser(bytes.NewReader(frames)), mount, command, client)
		if err != nil || result.GetExitCode() != 7 {
			t.Fatalf("%v %v", result, err)
		}
	}
	if len(client.commandLogs) != 3 {
		t.Fatal(client.commandLogs)
	}
	for _, request := range client.commandLogs {
		if !reflect.DeepEqual(request, client.commandLogs[0]) {
			t.Fatal("retry changed chunk identity")
		}
		if request.OrgID != "org" || request.ComputerInstanceID != "instance" || request.WriterGeneration != 4 || !bytes.Equal(request.Content, chunk.Content) {
			t.Fatal(request)
		}
	}
}

func TestCommandOutputRejectedAppendCannotReturnResult(t *testing.T) {
	client := &serverTestClient{appendErrors: []error{serverHTTPError(409)}}
	chunk := &computerv0.CommandOutputChunk{Stream: "stdout", ObservedAtUnixNano: 1, Content: []byte("out")}
	result, err := (Server{}).readCommandOutput(t.Context(), io.NopCloser(bytes.NewReader(commandOutputFrames(t, chunk))), workerapi.ComputerInstanceAssignment{}, workerapi.ComputerCommand{}, client)
	var rejected *computerBasicExecProtocolError
	if result != nil || !errors.As(err, &rejected) || len(client.commandLogs) != 1 {
		t.Fatalf("%v %v", result, err)
	}
}

type blockedCommandLogClient struct {
	serverTestClient
	entered chan struct{}
	release chan struct{}
}

func (c *blockedCommandLogClient) AppendCommandLog(ctx context.Context, _ workerapi.CommandLogAppendRequest) error {
	close(c.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.release:
		return nil
	}
}

func TestCommandOutputWaitsForAcknowledgementBeforeReadingResult(t *testing.T) {
	client := &blockedCommandLogClient{entered: make(chan struct{}), release: make(chan struct{})}
	reader, writer := net.Pipe()
	defer reader.Close()
	defer writer.Close()
	done := make(chan error, 1)
	go func() {
		_, err := (Server{}).readCommandOutput(t.Context(), reader, workerapi.ComputerInstanceAssignment{}, workerapi.ComputerCommand{}, client)
		done <- err
	}()
	chunk := &computerv0.CommandOutputChunk{Stream: "stderr", ObservedAtUnixNano: 1, Content: []byte("last")}
	frames := commandOutputFrames(t, chunk)
	sent := make(chan error, 1)
	go func() { _, err := writer.Write(frames); sent <- err }()
	<-client.entered
	select {
	case err := <-done:
		t.Fatalf("returned before ack: %v", err)
	default:
	}
	select {
	case err := <-sent:
		t.Fatalf("read terminal frame before ack: %v", err)
	default:
	}
	close(client.release)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCommandOutputCancellationUnblocksRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	reader, writer := net.Pipe()
	defer writer.Close()
	done := make(chan error, 1)
	go func() {
		_, err := (Server{}).readCommandOutput(ctx, reader, workerapi.ComputerInstanceAssignment{}, workerapi.ComputerCommand{}, &serverTestClient{})
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCommandOutputRejectsSequenceGap(t *testing.T) {
	chunk := &computerv0.CommandOutputChunk{Stream: "stdout", Sequence: 1, ObservedAtUnixNano: 1, Content: []byte("out")}
	client := &serverTestClient{}
	result, err := (Server{}).readCommandOutput(t.Context(), io.NopCloser(bytes.NewReader(commandOutputFrames(t, chunk))), workerapi.ComputerInstanceAssignment{}, workerapi.ComputerCommand{}, client)
	if result != nil || err == nil || len(client.commandLogs) != 0 {
		t.Fatalf("%v %v", result, err)
	}
}
