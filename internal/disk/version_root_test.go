package disk

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestVersionRootFraming(t *testing.T) {
	valid := VersionRoot{FormatVersion: 1, LogicalBytes: 4096, Offset: 128,
		Pack: VersionPack{Digest: sha256sum.DigestBytes([]byte("pack")), SizeBytes: 512, Rank: 2},
		Page: VersionPage{Digest: sha256sum.DigestBytes([]byte("page")), Salt: strings.Repeat("ab", 32), KeyID: uuid.NewV7().String(), Kind: 3, Count: 1, SizeBytes: 64}}
	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseVersionRoot(raw, 4096)
	if err != nil || got != valid {
		t.Fatalf("root roundtrip: %v", err)
	}
	locator, err := valid.Locator(4096)
	if err != nil {
		t.Fatal(err)
	}
	if restored, err := NewVersionRoot(locator, 4096); err != nil || restored != valid {
		t.Fatalf("descriptor conversion: %v", err)
	}
	badLocator := locator
	badLocator.Page.Kind = 255
	if r, err := NewVersionRoot(badLocator, 4096); err == nil || r != (VersionRoot{}) {
		t.Fatal("invalid codec root returned usable descriptor")
	}
	cases := map[string]func(*VersionRoot){
		"version":       func(r *VersionRoot) { r.FormatVersion = 0 },
		"capacity":      func(r *VersionRoot) { r.LogicalBytes = 8192 },
		"pack digest":   func(r *VersionRoot) { r.Pack.Digest = strings.ToUpper(r.Pack.Digest) },
		"oversize":      func(r *VersionRoot) { r.Pack.SizeBytes = 4<<20 + 1 },
		"rank":          func(r *VersionRoot) { r.Pack.Rank = 7 },
		"page kind":     func(r *VersionRoot) { r.Page.Kind = 2 },
		"count":         func(r *VersionRoot) { r.Page.Count = 2 },
		"salt":          func(r *VersionRoot) { r.Page.Salt = "ab" },
		"key":           func(r *VersionRoot) { r.Page.KeyID = "caller-key" },
		"overflow":      func(r *VersionRoot) { r.Offset = math.MaxInt64 },
		"past pack":     func(r *VersionRoot) { r.Offset = r.Pack.SizeBytes - r.Page.SizeBytes + 1 },
		"negative size": func(r *VersionRoot) { r.Page.SizeBytes = -1 },
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
		if r, err := ParseVersionRoot(bad, 4096); err == nil || r != (VersionRoot{}) {
			t.Fatal("malformed root returned usable data")
		}
	}
}
