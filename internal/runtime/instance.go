package runtime

import (
	"fmt"

	"github.com/helmrdotdev/helmr/internal/token"
)

func NewInstanceToken() (string, error) {
	raw, err := token.GenerateOpaque(32)
	if err != nil {
		return "", fmt.Errorf("generate runtime instance token: %w", err)
	}
	return raw, nil
}
