package guestd

import (
	"context"
	"errors"
	"io"
	"math"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
)

// The retained execution owns two bounded pipes, including their unacknowledged
// heads. Transport replacement never recreates output or chooses a replay cursor.
type commandOutputSpool struct {
	mu         sync.Mutex
	pipes      map[string]*diagnosticBuffer
	gapped     map[string]bool
	boundaries map[string]*computerv0.CommandOutputBoundary
	changed    chan struct{}
	generation uint64
	detach     func()
	closed     bool
}

func newCommandOutputSpool(limits diagnosticLimits) (*commandOutputSpool, error) {
	s := &commandOutputSpool{pipes: make(map[string]*diagnosticBuffer), gapped: make(map[string]bool), boundaries: make(map[string]*computerv0.CommandOutputBoundary), changed: make(chan struct{})}
	for _, stream := range []string{"stdout", "stderr"} {
		pipe, err := newDiagnosticBuffer(limits)
		if err != nil {
			return nil, err
		}
		s.pipes[stream] = pipe
	}
	return s, nil
}

func (s *commandOutputSpool) notifyLocked() { close(s.changed); s.changed = make(chan struct{}) }
func (s *commandOutputSpool) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.detach != nil {
		s.detach()
	}
	for _, pipe := range s.pipes {
		pipe.mu.Lock()
		for i := range pipe.records {
			clear(pipe.records[i].Data)
		}
		pipe.records = nil
		pipe.count = 0
		pipe.pendingGap = nil
		pipe.end = nil
		pipe.mu.Unlock()
	}
	s.notifyLocked()
}
func (s *commandOutputSpool) append(stream string, content []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("command output was released")
	}
	pipe := s.pipes[stream]
	if pipe == nil {
		return errors.New("invalid command output pipe")
	}
	accepted, err := pipe.append(content, time.Now())
	if !accepted {
		s.gapped[stream] = true
	}
	s.notifyLocked()
	return err
}

// finish records immutable EOF frontiers before process completion is exposed.
func (s *commandOutputSpool) finish(complete bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for stream, pipe := range s.pipes {
		if s.boundaries[stream] != nil {
			continue
		}
		pipe.close(complete, time.Now())
		pipe.mu.Lock()
		s.boundaries[stream] = &computerv0.CommandOutputBoundary{ThroughSequence: uint64(pipe.end.Through), Complete: pipe.end.Complete, Gapped: s.gapped[stream]}
		pipe.mu.Unlock()
	}
	s.notifyLocked()
}
func (s *commandOutputSpool) settledLocked() bool {
	for stream, pipe := range s.pipes {
		boundary := s.boundaries[stream]
		if boundary == nil {
			return false
		}
		pipe.mu.Lock()
		settled := uint64(pipe.acknowledged) == boundary.ThroughSequence
		pipe.mu.Unlock()
		if !settled {
			return false
		}
	}
	return true
}
func (s *commandOutputSpool) settled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settledLocked()
}

func commandOutputRecord(stream string, record diagnosticRecord) *computerv0.CommandOutputChunk {
	kind := map[diagnosticKind]string{diagnosticData: "data", diagnosticGap: "gap", diagnosticEnd: "end"}[record.Kind]
	return &computerv0.CommandOutputChunk{Stream: stream, Sequence: uint64(record.Sequence), ThroughSequence: uint64(record.Through), ObservedAtUnixNano: record.ObservedAt.UnixNano(), Content: record.Data, Kind: kind, DroppedBytes: record.DroppedBytes, Complete: record.Complete}
}

type commandOutputWriter struct {
	spool   *commandOutputSpool
	stream  string
	onError func(error)
}

func (w *commandOutputWriter) Write(content []byte) (int, error) {
	written := 0
	for len(content) > 0 {
		n := min(len(content), w.spool.pipes[w.stream].limits.ChunkBytes)
		if err := w.spool.append(w.stream, content[:n]); err != nil {
			if w.onError != nil {
				w.onError(err)
			}
			return written, err
		}
		written += n
		content = content[n:]
	}
	return written, nil
}
func commandResultEvent(result *computerv0.ComputerBasicExecResult) *computerv0.ComputerBasicExecEvent {
	return &computerv0.ComputerBasicExecEvent{Event: &computerv0.ComputerBasicExecEvent_Result{Result: result}}
}

// One immutable head per pipe can be outstanding. The terminal outcome is sent
// independently of ACK progress so telemetry pressure cannot hide process exit.
func (e *computerBasicExec) streamOutput(ctx context.Context, conn io.ReadWriter) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	closer, ok := conn.(io.Closer)
	if !ok {
		return errors.New("command output transport must be closeable")
	}
	s := e.output
	s.mu.Lock()
	if s.closed || s.generation == math.MaxUint64 {
		s.mu.Unlock()
		return errors.New("command output unavailable")
	}
	if s.detach != nil {
		s.detach()
	}
	ctx, cancel := context.WithCancel(ctx)
	detach := func() { cancel(); _ = closer.Close() }
	s.generation++
	generation := s.generation
	s.detach = detach
	s.mu.Unlock()
	defer detach()
	stop := context.AfterFunc(ctx, func() { _ = closer.Close() })
	defer stop()
	errorsCh := make(chan error, 1)
	sent := map[string]uint64{}
	// ACK validation and releasing heads share the attachment fence lock.
	go func() {
		for {
			ack := new(computerv0.CommandOutputAck)
			if err := frameio.ReadProtoFrameBounded(conn, 1024, ack); err != nil {
				errorsCh <- err
				return
			}
			s.mu.Lock()
			pipe := s.pipes[ack.Stream]
			valid := generation == s.generation && !s.closed && pipe != nil && ack.ThroughSequence > 0 && ack.ThroughSequence <= math.MaxInt64 && ack.ThroughSequence == sent[ack.Stream] && (ack.Disposition == "accepted" || ack.Disposition == "expired")
			var err error
			if !valid {
				err = errors.New("command output ACK is fenced or invalid")
			} else {
				err = pipe.acknowledge(int64(ack.ThroughSequence))
				if err == nil {
					delete(sent, ack.Stream)
					s.notifyLocked()
				}
			}
			s.mu.Unlock()
			if err != nil {
				errorsCh <- err
				return
			}
		}
	}()
	resultSent := false
	done := e.done
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !resultSent {
			select {
			case <-done:
				if e.result == nil {
					return errors.New("command finished without a result")
				}
				if err := frameio.WriteProtoFrame(conn, commandResultEvent(e.result)); err != nil {
					return err
				}
				resultSent = true
				done = nil
			default:
			}
		}
		s.mu.Lock()
		if generation != s.generation || s.closed {
			s.mu.Unlock()
			return errors.New("command output attachment replaced")
		}
		settled := s.settledLocked()
		var chunk *computerv0.CommandOutputChunk
		for _, stream := range []string{"stdout", "stderr"} {
			if sent[stream] != 0 {
				continue
			}
			if record, ok := s.pipes[stream].peek(); ok {
				chunk = commandOutputRecord(stream, record)
				sent[stream] = chunk.ThroughSequence
				break
			}
		}
		changed := s.changed
		s.mu.Unlock()
		if resultSent && settled {
			return nil
		}
		if chunk != nil {
			if err := frameio.WriteProtoFrame(conn, &computerv0.ComputerBasicExecEvent{Event: &computerv0.ComputerBasicExecEvent_Output{Output: chunk}}); err != nil {
				return err
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errorsCh:
			return err
		case <-changed:
		case <-done:
		}
	}
}
