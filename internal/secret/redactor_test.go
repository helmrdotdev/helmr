package secret

import "testing"

func TestRedactorRedactsMultipleOccurrences(t *testing.T) {
	redactor := NewRedactor([]byte("secret-value"))
	got := redactor.RedactString("secret-value hello secret-value")
	if got != "*** hello ***" {
		t.Fatalf("redacted = %q", got)
	}
}

func TestRedactorRedactsShortPatterns(t *testing.T) {
	redactor := NewRedactor([]byte("abc"), []byte("12345678"))
	got := redactor.RedactString("abc 12345678")
	if got != "*** ***" {
		t.Fatalf("redacted = %q", got)
	}
}

func TestRedactorUsesLongestMatch(t *testing.T) {
	redactor := NewRedactor([]byte("secret-value"), []byte("secret-value-longer"))
	got := redactor.RedactString("secret-value-longer")
	if got != "***" {
		t.Fatalf("redacted = %q", got)
	}
}

func TestRedactorCopiesPatterns(t *testing.T) {
	pattern := []byte("secret-value")
	redactor := NewRedactor(pattern)
	copy(pattern, "public-value")
	got := redactor.RedactString("secret-value public-value")
	if got != "*** public-value" {
		t.Fatalf("redacted = %q", got)
	}
}
