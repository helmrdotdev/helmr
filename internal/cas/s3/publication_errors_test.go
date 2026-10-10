package s3

import (
	"context"
	"errors"
	"github.com/aws/smithy-go"
	"github.com/helmrdotdev/helmr/internal/cas"
	"testing"
)

func TestPublicationFailureRetainsProviderRetryClassification(t *testing.T) {
	for _, test := range []struct {
		code  string
		retry bool
	}{{"SlowDown", true}, {"InternalError", true}, {"AccessDenied", false}, {"InvalidRequest", false}} {
		t.Run(test.code, func(t *testing.T) {
			cause := &smithy.GenericAPIError{Code: test.code, Message: "provider response"}
			if test.code == "InternalError" {
				cause.Fault = smithy.FaultServer
			}
			err := storageFailure(t.Context(), cause)
			if errors.Is(err, cas.ErrUnavailable) != test.retry || !errors.Is(err, cause) {
				t.Fatalf("classification: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if errors.Is(storageFailure(ctx, &smithy.GenericAPIError{Code: "SlowDown"}), cas.ErrUnavailable) {
		t.Fatal("caller cancellation became retryable")
	}
	if errors.Is(storageFailure(t.Context(), cas.ErrDigestMismatch), cas.ErrUnavailable) {
		t.Fatal("integrity failure became retryable")
	}
}
