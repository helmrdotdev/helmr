package workergroup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
)

type advancingFenceClock struct{ now time.Time }

func (c *advancingFenceClock) Now() time.Time { return c.now }
func (c *advancingFenceClock) Wait(ctx context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	return ctx.Err()
}

func TestServingProbeRequiresOriginReadiness(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		wantError  bool
	}{
		{"ready", `{"status":"ready"}`, 200, false},
		{"not ready", `{"status":"not_ready"}`, 200, true},
		{"unavailable", `{"status":"ready"}`, 503, true},
		{"maintenance page", `<html>ready</html>`, 200, true},
		{"extra response", `{"status":"ready"}{}`, 200, true},
		{"oversized", `{"status":"ready"}` + strings.Repeat(" ", 1024), 200, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/readyz" || r.Header.Get("Cache-Control") != "no-cache" {
					t.Errorf("readiness request: %s %v", r.URL.Path, r.Header)
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			probe, err := controlPlaneServingProbe(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			if err := probe(t.Context()); (err != nil) != test.wantError {
				t.Fatalf("probe error = %v", err)
			}
		})
	}
	t.Run("redirect is not serving evidence", func(t *testing.T) {
		var followed atomic.Bool
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/readyz" {
				http.Redirect(w, r, "/maintenance", http.StatusFound)
				return
			}
			followed.Store(true)
			_, _ = io.WriteString(w, `{"status":"ready"}`)
		}))
		defer server.Close()
		probe, err := controlPlaneServingProbe(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		if err := probe(t.Context()); err == nil || followed.Load() {
			t.Fatalf("redirect accepted: %v", err)
		}
	})
	t.Run("canceled observation", func(t *testing.T) {
		started := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
		defer server.Close()
		probe, err := controlPlaneServingProbe(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- probe(ctx) }()
		<-started
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("probe = %v", err)
		}
	})
	for _, endpoint := range []string{"", "ftp://example.com", "/relative", "https://user:secret@example.com", "https://example.com?x=1", "https://example.com#fragment", "https://example.com/base/", "http://example.com"} {
		if _, err := controlPlaneServingProbe(endpoint); err == nil {
			t.Fatalf("accepted invalid endpoint %q", endpoint)
		}
	}
}

func TestServingWindowResetsAfterOutageGapAndRestart(t *testing.T) {
	for _, interruption := range []string{"outage", "missed sample", "restart"} {
		t.Run(interruption, func(t *testing.T) {
			clock := &advancingFenceClock{now: time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)}
			store := &fakeStaleHostFenceQueries{}
			var unavailable bool
			probe := func(context.Context) error {
				if unavailable {
					return errors.New("API unavailable")
				}
				return nil
			}
			newFencer := func() *StaleHostFencer {
				f, err := newStaleHostFencer(fakeStaleHostFenceTransactions{queries: store}, nil, probe, WithStaleHostFenceClock(clock), WithStaleHostFenceLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
				if err != nil {
					t.Fatal(err)
				}
				return f
			}
			f := newFencer()
			// A second replica never inherits the first replica's serving window.
			assertSuspended := func(f *StaleHostFencer, want bool) {
				t.Helper()
				cycle, err := f.ReconcileOnce(t.Context())
				if err != nil || cycle.Suspended != want {
					t.Fatalf("cycle=%+v error=%v, want suspended=%v", cycle, err, want)
				}
			}
			for range 24 {
				assertSuspended(f, true)
				clock.now = clock.now.Add(5 * time.Second)
			}
			assertSuspended(f, false)
			assertSuspended(newFencer(), true)
			switch interruption {
			case "outage":
				unavailable = true
				for range 26 {
					clock.now = clock.now.Add(5 * time.Second)
					assertSuspended(f, true)
				}
				unavailable = false
			case "missed sample":
				clock.now = clock.now.Add(11 * time.Second)
			case "restart":
				f = newFencer()
			}
			queriesBefore := len(store.listParams)
			for range 24 {
				assertSuspended(f, true)
				clock.now = clock.now.Add(5 * time.Second)
			}
			if len(store.listParams) != queriesBefore {
				t.Fatal("suspended cycle queried fence candidates")
			}
			assertSuspended(f, false)
		})
	}
}

