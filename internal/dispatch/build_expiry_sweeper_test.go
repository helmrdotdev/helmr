package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func TestBuildExpirySweeperRetriesPersistentFailures(t *testing.T) {
	store := &buildExpiryStore{err: errors.New("database unavailable")}
	sweeper, err := NewBuildExpirySweeper(
		store,
		WithBuildExpirySweepInterval(2*time.Millisecond),
		WithBuildExpirySweepTimeout(50*time.Millisecond),
		WithBuildExpirySweepConsecutiveFailureLimit(3),
		WithBuildExpirySweepLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- sweeper.Run(ctx)
	}()
	deadline := time.After(time.Second)
	for store.calls() < 3 {
		select {
		case <-deadline:
			t.Fatal("build expiry sweeper did not retry")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
}

type buildExpiryStore struct {
	mu  sync.Mutex
	n   int
	err error
}

func (s *buildExpiryStore) RequeueExpiredDeploymentBuildLeases(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return s.err
}

func (s *buildExpiryStore) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}
