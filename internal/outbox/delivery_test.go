package outbox

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRetryAfter(t *testing.T) {
	for attempt, want := range map[int32]time.Duration{
		0: time.Second, 1: time.Second, 2: 2 * time.Second,
		6: 32 * time.Second, 7: time.Minute, 20: time.Minute,
	} {
		if got := RetryAfter(attempt); got != want {
			t.Fatalf("RetryAfter(%d) = %s, want %s", attempt, got, want)
		}
	}
}

func TestErrorBoundsMessage(t *testing.T) {
	raw := Error(errors.New(string(make([]byte, 4096))+"界"+string(make([]byte, 4096))), "delivery failed")
	if !raw.Valid || raw.String != "界" {
		t.Fatalf("message = %+v", raw)
	}

	raw = Error(errors.New(strings.Repeat("界", 2048)), "delivery failed")
	if !raw.Valid || len(raw.String) > maxErrorBytes || !utf8.ValidString(raw.String) {
		t.Fatalf("bounded message = %+v", raw)
	}
}

func TestErrorFallsBackForBlankMessage(t *testing.T) {
	for _, cause := range []error{nil, errors.New(" \t\n"), errors.New("\x00")} {
		raw := Error(cause, "delivery failed")
		if !raw.Valid || raw.String != "delivery failed" {
			t.Fatalf("message = %+v", raw)
		}
	}
}
