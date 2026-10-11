package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestRuntimeTurnRechecksLeaseAfterProcessLock(t *testing.T) {
	f := newFixture(t)
	admission := f.enqueue(t, "dispatch")
	a, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()+interval '1 second'`)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(t.Context(), `SELECT epoch FROM session_processes FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := DispatchRuntimeTurn(t.Context(), f.pool, *f.host(), f.execution(), a.Sequence)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err = f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%attachment_sequence%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dispatch did not wait for process lock")
		}
		time.Sleep(time.Millisecond)
	}
	for {
		var expired bool
		if err = f.pool.QueryRow(t.Context(), `SELECT expires_at<=clock_timestamp() FROM computer_leases`).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lease did not expire")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil {
		t.Fatal("expired dispatch was accepted")
	}
	var status string
	if err = f.pool.QueryRow(t.Context(), `SELECT status FROM turns WHERE id=$1`, admission.TurnID).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("expired dispatch mutated Turn: %s %v", status, err)
	}
}

func TestRuntimeTurnReceiptMatchesPublishedResult(t *testing.T) {
	f := newFixture(t)
	s := newSaveStorageFixture(t, f)
	a, save := f.finalize(t, "terminal-receipt")
	attachment, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), f.execution())
	if err != nil {
		t.Fatal(err)
	}
	outcome := json.RawMessage(`{"status":"completed","result":{"result":1}}`)
	if _, err := ObserveRuntimeTurn(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence, a.TurnID, outcome); err == nil {
		t.Fatal("prepared result acknowledged as completed")
	}
	root, _ := s.cut(t, 8)
	if err := s.publisher.Capture(t.Context(), s.ref(save.ID), root, "owned cut"); err != nil {
		t.Fatal(err)
	}
	if err := s.publish(t, save.ID, root); err != nil {
		t.Fatal(err)
	}
	if err := Complete(t.Context(), f.pool, f.env, f.session, a.TurnID); err != nil {
		t.Fatal(err)
	}
	// Current physical ownership may observe a prior outcome after a hold changes
	// business authority; it cannot rewrite the retained result.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE sessions SET authority_generation=authority_generation+1 WHERE id=$1`, f.session)
	for range 2 {
		sequence, err := ObserveRuntimeTurn(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence, a.TurnID, outcome)
		if err != nil || sequence != a.Sequence {
			t.Fatalf("terminal receipt: %d %v", sequence, err)
		}
	}
	if _, err := ObserveRuntimeTurn(t.Context(), f.pool, *f.host(), f.execution(), attachment.Sequence, a.TurnID, json.RawMessage(`{"status":"completed","result":{"result":2}}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed result: %v", err)
	}
}
