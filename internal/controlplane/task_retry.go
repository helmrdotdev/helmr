package controlplane

import (
	"time"

	"github.com/helmrdotdev/helmr/internal/definition"
)

func taskRetryDelay(
	policy definition.RetryManifest,
	failedAttempt int32,
	sample func(int64) (int64, error),
) (time.Duration, bool, error) {
	return definition.RetryDelay(policy, failedAttempt, sample)
}
