package computerkey

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/arn"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// KMSAPI is the provider boundary used by this wrapping adapter.
type KMSAPI interface {
	Encrypt(context.Context, *kms.EncryptInput, ...func(*kms.Options)) (*kms.EncryptOutput, error)
	Decrypt(context.Context, *kms.DecryptInput, ...func(*kms.Options)) (*kms.DecryptOutput, error)
}
type KMS struct {
	client KMSAPI
	keyARN string
}

func validARN(s string) bool {
	a, err := arn.Parse(s)
	return err == nil && a.Service == "kms" && a.Region != "" && a.AccountID != "" && strings.HasPrefix(a.Resource, "key/") && len(a.Resource) > 4 && len(s) <= 2048
}
func NewKMS(client KMSAPI, keyARN string) (*KMS, error) {
	if client == nil || !validARN(keyARN) {
		return nil, errors.New("KMS client and wrapping key ARN are required")
	}
	return &KMS{client: client, keyARN: keyARN}, nil
}
func kmsContext(scope, keyID string) (map[string]string, error) {
	if _, err := contextBytes(scope, keyID); err != nil {
		return nil, err
	}
	return map[string]string{"purpose": "helmr.computer-key.v1", "computer_scope": scope, "key_id": keyID}, nil
}
func (k *KMS) Wrap(ctx context.Context, scope, keyID string, plain []byte) (Envelope, error) {
	aad, err := kmsContext(scope, keyID)
	if err != nil {
		return Envelope{}, err
	}
	if len(plain) != Size {
		return Envelope{}, errors.New("invalid computer data key")
	}
	out, err := k.client.Encrypt(ctx, &kms.EncryptInput{KeyId: aws.String(k.keyARN), EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault, EncryptionContext: aad, Plaintext: plain})
	if err != nil {
		return Envelope{}, err
	}
	if out == nil || aws.ToString(out.KeyId) != k.keyARN || out.EncryptionAlgorithm != types.EncryptionAlgorithmSpecSymmetricDefault || len(out.CiphertextBlob) == 0 || len(out.CiphertextBlob) > MaxWrappedSize {
		return Envelope{}, errors.New("invalid KMS wrapping response")
	}
	return Envelope{WrappingKeyID: k.keyARN, Ciphertext: out.CiphertextBlob}, nil
}
func (k *KMS) Unwrap(ctx context.Context, scope, keyID string, e Envelope) ([]byte, error) {
	aad, err := kmsContext(scope, keyID)
	if err != nil {
		return nil, err
	}
	// Only the broker supplies persisted envelopes. The Worker cannot select this ARN.
	if !validARN(e.WrappingKeyID) || len(e.Ciphertext) == 0 || len(e.Ciphertext) > MaxWrappedSize {
		return nil, errors.New("invalid KMS envelope")
	}
	out, err := k.client.Decrypt(ctx, &kms.DecryptInput{KeyId: aws.String(e.WrappingKeyID), EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault, EncryptionContext: aad, CiphertextBlob: e.Ciphertext})
	if err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errors.New("empty KMS unwrapping response")
	}
	if aws.ToString(out.KeyId) != e.WrappingKeyID || out.EncryptionAlgorithm != types.EncryptionAlgorithmSpecSymmetricDefault || len(out.Plaintext) != Size {
		clear(out.Plaintext)
		return nil, errors.New("invalid KMS unwrapping response")
	}
	return out.Plaintext, nil
}
