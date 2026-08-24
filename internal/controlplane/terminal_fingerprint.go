package controlplane

import (
	"crypto/sha256"
	"encoding/json"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func terminalRequestFingerprint(scope string, payload any) (string, error) {
	body, err := json.Marshal(struct {
		Scope   string `json:"scope"`
		Payload any    `json:"payload"`
	}{Scope: scope, Payload: payload})
	if err != nil {
		return "", err
	}
	canonical, err := canonicalJSON(body)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return sha256sum.FormatDigest(sum[:]), nil
}
