package sha256sum

import (
	"crypto/sha256"
	"testing"
)

func TestDigestBytes(t *testing.T) {
	if got, want := DigestBytes([]byte("hello")), "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"; got != want {
		t.Fatalf("DigestBytes() = %q, want %q", got, want)
	}
}

func TestHexHash(t *testing.T) {
	hash := sha256.New()
	_, _ = hash.Write([]byte("hello"))
	if got, want := HexHash(hash), "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"; got != want {
		t.Fatalf("HexHash() = %q, want %q", got, want)
	}
}
