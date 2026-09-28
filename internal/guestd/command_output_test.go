package guestd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

func commandSpoolBytes(t *testing.T, spool *commandOutputSpool, stream string) []byte {
	t.Helper()
	var result []byte
	var offset int64
	for {
		chunk, next, _, err := spool.read(offset)
		if err != nil {
			t.Fatal(err)
		}
		if chunk == nil {
			return result
		}
		if chunk.GetStream() == stream {
			result = append(result, chunk.GetContent()...)
		}
		offset = next
	}
}

func TestCommandOutputStreamsBeforeExitAndReplaysAfterDisconnect(t *testing.T) {
	t.Setenv("HELMR_GUESTD_TMPDIR", t.TempDir())
	spool, err := newCommandOutputSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	execution := &computerBasicExec{output: spool, done: make(chan struct{})}
	reader, writer := io.Pipe()
	firstDone := make(chan error, 1)
	go func() { firstDone <- execution.streamOutput(t.Context(), writer); writer.Close() }()
	payload := []byte{0, 255, 10, 128}
	if err := spool.append("stdout", payload); err != nil {
		t.Fatal(err)
	}
	var first computerv0.ComputerBasicExecEvent
	if err := frameio.ReadProtoFrame(reader, &first); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.GetOutput().GetContent(), payload) {
		t.Fatal(&first)
	}
	// Disconnect after receiving a chunk, before process exit or acknowledgement.
	reader.Close()
	if err := spool.append("stderr", []byte("last")); err != nil {
		t.Fatal(err)
	}
	execution.result = &computerv0.ComputerBasicExecResult{Outcome: "exited", ExitCode: 7}
	close(execution.done)
	if err := <-firstDone; err == nil {
		t.Fatal("disconnected stream succeeded")
	}
	var replay bytes.Buffer
	if err := execution.streamOutput(t.Context(), &replay); err != nil {
		t.Fatal(err)
	}
	var got computerv0.ComputerBasicExecEvent
	if err := frameio.ReadProtoFrame(&replay, &got); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(&first, &got) {
		t.Fatalf("replay changed: %v / %v", &first, &got)
	}
	if err := frameio.ReadProtoFrame(&replay, &got); err != nil {
		t.Fatal(err)
	}
	if got.GetOutput().GetStream() != "stderr" || got.GetOutput().GetSequence() != 0 {
		t.Fatal(&got)
	}
	if err := frameio.ReadProtoFrame(&replay, &got); err != nil {
		t.Fatal(err)
	}
	if got.GetResult().GetExitCode() != 7 || replay.Len() != 0 {
		t.Fatal(&got)
	}
}

func TestCommandOutputConcurrentBinaryWriters(t *testing.T) {
	t.Setenv("HELMR_GUESTD_TMPDIR", t.TempDir())
	spool, err := newCommandOutputSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	payload := bytes.Repeat([]byte{0, 255, 128, 10}, (5<<20)/4)
	var writers sync.WaitGroup
	for _, stream := range []string{"stdout", "stderr"} {
		writers.Go(func() {
			writer := &commandOutputWriter{spool: spool, stream: stream, onError: func(err error) { t.Error(err) }}
			if n, err := writer.Write(payload); err != nil || n != len(payload) {
				t.Errorf("write %d: %v", n, err)
			}
		})
	}
	writers.Wait()
	for _, stream := range []string{"stdout", "stderr"} {
		if !bytes.Equal(commandSpoolBytes(t, spool, stream), payload) {
			t.Fatalf("lost %s bytes", stream)
		}
	}
	next := map[string]uint64{"stdout": 0, "stderr": 0}
	var offset int64
	for {
		chunk, end, _, err := spool.read(offset)
		if err != nil {
			t.Fatal(err)
		}
		if chunk == nil {
			break
		}
		if chunk.Sequence != next[chunk.Stream] || len(chunk.Content) > commandOutputChunkBytes {
			t.Fatal(chunk)
		}
		next[chunk.Stream]++
		offset = end
	}
}

func TestCommandOutputCaptureFailurePreservesPrefix(t *testing.T) {
	t.Setenv("HELMR_GUESTD_TMPDIR", t.TempDir())
	spool, err := newCommandOutputSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	if err := spool.append("stdout", []byte("prefix")); err != nil {
		t.Fatal(err)
	}
	// Force the actual file write to fail without changing the published frontier.
	if err := spool.file.Close(); err != nil {
		t.Fatal(err)
	}
	failed := false
	writer := &commandOutputWriter{spool: spool, stream: "stdout", onError: func(error) { failed = true }}
	if n, err := writer.Write([]byte("tail")); n != 0 || err == nil || !failed {
		t.Fatalf("n=%d err=%v failure=%v", n, err, failed)
	}
	if spool.sequences["stdout"] != 1 {
		t.Fatal("failed append advanced sequence")
	}
}

func TestCommandOutputObservationCancellation(t *testing.T) {
	t.Setenv("HELMR_GUESTD_TMPDIR", t.TempDir())
	spool, err := newCommandOutputSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	execution := &computerBasicExec{output: spool, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := execution.streamOutput(ctx, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := spool.append("stdout", []byte("still running")); err != nil {
		t.Fatal(err)
	}
}

func TestCommandOutputUsesPrivateScratchFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HELMR_GUESTD_TMPDIR", root)
	spool, err := newCommandOutputSpool()
	if err != nil {
		t.Fatal(err)
	}
	defer spool.close()
	if filepath.Dir(spool.file.Name()) != root {
		t.Fatalf("spool outside scratch: %s", spool.file.Name())
	}
	info, err := spool.file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("permissions %v", info.Mode())
	}
	if _, err := os.Stat(spool.file.Name()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spool must be unlinked: %v", err)
	}
}
