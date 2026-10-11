package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSessionDeliveryRetainsIdentityAndRejectsOldAttachment(t *testing.T) {
	f := newFixture(t)
	a, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	first, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
	if err != nil || first.Kind != "resume" || first.Sequence != 1 {
		t.Fatalf("first: %+v %v", first, err)
	}
	second, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
	if err != nil || second.Sequence != first.Sequence {
		t.Fatalf("retry: %+v %v", second, err)
	}
	if err := AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence, first.Sequence, first.Generation, first.Kind, ""); err != nil {
		t.Fatal(err)
	}
	acked, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
	if err != nil || !acked.Acknowledged {
		t.Fatalf("ack: %+v %v", acked, err)
	}
	b, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence); !errors.Is(err, ErrDenied) {
		t.Fatalf("old attachment: %v", err)
	}
	latest, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), b.Sequence)
	if err != nil || latest.Sequence != first.Sequence+1 || latest.Acknowledged {
		t.Fatalf("reattach: %+v %v", latest, err)
	}
	if err := AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence, first.Sequence, first.Generation, first.Kind, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("old ack: %v", err)
	}
}

func TestSessionDeliveryCurrentHoldAndPhysicalStopAreSeparate(t *testing.T) {
	f := newFixture(t)
	a, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	old, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	hold, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "interrupt", "hold"))
	if err != nil {
		t.Fatal(err)
	}
	if err := AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence, old.Sequence, old.Generation, old.Kind, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale resume: %v", err)
	}
	held, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
	if err != nil || held.Kind != "suspend" || held.Generation != 2 {
		t.Fatalf("held: %+v %v", held, err)
	}
	req := controlRequest(f, "resume", "release")
	req.HoldID = hold.HoldID
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), req); err != nil {
		t.Fatal(err)
	}
	resumed, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
	if err != nil || resumed.Kind != "resume" || resumed.Generation != 3 {
		t.Fatalf("resume: %+v %v", resumed, err)
	}
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), controlRequest(f, "cancel", "cancel")); err != nil {
		t.Fatal(err)
	}
	stop, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
	if err != nil || stop.Kind != "shutdown" {
		t.Fatalf("stop: %+v %v", stop, err)
	}
	if err := AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence, stop.Sequence, stop.Generation, stop.Kind, ""); err != nil {
		t.Fatal(err)
	}
	var fenced bool
	if err := f.pool.QueryRow(t.Context(), `SELECT fenced_at IS NOT NULL FROM session_processes WHERE session_id=$1`, f.session).Scan(&fenced); err != nil || fenced {
		t.Fatalf("command receipt fenced live process: %v %v", fenced, err)
	}
	for range 2 {
		if err := ObserveSessionStopped(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT status='stopped' AND fenced_at IS NOT NULL FROM session_processes WHERE session_id=$1`, f.session).Scan(&fenced); err != nil || !fenced {
		t.Fatalf("physical stop not recorded: %v %v", fenced, err)
	}
}

func TestSessionDeliveryRejectsExpiredOwnerWithoutAdvancing(t *testing.T) {
	f := newFixture(t)
	a, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'`)
	if _, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence); !errors.Is(err, ErrNotReady) {
		t.Fatalf("expired: %v", err)
	}
	var seq int64
	if err := f.pool.QueryRow(t.Context(), `SELECT control_sequence FROM session_processes`).Scan(&seq); err != nil || seq != 0 {
		t.Fatalf("sequence %d: %v", seq, err)
	}
}

func TestSessionDeliveryFailureIsInspectableAndDoesNotAcknowledge(t *testing.T) {
	f := newFixture(t)
	a, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	if err := AcknowledgeSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence, delivery.Sequence, delivery.Generation, delivery.Kind, "native work did not converge"); err != nil {
		t.Fatal(err)
	}
	failed, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
	if err != nil || failed.Acknowledged || failed.Error != "native work did not converge" || failed.Sequence != delivery.Sequence {
		t.Fatalf("failure receipt: %+v %v", failed, err)
	}
	if err := ObserveSessionStopped(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence); err != nil {
		t.Fatalf("physical stop after control failure: %v", err)
	}
}

func TestSessionDeliveryUnchangedObservationDoesNotLockTree(t *testing.T) {
	f := newFixture(t)
	a, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	first, err := PrepareSessionDelivery(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), `SELECT id FROM sessions WHERE id=$1 FOR NO KEY UPDATE`, f.session); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	next, err := PrepareSessionDelivery(ctx, f.pool, *f.host(), f.execution(), a.Sequence)
	if err != nil || next.Sequence != first.Sequence {
		t.Fatalf("unchanged observation waited on tree lock: %+v %v", next, err)
	}
}
