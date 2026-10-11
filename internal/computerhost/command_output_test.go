package computerhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func outputEvent(chunk *computerv0.CommandOutputChunk) *computerv0.ComputerBasicExecEvent {
	return &computerv0.ComputerBasicExecEvent{Event: &computerv0.ComputerBasicExecEvent_Output{Output: chunk}}
}
func outputResult() *computerv0.ComputerBasicExecEvent {
	return &computerv0.ComputerBasicExecEvent{Event: &computerv0.ComputerBasicExecEvent_Result{Result: &computerv0.ComputerBasicExecResult{Outcome: "exited", ExitCode: 7, Stdout: &computerv0.CommandOutputBoundary{ThroughSequence: 2, Complete: true}, Stderr: &computerv0.CommandOutputBoundary{ThroughSequence: 1, Complete: true}}}}
}
func outputChunk(stream string, seq uint64, kind string) *computerv0.CommandOutputChunk {
	c := &computerv0.CommandOutputChunk{Stream: stream, Sequence: seq, ThroughSequence: seq, Kind: kind, ObservedAtUnixNano: time.Now().UnixNano()}
	if kind == "data" {
		c.Content = []byte{0, 255, 128, 10}
	} else {
		c.Complete = true
	}
	return c
}
func writeOutput(t *testing.T, c net.Conn, e *computerv0.ComputerBasicExecEvent) {
	t.Helper()
	if err := frameio.WriteProtoFrame(c, e); err != nil {
		t.Fatal(err)
	}
}
func readOutputAck(t *testing.T, c net.Conn) *computerv0.CommandOutputAck {
	t.Helper()
	a := new(computerv0.CommandOutputAck)
	if err := frameio.ReadProtoFrame(c, a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestCommandOutputAppendRetryPreservesIdentity(t *testing.T) {
	client := &serverTestClient{appendErrors: []error{serverHTTPError(503), nil}}
	service := commandService{CompleteErrorBackoff: time.Nanosecond}
	mount := commandAuthority{EnvironmentID: "env", ComputerInstanceID: "instance"}
	command := workerapi.ComputerCommand{CommandID: "command", WriterGeneration: 4}
	chunk := outputChunk("stdout", 1, "data")
	request := workerapi.CommandLogAppendRequest{EnvironmentID: mount.EnvironmentID, CommandID: command.CommandID, ComputerInstanceID: mount.ComputerInstanceID, WriterGeneration: 4, Kind: chunk.Kind, Stream: "stdout", ObservedSeq: 1, ThroughSequence: 1, ObservedAt: time.Unix(0, chunk.ObservedAtUnixNano), Content: chunk.Content}
	for range 2 {
		receipt, err := service.appendCommandOutput(t.Context(), client, request)
		if err != nil || receipt.ThroughSequence != 1 {
			t.Fatalf("%v %v", receipt, err)
		}
	}
	if len(client.commandLogs) != 3 {
		t.Fatal(client.commandLogs)
	}
	for _, got := range client.commandLogs {
		if !reflect.DeepEqual(got, request) || !bytes.Equal(got.Content, chunk.Content) {
			t.Fatal("retry changed identity")
		}
	}
}

type blockedCommandLogClient struct {
	serverTestClient
	entered chan struct{}
	release chan struct{}
}

func (c *blockedCommandLogClient) AppendCommandLog(ctx context.Context, r workerapi.CommandLogAppendRequest) (workerapi.DiagnosticLogReceipt, error) {
	if r.Stream == "stdout" && r.Kind == "data" {
		close(c.entered)
		select {
		case <-ctx.Done():
			return workerapi.DiagnosticLogReceipt{}, ctx.Err()
		case <-c.release:
		}
	}
	return c.serverTestClient.AppendCommandLog(ctx, r)
}
func TestCommandOutputPendingPipeDoesNotHidePeerOrTerminal(t *testing.T) {
	client := &blockedCommandLogClient{entered: make(chan struct{}), release: make(chan struct{})}
	host, guest := net.Pipe()
	defer guest.Close()
	_ = guest.SetDeadline(time.Now().Add(5 * time.Second))
	terminal := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := (commandService{}).readCommandOutput(t.Context(), host, commandAuthority{}, workerapi.ComputerCommand{}, client, func(*computerv0.ComputerBasicExecResult) error { close(terminal); return nil })
		done <- err
	}()
	writeOutput(t, guest, outputEvent(outputChunk("stdout", 1, "data")))
	<-client.entered
	writeOutput(t, guest, outputEvent(outputChunk("stderr", 1, "end")))
	ack := readOutputAck(t, guest)
	if ack.Stream != "stderr" || ack.Disposition != "accepted" {
		t.Fatal(ack)
	}
	writeOutput(t, guest, outputResult())
	select {
	case <-terminal:
	case <-time.After(time.Second):
		t.Fatal("pending append blocked process exit")
	}
	close(client.release)
	ack = readOutputAck(t, guest)
	if ack.Stream != "stdout" || ack.ThroughSequence != 1 {
		t.Fatal(ack)
	}
	writeOutput(t, guest, outputEvent(outputChunk("stdout", 2, "end")))
	readOutputAck(t, guest)
	guest.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
func TestCommandOutputCancellationUnblocksRead(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	host, guest := net.Pipe()
	defer guest.Close()
	done := make(chan error, 1)
	go func() {
		_, err := (commandService{}).readCommandOutput(ctx, host, commandAuthority{}, workerapi.ComputerCommand{}, &serverTestClient{}, nil)
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type receiptCommandClient struct {
	serverTestClient
	receipt workerapi.DiagnosticLogReceipt
}

func (c *receiptCommandClient) AppendCommandLog(context.Context, workerapi.CommandLogAppendRequest) (workerapi.DiagnosticLogReceipt, error) {
	return c.receipt, nil
}
func TestCommandOutputReceiptValidation(t *testing.T) {
	now := time.Now()
	request := workerapi.CommandLogAppendRequest{ThroughSequence: 3}
	for _, receipt := range []workerapi.DiagnosticLogReceipt{{ThroughSequence: 2, Expired: true}, {ThroughSequence: 3}, {ThroughSequence: 3, Expired: true, AcceptedAt: now}, {ThroughSequence: 3, AcceptedAt: now, ExpiresAt: now.Add(time.Hour)}} {
		if _, err := (commandService{}).appendCommandOutput(t.Context(), &receiptCommandClient{receipt: receipt}, request); err == nil {
			t.Fatal("accepted malformed receipt", receipt)
		}
	}
	receipt, err := (commandService{}).appendCommandOutput(t.Context(), &receiptCommandClient{receipt: workerapi.DiagnosticLogReceipt{ThroughSequence: 3, Expired: true}}, request)
	if err != nil || !receipt.Expired {
		t.Fatal(receipt, err)
	}
}
func TestCommandOutputTerminalFrontierMismatch(t *testing.T) {
	host, guest := net.Pipe()
	defer guest.Close()
	_ = guest.SetDeadline(time.Now().Add(5 * time.Second))
	done := make(chan error, 1)
	go func() {
		_, err := (commandService{}).readCommandOutput(t.Context(), host, commandAuthority{}, workerapi.ComputerCommand{}, &serverTestClient{}, nil)
		done <- err
	}()
	writeOutput(t, guest, outputEvent(outputChunk("stdout", 3, "end")))
	readOutputAck(t, guest)
	writeOutput(t, guest, outputResult())
	if err := <-done; err == nil {
		t.Fatal("accepted result behind end")
	}
}

func TestCommandOutputRejectsPrematureClose(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		t.Run(fmt.Sprint(acknowledged), func(t *testing.T) {
			client := &blockedCommandLogClient{entered: make(chan struct{}), release: make(chan struct{})}
			host, guest := net.Pipe()
			defer guest.Close()
			_ = guest.SetDeadline(time.Now().Add(5 * time.Second))
			done := make(chan error, 1)
			go func() {
				_, err := (commandService{}).readCommandOutput(t.Context(), host, commandAuthority{}, workerapi.ComputerCommand{}, client, nil)
				done <- err
			}()
			writeOutput(t, guest, outputEvent(outputChunk("stdout", 1, "data")))
			<-client.entered
			if acknowledged {
				close(client.release)
				readOutputAck(t, guest)
			}
			writeOutput(t, guest, outputResult())
			guest.Close()
			if err := <-done; err == nil {
				t.Fatal("incomplete output accepted")
			}
		})
	}
}
