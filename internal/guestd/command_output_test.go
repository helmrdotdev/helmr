package guestd

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

func testCommandOutput(t *testing.T, limits diagnosticLimits) *commandOutputSpool {
	t.Helper()
	s, err := newCommandOutputSpool(limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	return s
}
func commandSpoolBytes(t *testing.T, s *commandOutputSpool, stream string) []byte {
	t.Helper()
	var result []byte
	for {
		r, ok := s.pipes[stream].peek()
		if !ok {
			return result
		}
		result = append(result, r.Data...)
		if err := s.pipes[stream].acknowledge(r.Through); err != nil {
			t.Fatal(err)
		}
	}
}
func attachCommand(t *testing.T, e *computerBasicExec) (net.Conn, <-chan error) {
	t.Helper()
	host, guest := net.Pipe()
	_ = host.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { host.Close() })
	done := make(chan error, 1)
	go func() { done <- e.streamOutput(t.Context(), guest) }()
	return host, done
}
func readCommandEvent(t *testing.T, c net.Conn) *computerv0.ComputerBasicExecEvent {
	t.Helper()
	event := new(computerv0.ComputerBasicExecEvent)
	if err := frameio.ReadProtoFrame(c, event); err != nil {
		t.Fatal(err)
	}
	return event
}
func ackCommand(t *testing.T, c net.Conn, chunk *computerv0.CommandOutputChunk) {
	t.Helper()
	if err := frameio.WriteProtoFrame(c, &computerv0.CommandOutputAck{Stream: chunk.Stream, ThroughSequence: chunk.ThroughSequence, Disposition: "accepted"}); err != nil {
		t.Fatal(err)
	}
}

func TestCommandOutputReplaysOnlyUnacknowledgedHead(t *testing.T) {
	s := testCommandOutput(t, diagnosticLimits{4, 8, 2})
	e := &computerBasicExec{output: s, done: make(chan struct{})}
	payload := []byte{0, 255, 10, 128}
	if err := s.append("stdout", payload); err != nil {
		t.Fatal(err)
	}
	c, done := attachCommand(t, e)
	first := readCommandEvent(t, c)
	c.Close()
	if err := <-done; err == nil {
		t.Fatal("disconnect succeeded")
	}
	c, done = attachCommand(t, e)
	replay := readCommandEvent(t, c)
	if !proto.Equal(first, replay) {
		t.Fatalf("head changed %v / %v", first, replay)
	}
	ackCommand(t, c, replay.GetOutput())
	// Once acknowledged, the successor is retained across the next disconnect.
	if err := s.append("stdout", []byte("next")); err != nil {
		t.Fatal(err)
	}
	next := readCommandEvent(t, c)
	if next.GetOutput().Sequence != 2 {
		t.Fatal(next)
	}
	c.Close()
	<-done
	c, done = attachCommand(t, e)
	again := readCommandEvent(t, c)
	if !proto.Equal(next, again) {
		t.Fatal("successor replay changed")
	}
	ackCommand(t, c, again.GetOutput())
	c.Close()
	<-done
}

func TestCommandOutputTerminalIndependentOfPendingPipes(t *testing.T) {
	s := testCommandOutput(t, diagnosticLimits{4, 4, 1})
	e := &computerBasicExec{output: s, done: make(chan struct{})}
	for _, stream := range []string{"stdout", "stderr"} {
		if err := s.append(stream, []byte("data")); err != nil {
			t.Fatal(err)
		}
	}
	c, done := attachCommand(t, e)
	first := readCommandEvent(t, c)
	second := readCommandEvent(t, c)
	if first.GetOutput().Stream == second.GetOutput().Stream {
		t.Fatal("peer pipe starved")
	}
	s.finish(true)
	e.result = &computerv0.ComputerBasicExecResult{Outcome: "exited", Stdout: s.boundaries["stdout"], Stderr: s.boundaries["stderr"]}
	close(e.done)
	terminal := readCommandEvent(t, c)
	if terminal.GetResult().Outcome != "exited" {
		t.Fatal("pending output hid terminal")
	}
	if s.settled() {
		t.Fatal("unacknowledged pipes settled")
	}
	ackCommand(t, c, first.GetOutput())
	ackCommand(t, c, second.GetOutput())
	for range 2 {
		end := readCommandEvent(t, c).GetOutput()
		if end.Kind != "end" || !end.Complete {
			t.Fatal(end)
		}
		ackCommand(t, c, end)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !s.settled() {
		t.Fatal("end ACKs not retained")
	}
}

func TestCommandOutputSaturationPreservesBinaryPrefixAndGap(t *testing.T) {
	s := testCommandOutput(t, diagnosticLimits{4, 4, 1})
	payload := []byte{0, 255, 128, 10}
	var writers sync.WaitGroup
	for _, stream := range []string{"stdout", "stderr"} {
		writers.Go(func() {
			w := commandOutputWriter{spool: s, stream: stream}
			if n, err := w.Write(append(append([]byte{}, payload...), bytes.Repeat([]byte("x"), 12)...)); err != nil || n != 16 {
				t.Errorf("write %d %v", n, err)
			}
		})
	}
	writers.Wait()
	s.finish(true)
	for _, stream := range []string{"stdout", "stderr"} {
		p := s.pipes[stream]
		r, _ := p.peek()
		if !bytes.Equal(r.Data, payload) {
			t.Fatal(r)
		}
		_ = p.acknowledge(r.Through)
		gap, _ := p.peek()
		if gap.Kind != diagnosticGap || gap.Sequence != 2 || gap.Through != 4 || gap.DroppedBytes != 12 {
			t.Fatal(gap)
		}
		_ = p.acknowledge(gap.Through)
		end, _ := p.peek()
		if end.Kind != diagnosticEnd || end.Sequence != 5 || !end.Complete || !s.boundaries[stream].Gapped {
			t.Fatal(end)
		}
	}
}

func TestCommandOutputReplacementFencesOldAttachment(t *testing.T) {
	s := testCommandOutput(t, diagnosticLimits{4, 4, 1})
	e := &computerBasicExec{output: s, done: make(chan struct{})}
	_ = s.append("stdout", []byte("head"))
	old, oldDone := attachCommand(t, e)
	first := readCommandEvent(t, old)
	current, currentDone := attachCommand(t, e)
	replay := readCommandEvent(t, current)
	if !proto.Equal(first, replay) {
		t.Fatal("replacement changed head")
	}
	if err := <-oldDone; err == nil {
		t.Fatal("old attachment succeeded")
	}
	if err := frameio.WriteProtoFrame(old, &computerv0.CommandOutputAck{Stream: "stdout", ThroughSequence: 1, Disposition: "accepted"}); err == nil {
		t.Fatal("old transport accepted ACK")
	}
	r, _ := s.pipes["stdout"].peek()
	if r.Sequence != 1 {
		t.Fatal("old ACK released head")
	}
	ackCommand(t, current, replay.GetOutput())
	current.Close()
	<-currentDone
}

func TestCommandOutputObservationCancellation(t *testing.T) {
	s := testCommandOutput(t, diagnosticLimits{16, 16, 1})
	e := &computerBasicExec{output: s, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	host, guest := net.Pipe()
	defer host.Close()
	defer guest.Close()
	if err := e.streamOutput(ctx, guest); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.append("stdout", []byte("still running")); err != nil {
		t.Fatal(err)
	}
}
