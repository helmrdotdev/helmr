package computer

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestGenerationRootFraming(t *testing.T) {
	valid := GenerationRoot{FormatVersion: 1, LogicalBytes: 4096, Offset: 128,
		Pack: GenerationPack{Digest: sha256sum.DigestBytes([]byte("pack")), SizeBytes: 512, Rank: 2},
		Page: GenerationPage{Digest: sha256sum.DigestBytes([]byte("page")), Salt: strings.Repeat("ab", 32), KeyID: uuid.NewV7().String(), Kind: 3, Count: 1, SizeBytes: 64}}
	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseGenerationRoot(raw, 4096)
	if err != nil || got != valid {
		t.Fatalf("root roundtrip: %v", err)
	}
	locator, err := valid.Locator(4096)
	if err != nil {
		t.Fatal(err)
	}
	if restored, err := NewGenerationRoot(locator, 4096); err != nil || restored != valid {
		t.Fatalf("descriptor conversion: %v", err)
	}
	badLocator := locator
	badLocator.Page.Kind = 255
	if r, err := NewGenerationRoot(badLocator, 4096); err == nil || r != (GenerationRoot{}) {
		t.Fatal("invalid codec root returned usable descriptor")
	}
	cases := map[string]func(*GenerationRoot){
		"version":       func(r *GenerationRoot) { r.FormatVersion = 0 },
		"capacity":      func(r *GenerationRoot) { r.LogicalBytes = 8192 },
		"pack digest":   func(r *GenerationRoot) { r.Pack.Digest = strings.ToUpper(r.Pack.Digest) },
		"oversize":      func(r *GenerationRoot) { r.Pack.SizeBytes = 4<<20 + 1 },
		"rank":          func(r *GenerationRoot) { r.Pack.Rank = 7 },
		"page kind":     func(r *GenerationRoot) { r.Page.Kind = 2 },
		"count":         func(r *GenerationRoot) { r.Page.Count = 2 },
		"salt":          func(r *GenerationRoot) { r.Page.Salt = "ab" },
		"key":           func(r *GenerationRoot) { r.Page.KeyID = "caller-key" },
		"overflow":      func(r *GenerationRoot) { r.Offset = math.MaxInt64 },
		"past pack":     func(r *GenerationRoot) { r.Offset = r.Pack.SizeBytes - r.Page.SizeBytes + 1 },
		"negative size": func(r *GenerationRoot) { r.Page.SizeBytes = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := valid
			mutate(&r)
			if err := r.Validate(4096); err == nil {
				t.Fatal("invalid locator accepted")
			}
			if l, err := r.Locator(4096); err == nil || l != (blockformat.Locator{}) {
				t.Fatal("invalid descriptor returned usable locator")
			}
		})
	}
	for _, bad := range [][]byte{append(append([]byte{}, raw...), raw...), []byte(strings.Repeat(" ", 2049)), []byte(strings.Replace(string(raw), "\"offset\":", "\"unknown\":0,\"offset\":", 1))} {
		if r, err := ParseGenerationRoot(bad, 4096); err == nil || r != (GenerationRoot{}) {
			t.Fatal("malformed root returned usable data")
		}
	}
}
