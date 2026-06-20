package workspaceop

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

func RequestFingerprint(operationKind string, requestJSON string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(strings.TrimSpace(operationKind)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(strings.TrimSpace(requestJSON)))
	return hex.EncodeToString(hash.Sum(nil))
}
