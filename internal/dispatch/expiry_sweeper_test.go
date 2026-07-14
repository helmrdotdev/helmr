package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestExpirySweepersIsolatePersistentBuildFailures(t *testing.T) {
	store := &expiryIsolationStore{buildErr: errors.New("build database unavailable")}
	runSweeper, buildSweeper := newIsolationSweepers(t, store)

	cancel, runDone, buildDone := startIsolationSweepers(t, runSweeper, buildSweeper)
	waitForSweepCount(t, "build", &store.buildCalls, 5)
	waitForSweepCount(t, "run", &store.runCalls, 8)
	assertSweepersRunning(t, runDone, buildDone)
	cancel()
	assertSweeperCanceled(t, "run", runDone)
	assertSweeperCanceled(t, "build", buildDone)
}

func TestExpirySweepersIsolatePersistentRunFailures(t *testing.T) {
	store := &expiryIsolationStore{runErr: errors.New("run database unavailable")}
	runSweeper, buildSweeper := newIsolationSweepers(t, store)

	cancel, runDone, buildDone := startIsolationSweepers(t, runSweeper, buildSweeper)
	waitForSweepCount(t, "run", &store.runCalls, 5)
	waitForSweepCount(t, "build", &store.buildCalls, 8)
	assertSweepersRunning(t, runDone, buildDone)
	cancel()
	assertSweeperCanceled(t, "run", runDone)
	assertSweeperCanceled(t, "build", buildDone)
}

func newIsolationSweepers(t *testing.T, store *expiryIsolationStore) (*ExpirySweeper, *BuildExpirySweeper) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runSweeper, err := NewExpirySweeper(
		store,
		WithExpirySweepInterval(2*time.Millisecond),
		WithExpirySweepTimeout(50*time.Millisecond),
		WithExpirySweepConsecutiveFailureLimit(3),
		WithExpirySweepLogger(log),
	)
	if err != nil {
		t.Fatal(err)
	}
	buildSweeper, err := NewBuildExpirySweeper(
		store,
		WithBuildExpirySweepInterval(2*time.Millisecond),
		WithBuildExpirySweepTimeout(50*time.Millisecond),
		WithBuildExpirySweepConsecutiveFailureLimit(3),
		WithBuildExpirySweepLogger(log),
	)
	if err != nil {
		t.Fatal(err)
	}
	return runSweeper, buildSweeper
}

func startIsolationSweepers(
	t *testing.T,
	runSweeper *ExpirySweeper,
	buildSweeper *BuildExpirySweeper,
) (context.CancelFunc, <-chan error, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	buildDone := make(chan error, 1)
	go func() { runDone <- runSweeper.Run(ctx) }()
	go func() { buildDone <- buildSweeper.Run(ctx) }()
	t.Cleanup(cancel)
	return cancel, runDone, buildDone
}

func waitForSweepCount(t *testing.T, domain string, calls *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s sweep calls = %d, want at least %d", domain, calls.Load(), want)
}

func assertSweepersRunning(t *testing.T, runDone, buildDone <-chan error) {
	t.Helper()
	select {
	case err := <-runDone:
		t.Fatalf("run sweeper stopped before cancellation: %v", err)
	default:
	}
	select {
	case err := <-buildDone:
		t.Fatalf("build sweeper stopped before cancellation: %v", err)
	default:
	}
}

func assertSweeperCanceled(t *testing.T, domain string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%s sweeper error = %v, want context cancellation", domain, err)
		}
	case <-time.After(time.Second):
		t.Fatalf("%s sweeper did not stop after cancellation", domain)
	}
}

type expiryIsolationStore struct {
	runCalls   atomic.Int64
	buildCalls atomic.Int64
	runErr     error
	buildErr   error
}

func (s *expiryIsolationStore) RequeueExpiredLeasedRunLeases(context.Context) error {
	s.runCalls.Add(1)
	return s.runErr
}

func (*expiryIsolationStore) RequeueExpiredRunningRunLeases(context.Context) error {
	return nil
}

func (s *expiryIsolationStore) RequeueExpiredDeploymentBuildLeases(context.Context) error {
	s.buildCalls.Add(1)
	return s.buildErr
}

func (*expiryIsolationStore) ListOrganizationIDsPage(context.Context, db.ListOrganizationIDsPageParams) ([]pgtype.UUID, error) {
	return nil, nil
}

func (*expiryIsolationStore) MarkExpiredRuntimeInstancesLost(context.Context, int32) ([]db.RuntimeInstance, error) {
	return nil, nil
}

func (*expiryIsolationStore) MarkStaleWorkspaceMountsLost(context.Context, int32) ([]db.WorkspaceMount, error) {
	return nil, nil
}

func (*expiryIsolationStore) ExpireWorkspaceLeases(context.Context, int32) ([]db.ExpireWorkspaceLeasesRow, error) {
	return nil, nil
}

func (*expiryIsolationStore) ExpireQueuedRuns(context.Context, pgtype.UUID) error {
	return nil
}

func (*expiryIsolationStore) ExpireDueSessions(context.Context, pgtype.UUID) ([]db.Session, error) {
	return nil, nil
}

func (*expiryIsolationStore) ExpireDueTokens(context.Context, pgtype.UUID) ([]db.ExpireDueTokensRow, error) {
	return nil, nil
}

func (*expiryIsolationStore) ResolveDueTimerWaits(context.Context, db.ResolveDueTimerWaitsParams) ([]db.ResolveDueTimerWaitsRow, error) {
	return nil, nil
}

func (*expiryIsolationStore) ExpireDueRunWaits(context.Context, int32) ([]db.RunWait, error) {
	return nil, nil
}

func (*expiryIsolationStore) RequeueStaleResumingRunWaits(context.Context, db.RequeueStaleResumingRunWaitsParams) ([]db.RequeueStaleResumingRunWaitsRow, error) {
	return nil, nil
}

func (*expiryIsolationStore) RequeueResolvedRunWaits(context.Context, db.RequeueResolvedRunWaitsParams) ([]db.RequeueResolvedRunWaitsRow, error) {
	return nil, nil
}
