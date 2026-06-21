package workspaceop

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

func requestFingerprint(operationKind string, requestJSON string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(strings.TrimSpace(operationKind)))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(strings.TrimSpace(requestJSON)))
	return hex.EncodeToString(hash.Sum(nil))
}

func CanonicalRequestFingerprint(operationKind string, requestJSON []byte) (string, error) {
	canonical, err := CanonicalJSON(requestJSON)
	if err != nil {
		return "", err
	}
	return requestFingerprint(operationKind, string(canonical)), nil
}

func CanonicalJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode workspace operation request JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("workspace operation request JSON contains trailing data")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode canonical workspace operation request JSON: %w", err)
	}
	return canonical, nil
}
