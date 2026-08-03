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

func TestValidDigest(t *testing.T) {
	valid := "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	for _, test := range []struct {
		value string
		want  bool
	}{
		{value: valid, want: true},
		{value: ""},
		{value: valid[:len(valid)-1]},
		{value: "sha256:2CF24DBA5FB0A30E26E83B2AC5B9E29E1B161E5C1FA7425E73043362938B9824"},
		{value: "sha256:" + valid[len("sha256:")+1:] + "g"},
	} {
		if got := ValidDigest(test.value); got != test.want {
			t.Fatalf("ValidDigest(%q) = %t, want %t", test.value, got, test.want)
		}
	}
}
