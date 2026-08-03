package config

import (
	"encoding/base64"
	"fmt"
)

const rootKeySize = 32

func rootKey(name string) ([]byte, error) {
	encoded := envString(name)
	if encoded == "" {
		return nil, fmt.Errorf("%s is required", name)
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%s must be base64: %w", name, err)
	}
	if len(decoded) != rootKeySize {
		return nil, fmt.Errorf(
			"%s must decode to exactly %d bytes, got %d",
			name,
			rootKeySize,
			len(decoded),
		)
	}
	if base64.StdEncoding.EncodeToString(decoded) != encoded {
		return nil, fmt.Errorf("%s must use canonical base64", name)
	}
	return decoded, nil
}
