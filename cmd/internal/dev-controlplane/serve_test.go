package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestServeDevClosesDependenciesAfterHTTPDrainAndLoops(t *testing.T) {
	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, event)
	}
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	loopCanceled := make(chan struct{})
	releaseLoop := make(chan struct{})
	addr := freeAddr(t)
	server := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(handlerStarted)
			<-releaseHandler
			record("handler finished")
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	loops := []backgroundLoop{{name: "blocked", run: func(ctx context.Context) error {
		<-ctx.Done()
		close(loopCanceled)
		<-releaseLoop
		record("loop finished")
		return ctx.Err()
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() {
		// runDev closes Redis, ClickHouse and PostgreSQL in defers once
		// serveDev returns; record that point.
		err := serveDev(ctx, discardLog(), server, "", loops, 5*time.Second)
		record("dependencies closed")
		serveErr <- err
	}()
	requestErr := make(chan error, 1)
	go func() { requestErr <- get(addr) }()
	waitFor(t, handlerStarted, "request did not reach the server")

	cancel()
	select {
	case <-loopCanceled:
		t.Fatal("background loop was canceled before the HTTP drain finished")
	case <-serveErr:
		t.Fatal("serveDev returned before the HTTP drain finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseHandler)
	if err := <-requestErr; err != nil {
		t.Fatal(err)
	}
	waitFor(t, loopCanceled, "background loop was not canceled after the HTTP drain")
	select {
	case <-serveErr:
		t.Fatal("serveDev returned before the background loop finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseLoop)
	if err := <-serveErr; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"handler finished", "loop finished", "dependencies closed"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestServeDevForceClosesAfterShutdownTimeoutAndJoinsLoops(t *testing.T) {
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	defer close(releaseHandler)
	loopStopped := make(chan struct{})
	addr := freeAddr(t)
	server := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			close(handlerStarted)
			<-releaseHandler
		}),
	}
	loops := []backgroundLoop{{name: "waiting", run: func(ctx context.Context) error {
		<-ctx.Done()
		close(loopStopped)
		return ctx.Err()
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveDev(ctx, discardLog(), server, "", loops, 20*time.Millisecond)
	}()
	requestErr := make(chan error, 1)
	go func() { requestErr <- get(addr) }()
	waitFor(t, handlerStarted, "request did not reach the server")

	cancel()
	var err error
	select {
	case err = <-serveErr:
	case <-time.After(5 * time.Second):
		t.Fatal("serveDev did not return after the shutdown timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want shutdown deadline", err)
	}
	select {
	case <-loopStopped:
	default:
		t.Fatal("serveDev returned before the background loop stopped")
	}
	select {
	case err := <-requestErr:
		if err == nil {
			t.Fatal("forced close completed the blocked request")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("forced close did not release the client")
	}
}

func TestServeDevReturnsListenFailureWithoutStartingLoops(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	started := make(chan struct{}, 1)
	server := &http.Server{Addr: occupied.Addr().String(), Handler: http.NotFoundHandler()}
	err = serveDev(t.Context(), discardLog(), server, "", []backgroundLoop{{name: "never", run: func(context.Context) error {
		started <- struct{}{}
		return nil
	}}}, time.Second)
	if err == nil {
		t.Fatal("occupied address was accepted")
	}
	select {
	case <-started:
		t.Fatal("background loop started after listen failure")
	default:
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func get(addr string) error {
	for deadline := time.Now().Add(5 * time.Second); ; {
		response, err := http.Get("http://" + addr + "/")
		if err == nil {
			return response.Body.Close()
		}
		var opErr *net.OpError
		if !errors.As(err, &opErr) || opErr.Op != "dial" || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitFor(t *testing.T, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal(message)
	}
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
