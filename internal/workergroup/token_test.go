package workergroup

import (
	"bytes"
	"strings"
	"testing"
)

func TestEnrollmentTokenRoundTrip(t *testing.T) {
	token, err := GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token.Raw, EnrollmentTokenPrefix) {
		t.Fatalf("token = %q", token.Raw)
	}
	hash, err := ParseEnrollmentToken(token.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hash, token.Hash) || len(hash) != 32 {
		t.Fatalf("hash length = %d", len(hash))
	}
}

func TestEnrollmentTokenParserRequiresCanonicalToken(t *testing.T) {
	valid, err := GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"", " " + valid.Raw, valid.Raw + "\n", strings.TrimPrefix(valid.Raw, EnrollmentTokenPrefix), valid.Raw + "="} {
		t.Run(raw, func(t *testing.T) {
			if _, err := ParseEnrollmentToken(raw); err == nil {
				t.Fatal("invalid token accepted")
			}
		})
	}
}
