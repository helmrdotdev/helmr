package disk

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const (
	FencingKeySize = 32
	fencingDomain  = "helmr.computer-fence.v0\x00"
)

type FenceInput struct {
	InstanceID       uuid.UUID
	ComputerID       uuid.UUID
	WriterGeneration int64
}

type FencingCapability struct {
	Token string
	Hash  string
}

type FencingKey struct {
	value []byte
}

func NewFencingKey(raw []byte) (FencingKey, error) {
	if len(raw) != FencingKeySize {
		return FencingKey{}, fmt.Errorf(
			"computer fencing key must be %d bytes, got %d",
			FencingKeySize,
			len(raw),
		)
	}
	return FencingKey{value: append([]byte(nil), raw...)}, nil
}

func (k FencingKey) Valid() bool {
	return len(k.value) == FencingKeySize
}

func (k FencingKey) Derive(input FenceInput) (FencingCapability, error) {
	if input.InstanceID == uuid.Nil() {
		return FencingCapability{}, errors.New("computer instance ID is required")
	}
	if input.ComputerID == uuid.Nil() {
		return FencingCapability{}, errors.New("computer ID is required")
	}
	if input.WriterGeneration <= 0 {
		return FencingCapability{}, errors.New("computer fencing generations must be positive")
	}

	message := make([]byte, 0, len(fencingDomain)+32+8)
	message = append(message, fencingDomain...)
	message = append(message, input.InstanceID[:]...)
	message = append(message, input.ComputerID[:]...)
	message = binary.BigEndian.AppendUint64(message, uint64(input.WriterGeneration))
	if !k.Valid() {
		return FencingCapability{}, errors.New("computer fencing key is invalid")
	}
	mac := hmac.New(sha256.New, k.value)
	_, _ = mac.Write(message)
	raw := mac.Sum(nil)
	sum := sha256.Sum256(raw)
	return FencingCapability{
		Token: base64.RawURLEncoding.EncodeToString(raw),
		Hash:  sha256sum.FormatDigest(sum[:]),
	}, nil
}
