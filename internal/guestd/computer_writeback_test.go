package guestd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
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

func TestComputerWritebackFinalizesOnce(t *testing.T) {
	var calls []string
	w := &computerWriteback{
		syncFilesystem:   func() error { calls = append(calls, "sync"); return nil },
		freezeFilesystem: func() error { calls = append(calls, "freeze"); return nil },
		closeFilesystem:  func() error { calls = append(calls, "close"); return nil },
	}
	if err := w.finalizePreparation(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := w.finalizePreparation(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := w.flush(t.Context()); err == nil {
		t.Fatal("ordinary flush accepted finalized source")
	}
	if err := w.close(); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(calls) != "[sync freeze close]" {
		t.Fatal(calls)
	}
}

func TestComputerWritebackFinalizationRetainsFault(t *testing.T) {
	for _, fail := range []string{"sync", "freeze"} {
		t.Run(fail, func(t *testing.T) {
			var syncs, freezes int
			w := &computerWriteback{
				syncFilesystem: func() error {
					syncs++
					if fail == "sync" {
						return errors.New("io")
					}
					return nil
				},
				freezeFilesystem: func() error { freezes++; return errors.New("io") },
			}
			for range 2 {
				if !errors.Is(w.finalizePreparation(t.Context()), errComputerWriteback) {
					t.Fatal("fault lost")
				}
			}
			want := 1
			if fail == "sync" {
				want = 0
			}
			if syncs != 1 || freezes != want {
				t.Fatalf("sync=%d freeze=%d", syncs, freezes)
			}
		})
	}
}

func TestComputerWritebackFinalizationJoinsCancelledFreeze(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
		w := &computerWriteback{
			syncFilesystem:   func() error { return nil },
			freezeFilesystem: func() error { close(entered); <-release; return nil },
			closeFilesystem:  func() error { close(closed); return nil },
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- w.finalizePreparation(ctx) }()
		<-entered
		cancel()
		synctest.Wait()
		// Mutex waits are not durably blocked for synctest. Check ownership
		// before starting close so Wait only observes the blocked freeze.
		if w.mu.TryLock() {
			w.mu.Unlock()
			close(release)
			<-done
			t.Fatal("freeze released writeback ownership")
		}
		go w.close()
		select {
		case <-closed:
			close(release)
			<-done
			t.Fatal("close overtook freeze")
		case <-done:
			close(release)
			<-closed
			t.Fatal("cancel released syscall")
		default:
		}
		close(release)
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		<-closed
		if !w.frozen {
			t.Fatal("completed freeze not retained after cancellation")
		}
	})
}
