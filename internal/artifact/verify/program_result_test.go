package verify

import (
	"reflect"
	"testing"
)

func TestProgramVerificationRoundTrip(t *testing.T) {
	canonical := canonicalVerifierProgramVerification(t)
	verified, err := parseProgramVerification(canonical)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := canonicalProgramVerification(verified)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reencoded, canonical) {
		t.Fatalf("reencoded verification = %q, want %q", reencoded, canonical)
	}
}
