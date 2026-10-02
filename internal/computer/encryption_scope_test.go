package computer

import (
	"bytes"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

func TestStorageCiphertextCannotCrossEnvironment(t *testing.T) {
	first, err := encryptionScope("org", "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := encryptionScope("org", "second")
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{7}, computerkey.Size)
	id := uuid.NewV7().String()
	provider, err := computerkey.NewLocal("local", bytes.Repeat([]byte{8}, computerkey.Size))
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := provider.Wrap(t.Context(), first, id, key)
	if err != nil {
		t.Fatal(err)
	}
	if released, err := provider.Unwrap(t.Context(), second, id, envelope); err == nil || len(released) != 0 {
		clear(released)
		t.Fatal("wrapped key crossed environments")
	}
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writer := blockformat.Writer{Source: store, Sink: store, Scope: first, ActiveKey: id, Keys: map[string][]byte{id: key}, PackLimit: blockformat.MinPackLimit}
	root, err := writer.Empty(t.Context(), 32<<30, 64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = blockformat.InspectPack(t.Context(), store, first, writer.Keys, root.Pack); err != nil {
		t.Fatal(err)
	}
	if _, err = blockformat.InspectPack(t.Context(), store, second, writer.Keys, root.Pack); err == nil {
		t.Fatal("disk ciphertext crossed environments with identical raw key bytes")
	}
}

func TestEncryptionScope(t *testing.T) {
	seen := map[string]bool{}
	for _, ids := range [][2]string{
		{"org", "env"}, {"other", "env"},
		{"org", "other"},
		{"org/env", "a"}, {"org", "env/a"},
		{`org","env`, "a"},
	} {
		scope, err := encryptionScope(ids[0], ids[1])
		if err != nil {
			t.Fatal(err)
		}
		if seen[scope] {
			t.Fatalf("identity collision: %q", ids)
		}
		seen[scope] = true
		again, err := encryptionScope(ids[0], ids[1])
		if err != nil || again != scope {
			t.Fatal("scope is not canonical")
		}
	}
	for _, ids := range [][2]string{{"", "e"}, {"o", ""}, {"o", string([]byte{255})}, {"o", strings.Repeat("x", 256)}} {
		if _, err := encryptionScope(ids[0], ids[1]); err == nil {
			t.Fatalf("invalid identity accepted: %q", ids)
		}
	}
	// Production UUID identities fit without changing the qualified codec framing.
	id := "01997f91-0564-7000-a000-000000000001"
	if _, err := encryptionScope(id, id); err != nil {
		t.Fatal(err)
	}
}
