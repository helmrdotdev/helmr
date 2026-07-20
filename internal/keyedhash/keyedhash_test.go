package keyedhash

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestKeyringSelectsDatabaseCurrentVersion(t *testing.T) {
	ring, err := New(map[int32][]byte{
		1: bytes.Repeat([]byte{1}, KeySize),
		2: bytes.Repeat([]byte{2}, KeySize),
		3: bytes.Repeat([]byte{3}, KeySize),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := ring.Fingerprint(1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ring.Fingerprint(2)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := ring.Select([]Version{
		{Number: 2, Fingerprint: second[:], Current: true},
		{Number: 1, Fingerprint: first[:]},
	})
	if err != nil {
		t.Fatal(err)
	}
	if selection.Current != 2 {
		t.Fatalf("current version = %d, want 2", selection.Current)
	}
	if len(selection.Versions) != 2 || selection.Versions[0] != 1 || selection.Versions[1] != 2 {
		t.Fatalf("versions = %v", selection.Versions)
	}
	current, err := ring.Sum(selection.Current, []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	previous, err := ring.Sum(1, []byte("value"))
	if err != nil {
		t.Fatal(err)
	}
	if current.Value == previous.Value {
		t.Fatal("different keys produced the same digest")
	}
}

func TestKeyringRejectsAuthorityMismatch(t *testing.T) {
	ring, err := New(map[int32][]byte{
		1: bytes.Repeat([]byte{1}, KeySize),
		2: bytes.Repeat([]byte{2}, KeySize),
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := ring.Fingerprint(1)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		versions []Version
	}{
		{name: "empty"},
		{name: "missing key", versions: []Version{{Number: 3, Fingerprint: first[:], Current: true}}},
		{name: "wrong bytes", versions: []Version{{Number: 1, Fingerprint: bytes.Repeat([]byte{9}, 32), Current: true}}},
		{name: "no current", versions: []Version{{Number: 1, Fingerprint: first[:]}}},
		{name: "multiple current", versions: []Version{
			{Number: 1, Fingerprint: first[:], Current: true},
			{Number: 2, Fingerprint: mustFingerprint(t, ring, 2), Current: true},
		}},
		{name: "duplicate", versions: []Version{
			{Number: 1, Fingerprint: first[:], Current: true},
			{Number: 1, Fingerprint: first[:]},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ring.Select(test.versions); err == nil {
				t.Fatal("expected selection error")
			}
		})
	}
}

func TestKeyringParsesKeysOnlyJSON(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, KeySize))
	ring, err := FromBase64JSON(`{"3":"` + encoded + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !ring.Has(3) {
		t.Fatal("version 3 is not configured")
	}
	if _, err := FromBase64JSON(`{"current":3,"keys":{"3":"` + encoded + `"}}`); err == nil {
		t.Fatal("expected legacy registry shape to fail")
	}
	for _, raw := range []string{
		`{"1":"` + encoded + `","01":"` + encoded + `"}`,
		`{"1":"` + encoded + `","1":"` + encoded + `"}`,
		`{"+1":"` + encoded + `"}`,
	} {
		if _, err := FromBase64JSON(raw); err == nil {
			t.Fatalf("expected ambiguous version configuration %s to fail", raw)
		}
	}
}

func TestKeyringRejectsInvalidConfiguration(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("expected empty key set error")
	}
	if _, err := New(map[int32][]byte{0: bytes.Repeat([]byte{1}, KeySize)}); err == nil {
		t.Fatal("expected invalid version error")
	}
	if _, err := New(map[int32][]byte{1: []byte("short")}); err == nil {
		t.Fatal("expected invalid key length error")
	}
}

func mustFingerprint(t *testing.T, ring Keyring, version int32) []byte {
	t.Helper()
	value, err := ring.Fingerprint(version)
	if err != nil {
		t.Fatal(err)
	}
	return value[:]
}
