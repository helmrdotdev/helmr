package generation

import (
	"bytes"
	"encoding/json"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
)

func TestPersistedGenerationRoot(t *testing.T) {
	key1, key2 := uuid.NewV7().String(), uuid.NewV7().String()
	scope, err := computer.EncryptionScope(uuid.NewV7().String(), uuid.NewV7().String(), uuid.NewV7().String())
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string][]byte{key1: bytes.Repeat([]byte{1}, 32), key2: bytes.Repeat([]byte{2}, 32)}
	codec, err := NewCodec(scope, key1, keys)
	if err != nil {
		t.Fatal(err)
	}
	data, packs := NewStore(), NewStore()
	const capacity = 32 << 30
	root, err := NewPacked(codec, packs, capacity, 64, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	root, err = CapturePacked(codec, data, packs, root, map[uint64][]byte{0: bytes.Repeat([]byte{7}, blockformat.BlockSize)}, 1<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	first := root
	codec.ActiveKey = key2
	root, err = CapturePacked(codec, data, packs, root, map[uint64][]byte{8192: bytes.Repeat([]byte{9}, blockformat.BlockSize)}, 1<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, locator := range []blockformat.Locator{first, root} {
		inspected, err := Inspect(codec, data, packs, locator, 1000, 16<<20)
		if err != nil {
			t.Fatal(err)
		}
		persisted, err := computer.NewGenerationRoot(inspected.Root, inspected.Capacity)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(persisted)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := computer.ParseGenerationRoot(raw, capacity)
		if err != nil {
			t.Fatal(err)
		}
		restored, err := parsed.Locator(capacity)
		if err != nil || restored != locator {
			t.Fatalf("locator changed across persistence: %v", err)
		}
		got, err := ReadPacked(codec, data, packs, restored, 0)
		if err != nil || !bytes.Equal(got, bytes.Repeat([]byte{7}, blockformat.BlockSize)) {
			t.Fatalf("restored earlier data: %v", err)
		}
		got, err = ReadPacked(codec, data, packs, restored, 8192)
		want := make([]byte, blockformat.BlockSize)
		if locator == root {
			want = bytes.Repeat([]byte{9}, blockformat.BlockSize)
		}
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("restored generation-specific data: %v", err)
		}
		// Framing can be valid while the claimed page identity is false. The
		// authenticated reader must reject it instead of selecting another page.
		bad := restored
		bad.Page.Salt[0] ^= 1
		if _, err = ReadPacked(codec, data, packs, bad, 0); err == nil {
			t.Fatal("false page identity accepted")
		}
	}
}
