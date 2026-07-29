package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
)

const MACKeySize = 32

func ValidateMACKey(key []byte) error {
	if len(key) != MACKeySize {
		return fmt.Errorf("authentication MAC key must be exactly %d bytes", MACKeySize)
	}
	return nil
}

func HashToken(key []byte, raw string) ([]byte, error) {
	if err := ValidateMACKey(key); err != nil {
		return nil, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrUnauthenticated
	}
	return MAC(key, []byte(raw))
}

func MAC(key []byte, values ...[]byte) ([]byte, error) {
	if err := ValidateMACKey(key); err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	var size [8]byte
	for _, value := range values {
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = mac.Write(size[:])
		_, _ = mac.Write(value)
	}
	return mac.Sum(nil), nil
}
