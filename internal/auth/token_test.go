package auth

import (
	"bytes"
	"testing"
)

func TestHashTokenRequiresStrongSecret(t *testing.T) {
	if _, err := HashToken([]byte("short"), "token"); err == nil {
		t.Fatal("expected weak secret error")
	}
}

func TestHashTokenIsStableAndKeyed(t *testing.T) {
	secret := []byte("01234567890123456789012345678901")
	first, err := HashToken(secret, "token")
	if err != nil {
		t.Fatal(err)
	}
	second, err := HashToken(secret, "token")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("hash is not stable")
	}
	other, err := HashToken([]byte("abcdefghijabcdefghijabcdefghij12"), "token")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, other) {
		t.Fatal("hash did not depend on secret")
	}
}

func TestNormalizeUserCode(t *testing.T) {
	if got := NormalizeUserCode(" abcd efgh "); got != "ABCD-EFGH" {
		t.Fatalf("code = %q", got)
	}
	if got := NormalizeUserCode("ABCD-EFGH"); got != "ABCD-EFGH" {
		t.Fatalf("code = %q", got)
	}
}
