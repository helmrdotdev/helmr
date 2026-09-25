package computerkey

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

const testARN = "arn:aws:kms:us-east-1:111122223333:key/11111111-1111-1111-1111-111111111111"

type kmsFake struct {
	encrypt func(*kms.EncryptInput) (*kms.EncryptOutput, error)
	decrypt func(*kms.DecryptInput) (*kms.DecryptOutput, error)
}

func (f kmsFake) Encrypt(_ context.Context, in *kms.EncryptInput, _ ...func(*kms.Options)) (*kms.EncryptOutput, error) {
	return f.encrypt(in)
}
func (f kmsFake) Decrypt(_ context.Context, in *kms.DecryptInput, _ ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	return f.decrypt(in)
}
func TestKMSContextAndProviderIdentity(t *testing.T) {
	plain := bytes.Repeat([]byte{3}, Size)
	client := kmsFake{
		encrypt: func(in *kms.EncryptInput) (*kms.EncryptOutput, error) {
			if aws.ToString(in.KeyId) != testARN || in.EncryptionAlgorithm != types.EncryptionAlgorithmSpecSymmetricDefault || in.EncryptionContext["computer_scope"] != "scope" || in.EncryptionContext["key_id"] != "version" || !bytes.Equal(in.Plaintext, plain) {
				t.Fatal("unbound wrapping request")
			}
			return &kms.EncryptOutput{KeyId: aws.String(testARN), EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault, CiphertextBlob: []byte("wrapped")}, nil
		},
		decrypt: func(in *kms.DecryptInput) (*kms.DecryptOutput, error) {
			if aws.ToString(in.KeyId) != testARN || in.EncryptionAlgorithm != types.EncryptionAlgorithmSpecSymmetricDefault || in.EncryptionContext["purpose"] != "helmr.computer-key.v1" || in.EncryptionContext["computer_scope"] != "scope" || in.EncryptionContext["key_id"] != "version" || string(in.CiphertextBlob) != "wrapped" {
				t.Fatal("unbound decrypt request")
			}
			return &kms.DecryptOutput{KeyId: aws.String(testARN), EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault, Plaintext: bytes.Clone(plain)}, nil
		},
	}
	adapter, err := NewKMS(client, testARN)
	if err != nil {
		t.Fatal(err)
	}
	e, err := adapter.Wrap(t.Context(), "scope", "version", plain)
	if err != nil {
		t.Fatal(err)
	}
	got, err := adapter.Unwrap(t.Context(), "scope", "version", e)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatal(err)
	}
	client.decrypt = func(*kms.DecryptInput) (*kms.DecryptOutput, error) { return nil, errors.New("provider unavailable") }
	adapter, _ = NewKMS(client, testARN)
	if got, err := adapter.Unwrap(t.Context(), "scope", "version", e); err == nil || got != nil {
		t.Fatal("provider failure returned material")
	}
}
func TestKMSRejectsMismatchedPlaintextResponse(t *testing.T) {
	returned := bytes.Repeat([]byte{7}, Size)
	adapter, _ := NewKMS(kmsFake{decrypt: func(*kms.DecryptInput) (*kms.DecryptOutput, error) {
		return &kms.DecryptOutput{KeyId: aws.String("wrong"), EncryptionAlgorithm: types.EncryptionAlgorithmSpecSymmetricDefault, Plaintext: returned}, nil
	}}, testARN)
	if got, err := adapter.Unwrap(t.Context(), "scope", "version", Envelope{testARN, []byte("wrapped")}); err == nil || got != nil {
		t.Fatal("wrong provider key accepted")
	}
	if !bytes.Equal(returned, make([]byte, Size)) {
		t.Fatal("rejected plaintext not cleared")
	}
}
