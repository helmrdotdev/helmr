//go:build linux || darwin

package computer

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
)

func TestSharedSeedDisksWriteIndependently(t *testing.T) {
	first, input := newVersionFixture(t)
	second := first.sibling(t)
	if _, err := first.publisher.PublishInitialVersion(t.Context(), first.principal, first.ref, input); err != nil {
		t.Fatal(err)
	}
	if ready, err := second.broker.PrepareSeed(t.Context(), second.principal, second.ref); err != nil || ready.Status != "ready" {
		t.Fatalf("seed adoption: %s %v", ready.Status, err)
	}
	type working struct {
		disk   *disk.LocalVersion
		source SourceMaterial
		root   disk.VersionRoot
	}
	var computers []working
	for index, f := range []preparationFixture{first, second} {
		source, err := f.broker.SourceKeys(t.Context(), f.principal, f.ref)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(source.Clear)
		keys := map[string][]byte{}
		for _, key := range source.Keys {
			keys[key.ID] = key.Key
		}
		local, err := disk.CreateLocalVersion(t.Context(), disk.LocalVersionConfig{Directory: filepath.Join(t.TempDir(), "working"), Base: source.Root, BaseSource: first.store, Scope: source.Scope, ActiveKey: source.WriteKeyID, Keys: keys, DirtyBlocks: 8, StagedBytes: 32 << 20, PackLimit: blockformat.MinPackLimit})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := local.Close(); err != nil {
				t.Error(err)
			}
		})
		data := bytes.Repeat([]byte{byte(index + 1)}, 4096)
		if _, err := local.WriteAt(t.Context(), data, 8192); err != nil {
			t.Fatal(err)
		}
		root, err := local.Flush(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if root.Page.KeyID != source.WriteKeyID || root == input.Root {
			t.Fatal("local change did not use the Computer's private writer")
		}
		computers = append(computers, working{disk: local, source: source, root: root})
	}
	if computers[0].root == computers[1].root || computers[0].source.WriteKeyID == computers[1].source.WriteKeyID {
		t.Fatal("independent Computer writes shared mutable authority")
	}
	for index, c := range computers {
		got := make([]byte, 4096)
		if _, err := c.disk.ReadAt(t.Context(), got, 8192); err != nil || !bytes.Equal(got, bytes.Repeat([]byte{byte(index + 1)}, 4096)) {
			t.Fatalf("sibling writes affected disk %d: %v", index, err)
		}
		keys := map[string][]byte{}
		for _, key := range c.source.Keys {
			keys[key.ID] = key.Key
		}
		original, err := disk.OpenVersion(t.Context(), first.store, c.source.Scope, keys, input.Root, input.Root.LogicalBytes)
		if err != nil {
			t.Fatal(err)
		}
		block, err := original.ReadBlock(t.Context(), 2)
		if err != nil || !bytes.Equal(block, make([]byte, 4096)) {
			t.Fatalf("private writes changed shared seed: %v", err)
		}
	}
}
