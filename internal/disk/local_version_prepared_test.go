//go:build linux || darwin

package disk

import (
	"bytes"
	"maps"
	"testing"
	"uuid"
)

func TestPreparedDiskAdoptsOnlyOnceBeforeAnyWrite(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	imageKeys := cfg.Keys
	ephemeral := uuid.NewV7().String()
	cfg.Keys = map[string][]byte{ephemeral: bytes.Repeat([]byte{8}, 32)}
	maps.Copy(cfg.Keys, imageKeys)
	cfg.ActiveKey = ephemeral
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	oldKey := p.disk.writer.Keys[ephemeral]
	actual := uuid.NewV7().String()
	keys := map[string][]byte{actual: bytes.Repeat([]byte{9}, 32)}
	maps.Copy(keys, imageKeys)
	if err := p.AdoptPrepared(t.Context(), cfg.Base, actual, keys, cfg.BaseSource); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(oldKey, make([]byte, 32)) || p.disk.writer.Keys[ephemeral] != nil {
		t.Fatal("preparation write key survived adoption")
	}
	if localByte(t, p) != 1 {
		t.Fatal("adoption changed source bytes")
	}
	if err := p.AdoptPrepared(t.Context(), cfg.Base, actual, keys, cfg.BaseSource); err == nil {
		t.Fatal("second adoption accepted")
	}
	if _, err := p.WriteAt(t.Context(), []byte{3}, 7); err != nil {
		t.Fatal(err)
	}
	root, err := p.Flush(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if root == cfg.Base || localByte(t, p) != 3 {
		t.Fatal("granted writer did not persist private data")
	}
	if p.disk.writer.ActiveKey != actual {
		t.Fatal("private writes used a preparation key")
	}
}

func TestPreparedDiskRejectsChangedOrWrongSource(t *testing.T) {
	for _, scenario := range []string{"written", "flushed", "wrong-root", "missing-image-key"} {
		t.Run(scenario, func(t *testing.T) {
			cfg, _ := localVersionFixture(t)
			p, err := CreateLocalVersion(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			root, keys := cfg.Base, cfg.Keys
			switch scenario {
			case "written":
				_, err = p.WriteAt(t.Context(), []byte{4}, 7)
			case "flushed":
				_, err = p.Flush(t.Context())
			case "wrong-root":
				root.LogicalBytes++
			case "missing-image-key":
				keys = map[string][]byte{uuid.NewV7().String(): bytes.Repeat([]byte{3}, 32)}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := p.AdoptPrepared(t.Context(), root, cfg.ActiveKey, keys, cfg.BaseSource); err == nil {
				t.Fatal("unsafe disk adoption accepted")
			}
		})
	}
}
