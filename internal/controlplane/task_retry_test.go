package controlplane

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/retry"
)

func TestTaskRetryDelayUsesAttemptCountAndCappedIntegerBackoff(t *testing.T) {
	maxAttempts := int64(5)
	policy := retry.Manifest{
		Enabled:     true,
		MaxAttempts: &maxAttempts,
		Backoff: &retry.Backoff{
			MinMs: 1000, MaxMs: 3000, Factor: 2, Jitter: retry.JitterNone,
		},
	}
	for attempt, want := range []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 3 * time.Second} {
		delay, retry, err := taskRetryDelay(policy, int32(attempt+1), nil)
		if err != nil || !retry || delay != want {
			t.Fatalf("Attempt %d = %s, %t, %v; want %s", attempt+1, delay, retry, err, want)
		}
	}
	if _, retry, err := taskRetryDelay(policy, 5, nil); err != nil || retry {
		t.Fatalf("exhausted retry = %t, %v", retry, err)
	}
}

func TestTaskRetryDelayUsesInclusiveFullJitter(t *testing.T) {
	maxAttempts := int64(2)
	policy := retry.Manifest{
		Enabled:     true,
		MaxAttempts: &maxAttempts,
		Backoff: &retry.Backoff{
			MinMs: 1000, MaxMs: 3000, Factor: 2, Jitter: retry.JitterFull,
		},
	}
	delay, retry, err := taskRetryDelay(policy, 1, func(maximum int64) (int64, error) {
		if maximum != 1000 {
			t.Fatalf("maximum = %d", maximum)
		}
		return maximum, nil
	})
	if err != nil || !retry || delay != time.Second {
		t.Fatalf("full jitter = %s, %t, %v", delay, retry, err)
	}
}
