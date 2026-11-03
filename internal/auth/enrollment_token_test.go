package auth

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
	parsed, err := ParseEnrollmentToken(token.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(parsed.Hash, token.Hash) || len(parsed.Hash) != 32 || parsed.SingleUse {
		t.Fatalf("parsed = %+v", parsed)
	}
	single, err := GenerateSingleUseEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = ParseEnrollmentToken(single.Raw)
	if err != nil || !strings.HasPrefix(single.Raw, SingleUseEnrollmentTokenPrefix) || !bytes.Equal(parsed.Hash, single.Hash) || !parsed.SingleUse {
		t.Fatalf("single-use token %q parsed = %+v, %v", single.Raw, parsed, err)
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
