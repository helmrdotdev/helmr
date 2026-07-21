package workspace

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestFencingKeysDeriveExactCapability(t *testing.T) {
	key := make([]byte, FencingKeySize)
	for index := range key {
		key[index] = byte(index)
	}
	const fingerprint = "sha256:5caaaabfb2d6290166b848f64cba58d352bc67c6007c0656a2395efad5c7eff5"
	keys, err := NewFencingKeys(fingerprint, map[string][]byte{fingerprint: key})
	if err != nil {
		t.Fatal(err)
	}
	input := FenceInput{
		LeaseID:                uuid.MustParse("00112233-4455-6677-8899-aabbccddeeff"),
		WorkspaceID:            uuid.MustParse("ffeeddcc-bbaa-9988-7766-554433221100"),
		OwnershipGeneration:    7,
		WriterGeneration:       11,
		MountFencingGeneration: 13,
	}
	capability, err := keys.DeriveActive(input)
	if err != nil {
		t.Fatal(err)
	}
	if capability.KeyFingerprint.String() != fingerprint ||
		capability.Token != "PVPhpFfbVfNqF-ACb681yKthwdUcPeXlUq-dnSq6EM4" ||
		capability.Hash != "sha256:6afbab08c7bdefa85ff618f8793eece21d2968f2cd7aa825020b979d567a354f" {
		t.Fatalf("capability = %+v", capability)
	}
	replayed, err := keys.Derive(capability.KeyFingerprint, input)
	if err != nil {
		t.Fatal(err)
	}
	if replayed != capability {
		t.Fatalf("replayed capability = %+v, want %+v", replayed, capability)
	}
}

func TestFencingKeysFromBase64JSONSelectsActiveReadableKey(t *testing.T) {
	first := base64.StdEncoding.EncodeToString(bytesOf(1))
	second := base64.StdEncoding.EncodeToString(bytesOf(2))
	keys, err := FencingKeysFromBase64JSON(
		fingerprintOf(2),
		`{"`+fingerprintOf(1)+`":"`+first+`","`+fingerprintOf(2)+`":"`+second+`"}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	firstFingerprint, _ := ParseFencingKeyFingerprint(fingerprintOf(1))
	secondFingerprint, _ := ParseFencingKeyFingerprint(fingerprintOf(2))
	if keys.ActiveFingerprint() != secondFingerprint ||
		!keys.Has(firstFingerprint) ||
		!keys.Has(secondFingerprint) {
		t.Fatalf(
			"keys active=%q old=%t current=%t",
			keys.ActiveFingerprint(),
			keys.Has(firstFingerprint),
			keys.Has(secondFingerprint),
		)
	}
}

func TestFencingKeysRejectInvalidAuthority(t *testing.T) {
	validKey := base64.StdEncoding.EncodeToString(bytesOf(1))
	validFingerprint := fingerprintOf(1)
	for _, test := range []struct {
		name   string
		active string
		raw    string
		want   string
	}{
		{name: "missing active", active: fingerprintOf(2), raw: `{"` + validFingerprint + `":"` + validKey + `"}`, want: "is not configured"},
		{name: "invalid fingerprint", active: "release/1", raw: `{"release/1":"` + validKey + `"}`, want: "fingerprint must"},
		{name: "non-canonical fingerprint", active: "SHA256:" + strings.Repeat("0", 64), raw: `{"` + validFingerprint + `":"` + validKey + `"}`, want: "fingerprint must"},
		{name: "mismatched fingerprint", active: fingerprintOf(2), raw: `{"` + fingerprintOf(2) + `":"` + validKey + `"}`, want: "does not match"},
		{name: "short key", active: validFingerprint, raw: `{"` + validFingerprint + `":"AQ=="}`, want: "must be 32 bytes"},
		{name: "duplicate fingerprint", active: validFingerprint, raw: `{"` + validFingerprint + `":"` + validKey + `","` + validFingerprint + `":"` + validKey + `"}`, want: "duplicate fingerprint"},
		{name: "trailing value", active: validFingerprint, raw: `{"` + validFingerprint + `":"` + validKey + `"} true`, want: "trailing value"},
		{name: "malformed trailing value", active: validFingerprint, raw: `{"` + validFingerprint + `":"` + validKey + `"} garbage`, want: "trailing value"},
		{name: "empty", active: validFingerprint, raw: `{}`, want: "at least one"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := FencingKeysFromBase64JSON(test.active, test.raw)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestFencingKeysRejectInvalidDerivationInput(t *testing.T) {
	keys, err := NewFencingKeys(
		fingerprintOf(1),
		map[string][]byte{fingerprintOf(1): bytesOf(1)},
	)
	if err != nil {
		t.Fatal(err)
	}
	valid := FenceInput{
		LeaseID:                uuid.New(),
		WorkspaceID:            uuid.New(),
		OwnershipGeneration:    1,
		WriterGeneration:       1,
		MountFencingGeneration: 1,
	}
	for _, test := range []struct {
		name        string
		fingerprint string
		edit        func(*FenceInput)
		want        string
	}{
		{name: "unknown key", fingerprint: fingerprintOf(2), edit: func(*FenceInput) {}, want: "is not configured"},
		{name: "missing lease", fingerprint: fingerprintOf(1), edit: func(input *FenceInput) { input.LeaseID = uuid.Nil }, want: "Lease ID"},
		{name: "missing Workspace", fingerprint: fingerprintOf(1), edit: func(input *FenceInput) { input.WorkspaceID = uuid.Nil }, want: "Workspace ID"},
		{name: "invalid generation", fingerprint: fingerprintOf(1), edit: func(input *FenceInput) { input.WriterGeneration = 0 }, want: "must be positive"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := valid
			test.edit(&input)
			fingerprint, parseErr := ParseFencingKeyFingerprint(test.fingerprint)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			_, err := keys.Derive(fingerprint, input)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func bytesOf(value byte) []byte {
	return []byte(strings.Repeat(string([]byte{value}), FencingKeySize))
}

func fingerprintOf(value byte) string {
	var key [FencingKeySize]byte
	copy(key[:], bytesOf(value))
	return FencingKeyFingerprintForKey(key).String()
}
