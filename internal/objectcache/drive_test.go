package objectcache

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestDriveLeasePinsDiscardsAndReseeds(t *testing.T) {
	c, err := Open(filepath.Join(t.TempDir(), "cache"), 16384)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	jail := t.TempDir()
	if err := c.EnableDrives(12345, os.Getgid(), jail); err != nil {
		t.Fatal(err)
	}
	body := []byte("sealed program")
	d := cas.Descriptor{Digest: sha256sum.DigestBytes(body), SizeBytes: int64(len(body)), MediaType: "application/octet-stream"}
	if ok, err := c.Seed(t.Context(), "program", d, bytes.NewReader(body)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	b := c.Borrow(t.Context(), "program", d)
	if b == nil {
		t.Fatal("no borrow")
	}
	dir, name, _, err := b.Open()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir.Name(), name)
	dir.Close()
	original, err := os.Stat(path)
	if err != nil || original.Mode().Perm() != 0440 {
		t.Fatal(original, err)
	}
	link := filepath.Join(jail, "program")
	if err := os.Link(path, link); err != nil {
		t.Fatal(err)
	}
	b.Discard()
	if c.Borrow(t.Context(), "program", d) != nil {
		t.Fatal("discard still hits")
	}
	if _, ok := c.descriptor("program", d.Digest); ok {
		t.Fatal("discard still visible to reader")
	}
	if ok, err := c.Seed(t.Context(), "program", d, bytes.NewReader(body)); ok || err != nil {
		t.Fatal("reseed replaced pinned entry", ok, err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("release removed cache name", err)
	}
	if ok, err := c.Seed(t.Context(), "program", d, bytes.NewReader(body)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid entry not evicted", err)
	}
	fresh := c.Borrow(t.Context(), "program", d)
	if fresh == nil {
		t.Fatal("no fresh borrow")
	}
	fdir, fname, _, err := fresh.Open()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(fdir.Name(), fname))
	fdir.Close()
	fresh.Close()
	if err != nil || os.SameFile(original, info) {
		t.Fatal("reseed retained inode", err)
	}
	got, err := os.ReadFile(link)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("eviction damaged surviving VM link", err)
	}
	if c.Snapshot().UsedBytes != 4096 {
		t.Fatal(c.Snapshot())
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 20 {
				if b := c.Borrow(t.Context(), "program", d); b != nil {
					b.Close()
				}
			}
		})
	}
	wg.Wait()
}

func TestDriveProbeFailureKeepsPrivateSeeds(t *testing.T) {
	c, err := Open(filepath.Join(t.TempDir(), "cache"), 16384)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	notDirectory := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDirectory, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.EnableDrives(12345, os.Getgid(), notDirectory); err == nil {
		t.Fatal("probe unexpectedly succeeded")
	}
	body := []byte("private")
	d := cas.Descriptor{Digest: sha256sum.DigestBytes(body), SizeBytes: int64(len(body)), MediaType: "application/octet-stream"}
	if ok, err := c.Seed(t.Context(), "program", d, bytes.NewReader(body)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if c.Borrow(t.Context(), "program", d) != nil {
		t.Fatal("unqualified drive borrowed")
	}
	e := c.entries[key{"program", d.Digest}]
	info, err := os.Stat(e.path)
	if err != nil || info.Mode().Perm() != 0400 {
		t.Fatal(info, err)
	}
}

func TestDriveProbeRemovesCrashResidueBeforeRecovery(t *testing.T) {
	for _, uid := range []int{0, 12345} {
		t.Run(fmt.Sprint(uid), func(t *testing.T) {
			c, err := Open(filepath.Join(t.TempDir(), "cache"), 16384)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			state, jail := t.TempDir(), t.TempDir()
			for _, d := range []string{state, jail} {
				if err := os.WriteFile(filepath.Join(d, driveProbeName), nil, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(d, "unrelated"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err = c.EnableDrives(uid, os.Getgid(), state, jail)
			if (uid == 0) != (err != nil) {
				t.Fatal("unexpected probe admission", err)
			}
			for _, d := range []string{state, jail} {
				if _, err := os.Lstat(filepath.Join(d, driveProbeName)); !os.IsNotExist(err) {
					t.Fatal("probe residue survives", err)
				}
				if _, err := os.Stat(filepath.Join(d, "unrelated")); err != nil {
					t.Fatal("unrelated file removed", err)
				}
			}
		})
	}
}
