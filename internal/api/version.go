package api

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	CurrentAPIVersion = "2026-06-06"
	APIVersionHeader  = "Helmr-API-Version"

	ClientVersionHeader   = "Helmr-Client-Version"
	SDKVersionHeader      = "Helmr-SDK-Version"
	CLIVersionHeader      = "Helmr-CLI-Version"
	MaxClientVersionBytes = 255

	CurrentWorkerProtocolVersion = "helmr.worker.v0"
)

func ValidateClientVersion(value string) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("version must be valid UTF-8")
	}
	if len(value) > MaxClientVersionBytes {
		return fmt.Errorf("version must be at most %d bytes", MaxClientVersionBytes)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("version must not contain surrounding whitespace")
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("version must not contain control characters")
		}
	}
	return nil
}
