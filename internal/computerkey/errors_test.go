package computerkey

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/aws/smithy-go"
)

// A failed KMS call is provider unavailability only when it is a recognized
// transport, throttling, provider-internal or deadline failure; access,
// key-state and ciphertext rejections keep only their cause, and so does a
// failure after the caller's context ended.
func TestKMSClassifiesProviderFailures(t *testing.T) {
	for _, test := range []struct {
		name        string
		err         error
		unavailable bool
	}{
		{"transport", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{"throttling", &smithy.GenericAPIError{Code: "ThrottlingException", Message: "rate exceeded"}, true},
		{"internal", &types.KMSInternalException{Message: aws.String("internal")}, true},
		{"dependency timeout", &types.DependencyTimeoutException{Message: aws.String("timeout")}, true},
		{"key unavailable", &types.KeyUnavailableException{Message: aws.String("retry later")}, true},
		{"deadline", context.DeadlineExceeded, true},
		{"invalid ciphertext", &types.InvalidCiphertextException{Message: aws.String("bad")}, false},
		{"access denied", &smithy.GenericAPIError{Code: "AccessDeniedException", Message: "denied"}, false},
		{"disabled key", &types.DisabledException{Message: aws.String("disabled")}, false},
		{"missing key", &types.NotFoundException{Message: aws.String("missing")}, false},
		{"key state", &types.KMSInvalidStateException{Message: aws.String("pending deletion")}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter, err := NewKMS(kmsFake{
				encrypt: func(*kms.EncryptInput) (*kms.EncryptOutput, error) { return nil, test.err },
				decrypt: func(*kms.DecryptInput) (*kms.DecryptOutput, error) { return nil, test.err },
			}, testARN)
			if err != nil {
				t.Fatal(err)
			}
			_, wrapErr := adapter.Wrap(t.Context(), "scope", "version", bytes.Repeat([]byte{3}, Size))
			got, unwrapErr := adapter.Unwrap(t.Context(), "scope", "version", Envelope{testARN, []byte("wrapped")})
			for _, err := range []error{wrapErr, unwrapErr} {
				if !errors.Is(err, test.err) || errors.Is(err, ErrUnavailable) != test.unavailable || errors.Is(err, ErrInvalidEnvelope) {
					t.Fatalf("classified as %v", err)
				}
			}
			if got != nil {
				t.Fatal("failure returned material")
			}
		})
	}
}

// A provider failure after the caller's context ended keeps its cause and is
// not provider unavailability.
func TestKMSCancelledCallerIsNotUnavailability(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	adapter, err := NewKMS(kmsFake{decrypt: func(*kms.DecryptInput) (*kms.DecryptOutput, error) {
		return nil, &net.OpError{Op: "dial", Err: context.Canceled}
	}}, testARN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = adapter.Unwrap(ctx, "scope", "version", Envelope{testARN, []byte("wrapped")}); err == nil || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrInvalidEnvelope) {
		t.Fatalf("cancelled caller classified as %v", err)
	}
}

// A persisted envelope KMS cannot accept is invalid before any provider call;
// an invalid provider response is neither unavailability nor an invalid
// envelope.
func TestKMSClassifiesEnvelopesAndResponses(t *testing.T) {
	called := false
	adapter, err := NewKMS(kmsFake{decrypt: func(*kms.DecryptInput) (*kms.DecryptOutput, error) {
		called = true
		return &kms.DecryptOutput{KeyId: aws.String(testARN), EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault, Plaintext: []byte("short")}, nil
	}}, testARN)
	if err != nil {
		t.Fatal(err)
	}
	for name, envelope := range map[string]Envelope{"bad ARN": {"not-an-arn", []byte("wrapped")}, "empty ciphertext": {testARN, nil}, "oversized ciphertext": {testARN, make([]byte, MaxWrappedSize+1)}} {
		if _, err := adapter.Unwrap(t.Context(), "scope", "version", envelope); !errors.Is(err, ErrInvalidEnvelope) || called {
			t.Fatalf("%s classified as %v (provider called %v)", name, err, called)
		}
	}
	if _, err := adapter.Unwrap(t.Context(), "scope", "version", Envelope{testARN, []byte("wrapped")}); err == nil || errors.Is(err, ErrInvalidEnvelope) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("invalid response classified as %v", err)
	}
}

// A local envelope for another wrapping key or with tampered ciphertext is
// invalid; a cancelled caller keeps its cause.
func TestLocalClassifiesEnvelopes(t *testing.T) {
	local, err := NewLocal("local-1", bytes.Repeat([]byte{1}, Size))
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := local.Wrap(t.Context(), "scope", "version", bytes.Repeat([]byte{2}, Size))
	if err != nil {
		t.Fatal(err)
	}
	foreign := envelope
	foreign.WrappingKeyID = "local-2"
	tampered := Envelope{WrappingKeyID: envelope.WrappingKeyID, Ciphertext: bytes.Clone(envelope.Ciphertext)}
	tampered.Ciphertext[len(tampered.Ciphertext)-1] ^= 1
	for name, e := range map[string]Envelope{"foreign key": foreign, "tampered": tampered} {
		if got, err := local.Unwrap(t.Context(), "scope", "version", e); !errors.Is(err, ErrInvalidEnvelope) || got != nil {
			t.Fatalf("%s classified as %v", name, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := local.Unwrap(ctx, "scope", "version", envelope); !errors.Is(err, context.Canceled) || errors.Is(err, ErrInvalidEnvelope) || errors.Is(err, ErrUnavailable) {
		t.Fatalf("cancelled caller classified as %v", err)
	}
}
