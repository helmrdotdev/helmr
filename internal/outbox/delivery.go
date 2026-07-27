package outbox

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
)

const maxErrorBytes = 2048

func RetryAfter(attempt int32) time.Duration {
	if attempt <= 1 {
		return time.Second
	}
	if attempt >= 7 {
		return time.Minute
	}
	return time.Duration(1<<uint(attempt-1)) * time.Second
}

func Error(cause error, fallback string) pgtype.Text {
	message := fallback
	if cause != nil {
		message = cause.Error()
	}
	message = normalizeError(message)
	if message == "" {
		message = normalizeError(fallback)
	}
	if message == "" {
		message = "Delivery failed"
	}
	return pgtype.Text{String: message, Valid: true}
}

func normalizeError(message string) string {
	message = strings.ReplaceAll(strings.ToValidUTF8(message, ""), "\x00", "")
	if strings.TrimSpace(message) == "" {
		return ""
	}
	if len(message) <= maxErrorBytes {
		return message
	}
	message = message[:maxErrorBytes]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}
	if strings.TrimSpace(message) == "" {
		return ""
	}
	return message
}
