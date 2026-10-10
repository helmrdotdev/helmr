package slack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func TestSlackRequestWorkersClaimAndCommitEachReceiptOnce(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	for i := range 8 {
		f.reply(t, fmt.Sprintf("reply-%d", i), false)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := ReconcileRequests(t.Context(), f.Pool, nil, testProjectionConfig(), f.admissionClient(t), 4); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var accepted, turns int
	if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM slack_requests WHERE status='accepted'),(SELECT count(*) FROM turns WHERE environment_id=$1)`, f.Environment).Scan(&accepted, &turns); err != nil || accepted != 8 || turns != 8 {
		t.Fatalf("worker outcomes: accepted=%d turns=%d %v", accepted, turns, err)
	}
	if count, err := ReconcileRequests(t.Context(), f.Pool, nil, testProjectionConfig(), f.admissionClient(t), 4); err != nil || count != 0 {
		t.Fatalf("terminal rows scanned again: %d %v", count, err)
	}
}

func TestSlackRequestScanReservationCanRecoverAfterCrash(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	id := f.reply(t, "recover", false)
	first, err := claimRequests(t.Context(), f.Pool, 4)
	if err != nil || len(first) != 1 || first[0] != id {
		t.Fatalf("claim: %v %v", first, err)
	}
	if next, err := claimRequests(t.Context(), f.Pool, 4); err != nil || len(next) != 0 {
		t.Fatalf("reserved row reclaimed early: %v %v", next, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1`, id)
	if count, err := ReconcileRequests(t.Context(), f.Pool, nil, testProjectionConfig(), f.admissionClient(t), 4); err != nil || count != 1 {
		t.Fatalf("claim not recovered: %d %v", count, err)
	}
	if err := ReconcileRequest(t.Context(), f.Pool, nil, testProjectionConfig(), f.admissionClient(t), id); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 1 {
		t.Fatalf("recovered request repeated: %d %v", count, err)
	}
}

func TestSlackRequestWorkerRoutesControlAndRejectsInvalidStoredPayload(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	stop := f.controlReceipt(t, f.control("stop"), nil)
	invalid := f.reply(t, "invalid", false)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET payload=convert_to('{"unexpected":true}','UTF8') WHERE id=$1`, invalid)
	if count, err := ReconcileRequests(t.Context(), f.Pool, nil, testProjectionConfig(), f.admissionClient(t), 4); err != nil || count != 2 {
		t.Fatalf("batch: %d %v", count, err)
	}
	var correct bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT status='accepted' AND operation='stop' FROM slack_requests WHERE id=$1) AND (SELECT status='rejected' AND error='gesture_invalid' FROM slack_requests WHERE id=$2)`, stop, invalid).Scan(&correct); err != nil || !correct {
		t.Fatalf("wrong dispatch: %v %v", correct, err)
	}
}

func TestSlackRequestRunnerConsumesReceiptAndJoinsOnCancellation(t *testing.T) {
	f := newStatusFixture(t)
	f.link(t)
	receipt := f.reply(t, "runner", false)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- RunRequests(ctx, f.Pool, nil, testProjectionConfig(), f.admissionClient(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	accepted := false
	for !accepted && ctx.Err() == nil {
		if err := f.Pool.QueryRow(ctx, `SELECT status='accepted' FROM slack_requests WHERE id=$1`, receipt).Scan(&accepted); err != nil {
			cancel()
			<-done
			t.Fatal(err)
		}
		if accepted {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("runner did not join cancellation: %v", err)
	}
	if !accepted {
		t.Fatal("runner never consumed durable receipt")
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 1 {
		t.Fatalf("runner admission: %d %v", count, err)
	}
}

func TestSlackRequestRunnerDeliversPrivateRejectionAfterReceiptCommit(t *testing.T) {
	f := newStatusFixture(t)
	id := f.reply(t, "unlinked runner", false)
	sent := make(chan struct{}, 1)
	client := deliveryTestClient(func(r *http.Request) string {
		if r.URL.Path != "/api/chat.postEphemeral" {
			t.Errorf("wrong rejection method: %s", r.URL.Path)
		}
		// A second connection can lock the receipt while HTTP is in flight: the
		// disposition and terminal admission must already be committed.
		err := db.RunTx(r.Context(), f.Pool, func(tx pgx.Tx) error {
			var ready bool
			if err := tx.QueryRow(r.Context(), `SELECT status='rejected' AND payload_expired_at IS NOT NULL AND payload IS NULL FROM slack_requests WHERE id=$1 FOR UPDATE NOWAIT`, id).Scan(&ready); err != nil {
				return err
			}
			if !ready {
				return errors.New("notification preceded terminal receipt commit")
			}
			return nil
		})
		if err != nil {
			t.Error(err)
		}
		select {
		case sent <- struct{}{}:
		default:
		}
		return `{"ok":true,"message_ts":"123.456"}`
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- RunRequests(ctx, f.Pool, nil, testProjectionConfig(), client, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("runner never sent private rejection")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if n, err := ReconcileRejectedFeedback(t.Context(), f.Pool, client, testProjectionConfig(), 4); err != nil || n != 0 {
		t.Fatalf("runner notification replayed: %d %v", n, err)
	}
}
