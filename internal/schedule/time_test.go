package schedule

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/ids"
)

func TestNextCronTimeUsesTimezone(t *testing.T) {
	next, err := NextCronTime("0 9 * * *", "Asia/Tokyo", time.Date(2026, 6, 1, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
}

func TestRetryDelayCapsAtOneHour(t *testing.T) {
	if got := RetryDelay(2); got != 4*time.Minute {
		t.Fatalf("retry delay = %s", got)
	}
	if got := RetryDelay(100); got != time.Hour {
		t.Fatalf("capped retry delay = %s", got)
	}
}

func TestDefaultDedupKeyIsStable(t *testing.T) {
	first := DefaultDedupKey("task", "0 * * * *")
	second := DefaultDedupKey("task", "0 * * * *")
	if first != second {
		t.Fatalf("dedup key changed: %q != %q", first, second)
	}
	if first == DefaultDedupKey("task", "15 * * * *") {
		t.Fatal("dedup key did not include cron expression")
	}
}

func TestJitterStaysWithinWindow(t *testing.T) {
	got := Jitter(ids.New(), 30*time.Second)
	if got < 0 || got >= 30*time.Second {
		t.Fatalf("jitter outside window: %s", got)
	}
}
