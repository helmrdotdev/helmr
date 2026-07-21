package workspace

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestFencingKeysDeriveExactCapability(t *testing.T) {
	key := make([]byte, FencingKeySize)
	for index := range key {
		key[index] = byte(index)
	}
	keys, err := NewFencingKeys("write-7", map[string][]byte{"write-7": key})
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
	if capability.KeyID != "write-7" ||
		capability.Token != "PVPhpFfbVfNqF-ACb681yKthwdUcPeXlUq-dnSq6EM4" ||
		capability.Hash != "sha256:6afbab08c7bdefa85ff618f8793eece21d2968f2cd7aa825020b979d567a354f" {
		t.Fatalf("capability = %+v", capability)
	}
	replayed, err := keys.Derive("write-7", input)
	if err != nil {
		t.Fatal(err)
	}
	if replayed != capability {
		t.Fatalf("replayed capability = %+v, want %+v", replayed, capability)
	}
	fingerprint, err := keys.Fingerprint("write-7")
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(fingerprint[:]); got != "5caaaabfb2d6290166b848f64cba58d352bc67c6007c0656a2395efad5c7eff5" {
		t.Fatalf("fingerprint = %s", got)
	}
}

func TestFencingKeysFromBase64JSONSelectsActiveReadableKey(t *testing.T) {
	first := base64.StdEncoding.EncodeToString(bytesOf(1))
	second := base64.StdEncoding.EncodeToString(bytesOf(2))
	keys, err := FencingKeysFromBase64JSON(
		"release.2",
		`{"release.1":"`+first+`","release.2":"`+second+`"}`,
	)
	if err != nil {
		t.Fatal(err)
	}
	if keys.ActiveID() != "release.2" || !keys.Has("release.1") || !keys.Has("release.2") {
		t.Fatalf("keys active=%q old=%t current=%t", keys.ActiveID(), keys.Has("release.1"), keys.Has("release.2"))
	}
}

func TestFencingKeysRejectInvalidAuthority(t *testing.T) {
	validKey := base64.StdEncoding.EncodeToString(bytesOf(1))
	for _, test := range []struct {
		name   string
		active string
		raw    string
		want   string
	}{
		{name: "missing active", active: "release.2", raw: `{"release.1":"` + validKey + `"}`, want: "is not configured"},
		{name: "invalid id", active: "release/1", raw: `{"release/1":"` + validKey + `"}`, want: "ID is invalid"},
		{name: "non-canonical active id", active: " release.1 ", raw: `{"release.1":"` + validKey + `"}`, want: "ID is invalid"},
		{name: "short key", active: "release.1", raw: `{"release.1":"AQ=="}`, want: "must be 32 bytes"},
		{name: "duplicate id", active: "release.1", raw: `{"release.1":"` + validKey + `","release.1":"` + validKey + `"}`, want: "duplicate ID"},
		{name: "trailing value", active: "release.1", raw: `{"release.1":"` + validKey + `"} true`, want: "trailing value"},
		{name: "malformed trailing value", active: "release.1", raw: `{"release.1":"` + validKey + `"} garbage`, want: "trailing value"},
		{name: "empty", active: "release.1", raw: `{}`, want: "at least one"},
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
	keys, err := NewFencingKeys("release.1", map[string][]byte{"release.1": bytesOf(1)})
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
		name  string
		keyID string
		edit  func(*FenceInput)
		want  string
	}{
		{name: "unknown key", keyID: "release.2", edit: func(*FenceInput) {}, want: "is not configured"},
		{name: "missing lease", keyID: "release.1", edit: func(input *FenceInput) { input.LeaseID = uuid.Nil }, want: "Lease ID"},
		{name: "missing Workspace", keyID: "release.1", edit: func(input *FenceInput) { input.WorkspaceID = uuid.Nil }, want: "Workspace ID"},
		{name: "invalid generation", keyID: "release.1", edit: func(input *FenceInput) { input.WriterGeneration = 0 }, want: "must be positive"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := valid
			test.edit(&input)
			_, err := keys.Derive(test.keyID, input)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func bytesOf(value byte) []byte {
	return []byte(strings.Repeat(string([]byte{value}), FencingKeySize))
}
