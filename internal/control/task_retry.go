package control

import (
	"crypto/rand"
	"errors"
	"math/big"
	"time"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

func taskRetryDelay(
	policy deployment.RetryManifest,
	failedAttempt int32,
	sample func(int64) (int64, error),
) (time.Duration, bool, error) {
	if failedAttempt <= 0 {
		return 0, false, errors.New("failed Task Attempt number must be positive")
	}
	if !policy.Enabled {
		return 0, false, nil
	}
	if policy.MaxAttempts == nil || policy.Backoff == nil {
		return 0, false, errors.New("retry manifest is incomplete")
	}
	if *policy.MaxAttempts < 1 || policy.Backoff.MinMs < 1 ||
		policy.Backoff.MaxMs < policy.Backoff.MinMs || policy.Backoff.Factor < 1 {
		return 0, false, errors.New("retry manifest is invalid")
	}
	if int64(failedAttempt) >= *policy.MaxAttempts {
		return 0, false, nil
	}
	base := policy.Backoff.MinMs
	for attempt := int32(1); attempt < failedAttempt && base < policy.Backoff.MaxMs; attempt++ {
		if base > policy.Backoff.MaxMs/policy.Backoff.Factor {
			base = policy.Backoff.MaxMs
			break
		}
		base *= policy.Backoff.Factor
		if base > policy.Backoff.MaxMs {
			base = policy.Backoff.MaxMs
		}
	}
	delay := base
	if policy.Backoff.Jitter == deployment.RetryJitterFull {
		if sample == nil {
			sample = sampleRetryMilliseconds
		}
		var err error
		delay, err = sample(base)
		if err != nil {
			return 0, false, err
		}
		if delay < 0 || delay > base {
			return 0, false, errors.New("retry jitter sample is outside its inclusive bound")
		}
	}
	return time.Duration(delay) * time.Millisecond, true, nil
}

func sampleRetryMilliseconds(maximum int64) (int64, error) {
	if maximum < 0 {
		return 0, errors.New("retry jitter maximum is negative")
	}
	value, err := rand.Int(rand.Reader, big.NewInt(maximum+1))
	if err != nil {
		return 0, err
	}
	return value.Int64(), nil
}