func TestServingEvidenceExpiresBeforeFenceCommit(t *testing.T) {
	clock := &advancingFenceClock{now: time.Now()}
	store := &fakeStaleHostFenceQueries{candidates: []db.ListStaleWorkerFenceCandidatesRow{staleHostCandidate(1, db.WorkerHostStatusActive, clock.now.Add(-10*time.Minute), staleHostReasonCode)}}
	store.recheck = func(context.Context, db.RecheckAndFenceStaleWorkerHostParams) (db.RecheckAndFenceStaleWorkerHostRow, error) {
		clock.now = clock.now.Add(11 * time.Second)
		return db.RecheckAndFenceStaleWorkerHostRow{}, nil
	}
	f := newTestStaleHostFencer(t, store, clock.now)
	f.clock = clock
	cycle, err := f.ReconcileOnce(t.Context())
	if err != nil || !cycle.Suspended || cycle.Fenced != 0 || len(cycle.Results) != 0 {
		t.Fatalf("expired evidence reported fence: %+v %v", cycle, err)
	}
	if !f.servingSince.IsZero() {
		t.Fatal("expired cycle retained a recovery window")
	}
}

func TestServingOutageDoesNotExitFencer(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clock := &cancelingStaleHostFenceClock{now: time.Now(), cancel: cancel, cancelAt: 4, waitCalls: &atomic.Int32{}}
	transactions := &failingStaleHostFenceTransactions{err: errors.New("must not query")}
	f, err := newStaleHostFencer(transactions, nil, func(context.Context) error { return errors.New("connection refused") }, WithStaleHostFenceClock(clock), WithStaleHostFenceLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if transactions.calls.Load() != 0 || clock.waitCalls.Load() != 4 {
		t.Fatal("serving outage did not keep retrying without fencing")
	}
}

// A lost COMMIT response cannot establish whether the changes persisted.
func TestServingEvidenceExpiryPreservesUncertainCommitError(t *testing.T) {
	clock := &advancingFenceClock{now: time.Now()}
	store := &fakeStaleHostFenceQueries{candidates: []db.ListStaleWorkerFenceCandidatesRow{staleHostCandidate(1, db.WorkerHostStatusActive, clock.now.Add(-10*time.Minute), staleHostReasonCode)}}
	f := newTestStaleHostFencer(t, store, clock.now)
	f.clock = clock
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	commitErr := errors.New("commit response lost")
	persisted := false
	f.transactions = fenceTransactionFunc(func(ctx context.Context, fn func(staleHostFenceQueries) error) error {
		if err := fn(store); err != nil {
			return err
		}
		persisted = len(store.rechecks) == 1
		clock.now = clock.now.Add(11 * time.Second)
		cancel()
		return commitErr
	})
	cycle, err := f.ReconcileOnce(ctx)
	if !persisted || !errors.Is(err, commitErr) || !cycle.Suspended || cycle.Fenced != 0 || len(cycle.Results) != 0 {
		t.Fatalf("uncertain commit hidden: persisted=%v cycle=%+v err=%v", persisted, cycle, err)
	}
	if !f.servingSince.IsZero() || !f.lastServingAt.IsZero() {
		t.Fatal("expired commit retained serving evidence")
	}
}

type fenceTransactionFunc func(context.Context, func(staleHostFenceQueries) error) error

func (fn fenceTransactionFunc) withinStaleHostFenceTransaction(ctx context.Context, queries func(staleHostFenceQueries) error) error {
	return fn(ctx, queries)
}

func TestServingOutageLogsRemainBoundedAcrossChangingErrors(t *testing.T) {
	clock := &advancingFenceClock{now: time.Now()}
	var logs strings.Builder
	f := newTestStaleHostFencer(t, &fakeStaleHostFenceQueries{}, clock.now)
	f.clock = clock
	f.log = slog.New(slog.NewTextHandler(&logs, nil))
	for i := range 24 {
		f.probeServing = func(context.Context) error { return errors.New(clock.now.String()) }
		cycle, err := f.ReconcileOnce(t.Context())
		if err != nil || !cycle.Suspended {
			t.Fatalf("cycle %d: %+v %v", i, cycle, err)
		}
		clock.now = clock.now.Add(5 * time.Second)
	}
	if got := strings.Count(logs.String(), "stale worker fencing suspended"); got != 2 {
		t.Fatalf("suspension warnings = %d, want 2", got)
	}
}

func TestServingSampleRejectsSuspendAndClockStepGaps(t *testing.T) {
	for _, tc := range []struct {
		name            string
		monotonic, wall time.Duration
		fresh           bool
	}{
		{"ordinary sample", 5 * time.Second, 5 * time.Second, true},
		{"host suspended", 5 * time.Second, 125 * time.Second, false},
		{"wall step hides missed samples", 125 * time.Second, 5 * time.Second, false},
		{"wall moved backwards", 5 * time.Second, -time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := servingSampleFresh(tc.monotonic, tc.wall, 10*time.Second); got != tc.fresh {
				t.Fatalf("sample freshness=%v, want %v", got, tc.fresh)
			}
		})
	}
}
