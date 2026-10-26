package slack

import (
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSlackOperationOwnersWakeRequestAndDeliveryConsumers(t *testing.T) {
	f := newStatusFixture(t)
	topics := dbtest.ListenChanges(t, f.Pool)
	gesture := f.gesture("notify-request")
	if _, err := receiveGesture(t.Context(), f.Pool, gesture); err != nil {
		t.Fatal(err)
	}
	if got := topics(); !got["slack_requests"] {
		t.Fatalf("new request did not notify: %v", got)
	}
	if _, err := receiveGesture(t.Context(), f.Pool, gesture); err != nil {
		t.Fatal(err)
	}
	if got := topics(); len(got) != 0 {
		t.Fatalf("retained receipt created work: %v", got)
	}
	// An opening confirmation makes queued replies eligible immediately.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE slack_threads SET thread_ts=NULL WHERE id=$1`, f.thread)
	opening := f.opening(t)
	claim := f.postClaim(t, opening)
	_ = topics()
	if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: Acknowledged, Timestamp: "123.789"}); err != nil {
		t.Fatal(err)
	}
	if got := topics(); !got["slack_delivery"] {
		t.Fatalf("opening did not notify dependent delivery: %v", got)
	}
	// Projection of a real Session event publishes new desired content.
	if progressed, err := projectParticipant(t.Context(), f.Pool, f.participant, testProjectionConfig()); err != nil || !progressed {
		t.Fatalf("project source: %v %v", progressed, err)
	}
	if got := topics(); !got["slack_delivery"] {
		t.Fatalf("projection did not notify delivery: %v", got)
	}
}

func TestStreamReconciliationNotifiesNewerDeliveryAndIgnoresReplay(t *testing.T) {
	f := newStatusFixture(t)
	post := f.streamPost(t, 1, "first")
	claim := f.postClaim(t, post)
	if err := FinishPost(t.Context(), f.Pool, auditpolicy.Policy{RetentionSeconds: 3600}, claim, DeliveryResult{Disposition: Uncertain}); err != nil {
		t.Fatal(err)
	}
	f.reviseStream(t, post, "first and newer content")
	observed := observedStream(t, f, post, "first", "in_progress")
	topics := dbtest.ListenChanges(t, f.Pool)
	if confirmed, err := confirmMessage(t.Context(), f.Pool, f.installation, observed); err != nil || !confirmed {
		t.Fatalf("positive stream confirmation: %v %v", confirmed, err)
	}
	if got := topics(); !got["slack_delivery"] {
		t.Fatalf("confirmed stream did not wake delivery: %v", got)
	}
	var pending bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT p.status='pending' AND p.desired_revision>p.confirmed_revision AND t.stream_status_repair FROM slack_posts p JOIN slack_threads t ON t.id=p.thread_id WHERE p.id=$1`, post).Scan(&pending); err != nil || !pending {
		t.Fatalf("newer stream work or status repair missing: %v %v", pending, err)
	}
	if confirmed, err := confirmMessage(t.Context(), f.Pool, f.installation, observed); err != nil || confirmed {
		t.Fatalf("replayed stream confirmation changed state: %v %v", confirmed, err)
	}
	if got := topics(); len(got) != 0 {
		t.Fatalf("stream confirmation replay notified: %v", got)
	}
}
