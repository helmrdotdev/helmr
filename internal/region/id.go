package region

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const MaxIDBytes = 255

func ValidateID(value string) error {
	if value == "" {
		return errors.New("region ID is required")
	}
	if !utf8.ValidString(value) {
		return errors.New("region ID must be valid UTF-8")
	}
	if len(value) > MaxIDBytes {
		return fmt.Errorf("region ID must be at most %d bytes", MaxIDBytes)
	}
	if strings.TrimSpace(value) != value {
		return errors.New("region ID must not contain surrounding whitespace")
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return errors.New("region ID must not contain control characters")
		}
	}
	return nil
}
