package controlplane

import (
	"time"

	"github.com/helmrdotdev/helmr/internal/retry"
)

func taskRetryDelay(
	policy retry.Manifest,
	failedAttempt int32,
	sample func(int64) (int64, error),
) (time.Duration, bool, error) {
	return retry.Delay(policy, failedAttempt, sample)
}
