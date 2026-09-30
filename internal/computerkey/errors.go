package computerkey

import (
	"context"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// ErrUnavailable reports that the wrapping provider could not serve the
// request: a transport failure, throttling, a provider-internal failure or a
// provider deadline. The request may succeed when retried.
var ErrUnavailable = errors.New("computer key provider is unavailable")

// ErrInvalidEnvelope reports a persisted envelope the provider rejects: a
// wrapping key identity or ciphertext it cannot accept, ciphertext that fails
// authentication, or ciphertext wrapped under another key.
var ErrInvalidEnvelope = errors.New("computer key envelope is invalid")

func invalidEnvelope(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidEnvelope, reason)
}

var kmsThrottles = retry.ThrottleErrorCode{Codes: retry.DefaultThrottleErrorCodes}

// kmsDecryptFailure classifies a failed KMS decrypt: ciphertext KMS cannot
// authenticate in its context, or that another wrapping key produced, is an
// invalid envelope; any other failure is classified as kmsFailure does.
func kmsDecryptFailure(ctx context.Context, err error) error {
	var invalid *types.InvalidCiphertextException
	var incorrect *types.IncorrectKeyException
	if ctx.Err() == nil && (errors.As(err, &invalid) || errors.As(err, &incorrect)) {
		return fmt.Errorf("%w: %w", ErrInvalidEnvelope, err)
	}
	return kmsFailure(ctx, err)
}

// kmsFailure classifies a failed KMS call. Recognized unavailability reports
// ErrUnavailable; a cancelled or expired caller context and every other
// failure keep only their cause.
func kmsFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return err
	}
	var internal *types.KMSInternalException
	var dependency *types.DependencyTimeoutException
	var keyUnavailable *types.KeyUnavailableException
	switch {
	case errors.As(err, &internal), errors.As(err, &dependency), errors.As(err, &keyUnavailable),
		errors.Is(err, context.DeadlineExceeded),
		kmsThrottles.IsErrorThrottle(err) == aws.TrueTernary,
		retry.RetryableConnectionError{}.IsErrorRetryable(err) == aws.TrueTernary:
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	default:
		return err
	}
}
