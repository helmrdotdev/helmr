package workspace

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

const (
	FencingKeySize = 32
	fencingDomain  = "helmr.workspace-fence.v0\x00"
)

type FenceInput struct {
	LeaseID                uuid.UUID
	WorkspaceID            uuid.UUID
	OwnershipGeneration    int64
	WriterGeneration       int64
	MountFencingGeneration int64
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
			"Workspace fencing key must be %d bytes, got %d",
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
	if input.LeaseID == uuid.Nil {
		return FencingCapability{}, errors.New("Workspace Lease ID is required")
	}
	if input.WorkspaceID == uuid.Nil {
		return FencingCapability{}, errors.New("Workspace ID is required")
	}
	if input.OwnershipGeneration <= 0 || input.WriterGeneration <= 0 || input.MountFencingGeneration <= 0 {
		return FencingCapability{}, errors.New("Workspace fencing generations must be positive")
	}

	message := make([]byte, 0, len(fencingDomain)+32+24)
	message = append(message, fencingDomain...)
	message = append(message, input.LeaseID[:]...)
	message = append(message, input.WorkspaceID[:]...)
	message = binary.BigEndian.AppendUint64(message, uint64(input.OwnershipGeneration))
	message = binary.BigEndian.AppendUint64(message, uint64(input.WriterGeneration))
	message = binary.BigEndian.AppendUint64(message, uint64(input.MountFencingGeneration))
	if !k.Valid() {
		return FencingCapability{}, errors.New("Workspace fencing key is invalid")
	}
	mac := hmac.New(sha256.New, k.value)
	_, _ = mac.Write(message)
	raw := mac.Sum(nil)
	sum := sha256.Sum256(raw)
	return FencingCapability{
		Token: base64.RawURLEncoding.EncodeToString(raw),
		Hash:  "sha256:" + hex.EncodeToString(sum[:]),
	}, nil
}
