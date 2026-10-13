package auth

import (
	"crypto/sha256"
)

func HashCredential(raw string) []byte {
	sum := sha256.Sum256([]byte(raw))
	return append([]byte(nil), sum[:]...)
}
