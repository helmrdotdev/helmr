package api

import (
	"fmt"
	"regexp"
	"strings"
)

const MaxStreamNameBytes = 256

var streamNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$`)

func ValidateStreamName(value string) error {
	name := strings.TrimSpace(value)
	if !streamNamePattern.MatchString(name) {
		return fmt.Errorf("stream name must match %s", streamNamePattern.String())
	}
	if len(name) > MaxStreamNameBytes {
		return fmt.Errorf("stream name must be at most %d bytes", MaxStreamNameBytes)
	}
	return nil
}
