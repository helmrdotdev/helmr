package computer

import (
	"encoding/json"
	"errors"
	"unicode/utf8"
)

// EncryptionScope binds ciphertext to immutable server-owned identities. It is
// authenticated context, not authorization; callers must obtain these identities
// from the admitted Computer, never from guest input or human-readable labels.
func EncryptionScope(orgID, environmentID, computerID string) (string, error) {
	for _, id := range []string{orgID, environmentID, computerID} {
		if id == "" || !utf8.ValidString(id) {
			return "", errors.New("invalid computer encryption identity")
		}
	}
	encoded, err := json.Marshal([3]string{orgID, environmentID, computerID})
	if err != nil {
		return "", err
	}
	scope := "helmr.computer.v1:" + string(encoded)
	if len(scope) > 256 {
		return "", errors.New("computer encryption scope exceeds codec limit")
	}
	return scope, nil
}
