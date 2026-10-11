package guestd

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func TestComputerWritebackLatchesFirstFailure(t *testing.T) {
	var calls int
	w := &computerWriteback{syncFilesystem: func() error {
		calls++
		if calls == 1 {
			return syscall.EIO
		}
		return nil
	}}
	for range 2 {
		if err := w.flush(t.Context()); !errors.Is(err, errComputerWriteback) {
			t.Fatalf("writeback error: %v", err)
		}
	}
	if calls != 1 {
		t.Fatal("later flush consumed an earlier writeback failure")
	}
}

func TestComputerWritebackCancellationJoinsKernelOperation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	w := &computerWriteback{syncFilesystem: func() error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() { first <- w.flush(ctx) }()
	<-entered
	cancel()
	if w.mu.TryLock() {
		w.mu.Unlock()
		t.Fatal("cancel released in-flight writeback owner")
	}
	select {
	case <-first:
		t.Fatal("cancel detached in-flight syncfs")
	default:
	}
	second := make(chan error, 1)
	go func() { second <- w.flush(t.Context()) }()
	if calls.Load() != 1 {
		t.Fatal("overlapping syncfs owners")
	}
	close(release)
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("first: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("second owner did not flush")
	}
}

func TestComputerWritebackProtocolReportsLatchedFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		w := &computerWriteback{syncFilesystem: func() error {
			if fail {
				return syscall.EIO
			}
			return nil
		}}
		for range 2 {
			client, server := net.Pipe()
			done := make(chan error, 1)
			id := uuid.NewV7().String()
			go func() {
				defer server.Close()
				done <- handleComputerFlush(t.Context(), server, wire.StreamHeader{OperationID: id}, 0, w)
			}()
			if err := frameio.WriteProtoFrame(client, &computerv0.FlushComputerRequest{OperationId: id}); err != nil {
				t.Fatal(err)
			}
			var response computerv0.FlushComputerResponse
			if err := frameio.ReadProtoFrame(client, &response); err != nil {
				t.Fatal(err)
			}
			client.Close()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if response.OperationId != id || (response.Error != "") != fail {
				t.Fatalf("response: %v", &response)
			}
		}
	}
}

func TestComputerWritebackRejectsMismatchedOperation(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	var calls atomic.Int32
	w := &computerWriteback{syncFilesystem: func() error { calls.Add(1); return nil }}
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		done <- handleComputerFlush(t.Context(), server, wire.StreamHeader{OperationID: uuid.NewV7().String()}, 0, w)
	}()
	if err := frameio.WriteProtoFrame(client, &computerv0.FlushComputerRequest{OperationId: uuid.NewV7().String()}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("mismatched operation accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("invalid operation reached writeback")
	}
}
