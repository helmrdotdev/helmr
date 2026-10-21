//go:build linux

package localcopy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/objectcache"
)

func TestBorrowedProgramRetainsOwnershipAndSurvivesClose(t *testing.T) {
	body := []byte("shared immutable program")
	d := testProgramDescriptor(artifact.RoleProgram, body)
	cache, err := objectcache.Open(filepath.Join(t.TempDir(), "cache"), 16384)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	directory := t.TempDir()
	if err := cache.EnableDrives(12345, os.Getgid(), directory); err != nil {
		t.Fatal(err)
	}
	if ok, err := cache.Seed(t.Context(), "program", cas.Descriptor(d), bytes.NewReader(body)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	b := cache.Borrow(t.Context(), "program", cas.Descriptor(d))
	local, err := BorrowProgram(t.Context(), b, d)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	before := local.content.platform.identity
	for _, owner := range []Owner{{0, os.Getgid()}, {os.Geteuid(), os.Getgid()}, {12345, os.Getgid() + 1}} {
		if err := local.LinkInto(directory, "bad", owner.UID, owner.GID); err == nil {
			t.Fatal("unsafe owner accepted", owner)
		}
	}
	uid := 12345
	if uid == os.Geteuid() {
		uid++
	}
	if err := local.LinkInto(directory, "first", uid, os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if err := local.LinkInto(directory, "second", uid, os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if err := local.LinkInto(directory, "second", uid, os.Getgid()); err == nil {
		t.Fatal("replaced destination")
	}
	if local.content.platform.identity != before {
		t.Fatal("shared owner changed")
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := os.Stat(filepath.Join(directory, "first"))
	if err != nil {
		t.Fatal(err)
	}
	z, err := os.Stat(filepath.Join(directory, "second"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, z) || a.Mode().Perm() != 0440 {
		t.Fatal("links are not same sealed inode")
	}
	again, err := BorrowProgram(t.Context(), cache.Borrow(t.Context(), "program", cas.Descriptor(d)), d)
	if err != nil {
		t.Fatal("close damaged cache", err)
	}
	again.Close()
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(directory, "first"))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatal("cache cleanup damaged link", err)
	}
}

func TestBorrowedProgramRejectsChangedBytesAndMode(t *testing.T) {
	for _, mode := range []string{"digest", "mode", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			body := []byte("good")
			d := testProgramDescriptor(artifact.RoleProgram, body)
			cache, err := objectcache.Open(filepath.Join(t.TempDir(), "cache"), 16384)
			if err != nil {
				t.Fatal(err)
			}
			defer cache.Close()
			if err := cache.EnableDrives(12345, os.Getgid(), t.TempDir()); err != nil {
				t.Fatal(err)
			}
			if ok, err := cache.Seed(t.Context(), "program", cas.Descriptor(d), bytes.NewReader(body)); !ok || err != nil {
				t.Fatal(ok, err)
			}
			b := cache.Borrow(t.Context(), "program", cas.Descriptor(d))
			dir, name, _, err := b.Open()
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir.Name(), name)
			dir.Close()
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "digest" {
				if err := os.WriteFile(path, []byte("evil"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0440); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "symlink" {
				if err := os.Rename(path, path+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+"-old", path); err != nil {
					t.Fatal(err)
				}
			}
			local, err := BorrowProgram(t.Context(), b, d)
			if err == nil {
				local.Close()
				t.Fatal("accepted changed cache")
			}
			if cache.Borrow(t.Context(), "program", cas.Descriptor(d)) != nil {
				t.Fatal("bad cache still borrowed")
			}
		})
	}
}

func BenchmarkProgramBorrowAndLink(b *testing.B) {
	body := make([]byte, 349581312)
	for i := range body {
		body[i] = byte(i*31 + i/4096)
	}
	d := testProgramDescriptor(artifact.RoleProgram, body)
	cache, err := objectcache.Open(filepath.Join(b.TempDir(), "cache"), int64(len(body))*8)
	if err != nil {
		b.Fatal(err)
	}
	defer cache.Close()
	directory := b.TempDir()
	if err := cache.EnableDrives(12345, os.Getgid(), directory); err != nil {
		b.Fatal(err)
	}
	if ok, err := cache.Seed(b.Context(), "program", cas.Descriptor(d), bytes.NewReader(body)); !ok || err != nil {
		b.Fatal(ok, err)
	}
	uid := 12345
	if uid == os.Getuid() {
		uid++
	}
	var borrowTime, linkTime time.Duration
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for range b.N {
		start := time.Now()
		local, err := BorrowProgram(b.Context(), cache.Borrow(b.Context(), "program", cas.Descriptor(d)), d)
		if err != nil {
			b.Fatal(err)
		}
		borrowTime += time.Since(start)
		start = time.Now()
		if err := local.LinkInto(directory, "program", uid, os.Getgid()); err != nil {
			b.Fatal(err)
		}
		linkTime += time.Since(start)
		if err := os.Remove(filepath.Join(directory, "program")); err != nil {
			b.Fatal(err)
		}
		if err := local.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(borrowTime.Seconds()/float64(b.N), "borrow-s/op")
	b.ReportMetric(linkTime.Seconds()/float64(b.N), "link-s/op")
}

func TestBorrowedProgramJailerCannotModifySharedInode(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires privilege to become jailer user")
	}
	body := []byte("jailer-readable immutable program")
	d := testProgramDescriptor(artifact.RoleProgram, body)
	cache, err := objectcache.Open(filepath.Join(t.TempDir(), "cache"), 16384)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	directory, err := os.MkdirTemp("", "helmr-shared-permissions-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	if err := os.Chmod(directory, 0755); err != nil {
		t.Fatal(err)
	}
	const gid = 12345
	if err := cache.EnableDrives(12345, gid, directory); err != nil {
		t.Fatal(err)
	}
	if ok, err := cache.Seed(t.Context(), "program", cas.Descriptor(d), bytes.NewReader(body)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	local, err := BorrowProgram(t.Context(), cache.Borrow(t.Context(), "program", cas.Descriptor(d)), d)
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if err := local.LinkInto(directory, "program", 12345, gid); err != nil {
		t.Fatal(err)
	}
	// Enter through the directory descriptor, as a jail enters its root before
	// dropping privilege. Test-runner temporary parents may be private to root.
	dir, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	command := exec.Command("/bin/sh", "-c", `cat "$1" && ! chmod 0600 "$1" 2>/dev/null && ! (printf x >> "$1") 2>/dev/null`, "sh", "/proc/self/fd/3/program")
	command.ExtraFiles = []*os.File{dir}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 12345, Gid: gid}}
	output, err := command.CombinedOutput()
	if err != nil || !bytes.Equal(output, body) {
		t.Fatalf("jailer permissions: %q %v", output, err)
	}
}

func TestBorrowCancellationReleasesPinWithoutDiscard(t *testing.T) {
	body := []byte("program")
	d := testProgramDescriptor(artifact.RoleProgram, body)
	cache, err := objectcache.Open(filepath.Join(t.TempDir(), "cache"), 16384)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if err := cache.EnableDrives(12345, os.Getgid(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if ok, err := cache.Seed(t.Context(), "program", cas.Descriptor(d), bytes.NewReader(body)); !ok || err != nil {
		t.Fatal(ok, err)
	}
	lease := cache.Borrow(t.Context(), "program", cas.Descriptor(d))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if local, err := BorrowProgram(ctx, lease, d); err == nil {
		local.Close()
		t.Fatal("cancellation ignored")
	}
	again := cache.Borrow(t.Context(), "program", cas.Descriptor(d))
	if again == nil {
		t.Fatal("cancellation discarded valid entry")
	}
	again.Close()
}

type unavailableBorrow struct{ closed, discarded int }

func (b *unavailableBorrow) Open() (*os.File, string, int, error) {
	return nil, "", 0, errors.New("descriptor unavailable")
}
func (b *unavailableBorrow) Discard()     { b.discarded++ }
func (b *unavailableBorrow) Close() error { b.closed++; return nil }
func TestBorrowOpenFailureReleasesWithoutDiscard(t *testing.T) {
	source := &unavailableBorrow{}
	descriptor := testProgramDescriptor(artifact.RoleProgram, []byte("program"))
	if local, err := BorrowProgram(t.Context(), source, descriptor); err == nil {
		local.Close()
		t.Fatal("Open failure ignored")
	}
	if source.closed != 1 || source.discarded != 0 {
		t.Fatalf("resource failure changed entry: %+v", source)
	}
}
