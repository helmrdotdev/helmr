package generation

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func localFixture(t *testing.T) (*Codec, *Local, Locator, *Store, *Store) {
	t.Helper()
	c, data := fixture(t)
	packs := NewStore()
	root, e := NewPacked(c, packs, 64*BlockSize, 64, 64<<10)
	if e != nil {
		t.Fatal(e)
	}
	root, e = CapturePacked(c, data, packs, root, map[uint64][]byte{0: bytes.Repeat([]byte{1}, BlockSize)}, 64<<10, true)
	if e != nil {
		t.Fatal(e)
	}
	local, e := OpenLocal(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = local.Commit(c, data, packs, root, 100, 8<<20); e != nil {
		t.Fatal(e)
	}
	return c, local, root, data, packs
}
func assertLocal(t *testing.T, c *Codec, l *Local, want byte) {
	t.Helper()
	r, d, p, e := l.Reopen(c, 100, 8<<20)
	if e != nil {
		t.Fatal(e)
	}
	got, e := ReadPacked(c, d, p, r, 0)
	if e != nil || !bytes.Equal(got, bytes.Repeat([]byte{want}, BlockSize)) {
		t.Fatal("reopened value", e)
	}
}
func TestLocalCommitAndReopen(t *testing.T) {
	c, l, old, data, packs := localFixture(t)
	// Unreferenced malformed files and interrupted staging files are never scanned.
	for _, name := range []string{"objects/orphan", "root-pending-orphan"} {
		if e := os.WriteFile(filepath.Join(l.dir, name), []byte("invalid"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	assertLocal(t, c, l, 1)
	next, e := CapturePacked(c, data, packs, old, map[uint64][]byte{0: bytes.Repeat([]byte{2}, BlockSize)}, 64<<10, true)
	if e != nil {
		t.Fatal(e)
	}
	var order []string
	l.after = func(s string) error { order = append(order, s); return nil }
	if e = l.Commit(c, data, packs, next, 100, 8<<20); e != nil {
		t.Fatal(e)
	}
	last := order[len(order)-4:]
	want := []string{"objects-synced", "root-synced", "root-installed", "root-directory-synced"}
	for i := range want {
		if last[i] != want[i] {
			t.Fatal(order)
		}
	}
	reopened, e := OpenLocal(l.dir)
	if e != nil {
		t.Fatal(e)
	}
	assertLocal(t, c, reopened, 2)
	if _, e = os.Stat(l.objectPath(old.Pack.Digest)); e != nil {
		t.Fatal("old objects deleted", e)
	}
	// Existing immutable objects are revalidated and synced on repeated commit.
	if e = l.Commit(c, data, packs, next, 100, 8<<20); e != nil {
		t.Fatal(e)
	}
	if _, _, _, e = l.Reopen(c, 1, 8<<20); e == nil {
		t.Fatal("budget ignored")
	}
	path := l.objectPath(next.Pack.Digest)
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	raw[0] ^= 1
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if _, _, _, e = l.Reopen(c, 100, 8<<20); e == nil {
		t.Fatal("corrupt root pack accepted")
	}
	if e = l.Commit(c, data, packs, next, 100, 8<<20); e == nil {
		t.Fatal("existing corrupt object overwritten")
	}
}
func TestLocalFailureBoundaries(t *testing.T) {
	for _, stage := range []string{"object-synced", "object-installed", "objects-synced", "root-synced", "root-installed", "root-directory-synced"} {
		t.Run(stage, func(t *testing.T) {
			c, l, old, data, packs := localFixture(t)
			next, e := CapturePacked(c, data, packs, old, map[uint64][]byte{0: bytes.Repeat([]byte{2}, BlockSize)}, 64<<10, true)
			if e != nil {
				t.Fatal(e)
			}
			injected := errors.New("injected stop")
			l.after = func(s string) error {
				if s == stage {
					return injected
				}
				return nil
			}
			if e = l.Commit(c, data, packs, next, 100, 8<<20); !errors.Is(e, injected) {
				t.Fatal("boundary not reached", e)
			}
			expected := byte(1)
			if stage == "root-installed" || stage == "root-directory-synced" {
				expected = 2
			}
			assertLocal(t, c, l, expected)
			l.after = nil
			if e = l.Commit(c, data, packs, next, 100, 8<<20); e != nil {
				t.Fatal("retry failed", e)
			}
			assertLocal(t, c, l, 2)
		})
	}
}
func TestLocalProcessExit(t *testing.T) {
	for _, stage := range []string{"object-synced", "object-installed", "objects-synced", "root-synced", "root-installed", "root-directory-synced"} {
		t.Run(stage, func(t *testing.T) {
			c, l, _, _, _ := localFixture(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestLocalCrashChild$")
			cmd.Env = append(os.Environ(), "HELMR_LOCAL_PROOF_DIR="+l.dir, "HELMR_LOCAL_PROOF_STOP="+stage)
			out, e := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(e, &exit) || exit.ExitCode() != 98 {
				t.Fatalf("child did not exit at boundary: %v %s", e, out)
			}
			reopened, e := OpenLocal(l.dir)
			if e != nil {
				t.Fatal(e)
			}
			expected := byte(1)
			if stage == "root-installed" || stage == "root-directory-synced" {
				expected = 2
			}
			assertLocal(t, c, reopened, expected)
		})
	}
}
func TestLocalCrashChild(t *testing.T) {
	dir := os.Getenv("HELMR_LOCAL_PROOF_DIR")
	if dir == "" {
		t.Skip("subprocess only")
	}
	c, _ := fixture(t)
	l, e := OpenLocal(dir)
	if e != nil {
		t.Fatal(e)
	}
	r, d, p, e := l.Reopen(c, 100, 8<<20)
	if e != nil {
		t.Fatal(e)
	}
	next, e := CapturePacked(c, d, p, r, map[uint64][]byte{0: bytes.Repeat([]byte{2}, BlockSize)}, 64<<10, true)
	if e != nil {
		t.Fatal(e)
	}
	l.after = func(s string) error {
		if s == os.Getenv("HELMR_LOCAL_PROOF_STOP") {
			os.Exit(98)
		}
		return nil
	}
	if e = l.Commit(c, d, p, next, 100, 8<<20); e != nil {
		t.Fatal(e)
	}
	t.Fatal("stop boundary not reached")
}

func TestLocalCommittedCorruptionDoesNotFallback(t *testing.T) {
	for _, mode := range []string{"missing-object", "truncated-root", "oversized-root"} {
		t.Run(mode, func(t *testing.T) {
			c, l, root, _, _ := localFixture(t)
			var e error
			switch mode {
			case "missing-object":
				e = os.Remove(l.objectPath(root.Pack.Digest))
			case "truncated-root":
				e = os.WriteFile(filepath.Join(l.dir, "root"), []byte(`"incomplete`), 0600)
			case "oversized-root":
				e = os.WriteFile(filepath.Join(l.dir, "root"), bytes.Repeat([]byte{' '}, 347), 0600)
			}
			if e != nil {
				t.Fatal(e)
			}
			r, d, p, e := l.Reopen(c, 100, 8<<20)
			if e == nil || r != (Locator{}) || d != nil || p != nil {
				t.Fatal("damaged committed state returned as usable")
			}
		})
	}
}
