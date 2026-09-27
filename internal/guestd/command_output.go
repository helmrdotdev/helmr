package guestd

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
)

const commandOutputChunkBytes = 64 << 10

// The execution owns this private, unlinked spool until its mount is retired.
// Readers retain only a file offset; reconnects replay the same bytes, sequence
// and observation time. The spool is outside the Computer's captured filesystem.
type commandOutputSpool struct {
	mu        sync.Mutex
	file      *os.File
	size      int64
	sequences map[string]uint64
	changed   chan struct{}
	err       error
}

func newCommandOutputSpool() (*commandOutputSpool, error) {
	root, err := guestdTempRoot()
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(root, "helmr-command-output-*")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		return nil, err
	}
	return &commandOutputSpool{file: file, sequences: make(map[string]uint64), changed: make(chan struct{})}, nil
}

func (s *commandOutputSpool) close() { _ = s.file.Close() }

func (s *commandOutputSpool) append(stream string, content []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	chunk := &computerv0.CommandOutputChunk{Stream: stream, Sequence: s.sequences[stream], ObservedAtUnixNano: time.Now().UnixNano(), Content: content}
	if err := frameio.WriteProtoFrame(s.file, chunk); err != nil {
		s.err = err
		return err
	}
	size, err := s.file.Seek(0, io.SeekCurrent)
	if err != nil {
		s.err = err
		return err
	}
	s.size = size
	s.sequences[stream]++
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}

func (s *commandOutputSpool) read(offset int64) (*computerv0.CommandOutputChunk, int64, <-chan struct{}, error) {
	s.mu.Lock()
	size, changed := s.size, s.changed
	s.mu.Unlock()
	if offset == size {
		return nil, offset, changed, nil
	}
	reader := io.NewSectionReader(s.file, offset, size-offset)
	chunk := new(computerv0.CommandOutputChunk)
	if err := frameio.ReadProtoFrameBounded(reader, commandOutputChunkBytes+1024, chunk); err != nil {
		return nil, offset, changed, err
	}
	consumed, err := reader.Seek(0, io.SeekCurrent)
	return chunk, offset + consumed, changed, err
}

type commandOutputWriter struct {
	spool   *commandOutputSpool
	stream  string
	onError func(error)
}

func (w *commandOutputWriter) Write(content []byte) (int, error) {
	written := 0
	for len(content) > 0 {
		n := min(len(content), commandOutputChunkBytes)
		if err := w.spool.append(w.stream, content[:n]); err != nil {
			w.onError(err)
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

func (e *computerBasicExec) streamOutput(ctx context.Context, writer io.Writer) error {
	var offset int64
	finished := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, next, changed, err := e.output.read(offset)
		if err != nil {
			return err
		}
		if chunk != nil {
			if err := frameio.WriteProtoFrame(writer, &computerv0.ComputerBasicExecEvent{Event: &computerv0.ComputerBasicExecEvent_Output{Output: chunk}}); err != nil {
				return err
			}
			offset = next
			continue
		}
		if finished {
			if e.result == nil {
				return errors.New("command finished without a result")
			}
			return frameio.WriteProtoFrame(writer, commandResultEvent(e.result))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		case <-e.done:
			finished = true
		}
	}
}
