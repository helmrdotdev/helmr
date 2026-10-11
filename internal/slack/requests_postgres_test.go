package slack

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func (f statusFixture) gesture(key string) inboundGesture {
	now := time.Now().UTC()
	return inboundGesture{Installation: f.installation, Key: key, Actor: "human", OccurredAt: now, ExpiresAt: now.Add(time.Minute), Payload: json.RawMessage(`{"channel":"C1","text":"work"}`)}
}

func TestSlackRequestConcurrentDeliveryRetainsOneGestureIdentity(t *testing.T) {
	f := newStatusFixture(t)
	gesture := f.gesture("event-1")
	var wg sync.WaitGroup
	receipts := make(chan requestReceipt, 8)
	failures := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			receipt, err := receiveGesture(t.Context(), f.Pool, gesture)
			receipts <- receipt
			failures <- err
		})
	}
	wg.Wait()
	close(receipts)
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first requestReceipt
	for receipt := range receipts {
		if first.ID == uuid.Nil() {
			first = receipt
		}
		if first != receipt || receipt.Status != "received" {
			t.Fatalf("duplicate gesture changed identity: %+v %+v", first, receipt)
		}
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_requests WHERE installation_id=$1`, f.installation).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate receipts: %d %v", count, err)
	}
	gesture.Payload = json.RawMessage(`{"text":"work","channel":"C1"}`)
	if retry, err := receiveGesture(t.Context(), f.Pool, gesture); err != nil || retry != first {
		t.Fatalf("canonical retry conflicted: %+v %v", retry, err)
	}
	gesture.Payload = json.RawMessage(`{"channel":"C1","text":"different"}`)
	if _, err := receiveGesture(t.Context(), f.Pool, gesture); !errors.Is(err, errGestureConflict) {
		t.Fatalf("changed content did not conflict: %v", err)
	}
}

func TestSlackRequestCutoffAndExpiredReceiptNeverAdmitNewWork(t *testing.T) {
	for _, kind := range []string{"lost", "old-source", "expired"} {
		t.Run(kind, func(t *testing.T) {
			f := newStatusFixture(t)
			gesture := f.gesture("event-1")
			switch kind {
			case "lost":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_installations SET authorization_lost_at=clock_timestamp() WHERE id=$1`, f.installation)
			case "old-source":
				gesture.OccurredAt = gesture.OccurredAt.Add(-time.Hour)
			case "expired":
				gesture.ExpiresAt = gesture.OccurredAt.Add(-time.Second)
			}
			receipt, err := receiveGesture(t.Context(), f.Pool, gesture)
			if err != nil || receipt.Status != "rejected" {
				t.Fatalf("stale input not rejected: %+v %v", receipt, err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_requests SET payload=NULL,payload_expired_at=clock_timestamp() WHERE id=$1`, receipt.ID)
			gesture.ExpiresAt = time.Now().Add(time.Hour)
			retry, err := receiveGesture(t.Context(), f.Pool, gesture)
			if err != nil || retry != receipt {
				t.Fatalf("retired payload revived gesture: %+v %v", retry, err)
			}
			var turns int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE environment_id=$1`, f.Environment).Scan(&turns); err != nil || turns != 0 {
				t.Fatalf("receipt admitted work: %d %v", turns, err)
			}
		})
	}
}

func TestSlackRequestDigestBindsActorAndSourceTime(t *testing.T) {
	f := newStatusFixture(t)
	gesture := f.gesture("event-1")
	if _, err := receiveGesture(t.Context(), f.Pool, gesture); err != nil {
		t.Fatal(err)
	}
	changed := gesture
	changed.Actor = "another-human"
	if _, err := receiveGesture(t.Context(), f.Pool, changed); !errors.Is(err, errGestureConflict) {
		t.Fatal("actor changed under retry key")
	}
	changed = gesture
	changed.OccurredAt = changed.OccurredAt.Add(time.Second)
	if _, err := receiveGesture(t.Context(), f.Pool, changed); !errors.Is(err, errGestureConflict) {
		t.Fatal("source timestamp changed under retry key")
	}
}
