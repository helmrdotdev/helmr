package controlplane

import (
	"time"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/retry"
)

func taskRetryDelay(
	policy deployment.RetryManifest,
	failedAttempt int32,
	sample func(int64) (int64, error),
) (time.Duration, bool, error) {
	return retry.Delay(policy, failedAttempt, sample)
}
