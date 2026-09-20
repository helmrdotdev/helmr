package substrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"golang.org/x/sys/unix"
)

func TestRuntimeSubstrateCacheSourceProjectsWithoutMutatingCacheAuthority(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
	}{
		{name: "small", body: []byte("immutable substrate bytes")},
		{name: "dense", body: bytes.Repeat([]byte{0x5a}, (96<<10)+13)},
		{name: "nonzero chunk tails", body: bytes.Repeat(append(make([]byte, (32<<10)-1), 1), 3)},
		{name: "all zero with partial tail", body: make([]byte, (96<<10)+13)},
		{name: "leading zeros", body: append(make([]byte, 64<<10), []byte("content")...)},
		{name: "trailing zeros", body: append([]byte("content"), make([]byte, (96<<10)+13)...)},
		{name: "mixed unaligned spans", body: bytes.Join([][]byte{
			[]byte("start"), make([]byte, (64<<10)+7), []byte("middle"),
			make([]byte, (64<<10)+19), []byte("end"),
		}, nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			cachePath := t.TempDir() + "/substrate.ext4"
			if err := os.WriteFile(cachePath, test.body, 0o444); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(cachePath)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(test.body)
			source := &DiskSource{
				path: cachePath, digest: sha256sum.FormatDigest(sum[:]), sizeBytes: int64(len(test.body)),
			}
			projected, err := source.MaterializeInto(
				context.Background(), t.TempDir(), "substrate.ext4", os.Getuid(), os.Getgid(),
			)
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(cachePath)
			if err != nil {
				t.Fatal(err)
			}
			if before.Mode() != after.Mode() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
				t.Fatalf("cache authority changed: before=%v after=%v", before, after)
			}
			if got, err := os.ReadFile(projected); err != nil || !bytes.Equal(got, test.body) {
				t.Fatalf("projected substrate differs: length=%d, err=%v", len(got), err)
			}
			projectedInfo, err := os.Stat(projected)
			if err != nil {
				t.Fatal(err)
			}
			if projectedInfo.Mode().Perm() != 0o440 || os.SameFile(after, projectedInfo) {
				t.Fatalf("projection must be sealed and have its own inode: %v", projectedInfo)
			}
			var stat unix.Stat_t
			if err := unix.Stat(projected, &stat); err != nil {
				t.Fatal(err)
			}
			if stat.Uid != uint32(os.Getuid()) || stat.Gid != uint32(os.Getgid()) {
				t.Fatalf("projection owner = %d:%d", stat.Uid, stat.Gid)
			}
		})
	}
}

func TestRuntimeSubstrateProjectionFailureLeavesNoPartialFile(t *testing.T) {
	for _, kind := range []string{"cancelled", "digest mismatch", "size changed", "file collision", "symlink collision"} {
		t.Run(kind, func(t *testing.T) {
			body := make([]byte, (64<<10)+7)
			cachePath := t.TempDir() + "/substrate.ext4"
			if err := os.WriteFile(cachePath, body, 0o444); err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(body)
			source := &DiskSource{path: cachePath, digest: sha256sum.FormatDigest(sum[:]), sizeBytes: int64(len(body))}
			arena := t.TempDir()
			destination := arena + "/substrate.ext4"
			ctx := context.Background()
			switch kind {
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "digest mismatch":
				source.digest = "sha256:" + strings.Repeat("0", 64)
			case "size changed":
				source.sizeBytes++
			case "file collision":
				if err := os.WriteFile(destination, []byte("existing file"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink collision":
				if err := os.Symlink(cachePath, destination); err != nil {
					t.Fatal(err)
				}
			}
			path, err := source.MaterializeInto(ctx, arena, "substrate.ext4", os.Getuid(), os.Getgid())
			if err == nil || path != "" {
				t.Fatalf("projection = %q, err=%v", path, err)
			}
			if kind == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation not preserved: %v", err)
			}
			switch kind {
			case "file collision":
				if got, err := os.ReadFile(destination); err != nil || string(got) != "existing file" {
					t.Fatalf("existing file changed: %q, %v", got, err)
				}
			case "symlink collision":
				if target, err := os.Readlink(destination); err != nil || target != cachePath {
					t.Fatalf("existing symlink changed: %q, %v", target, err)
				}
			default:
				if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("partial projection remains: %v", err)
				}
			}
			if got, err := os.ReadFile(cachePath); err != nil || !bytes.Equal(got, body) {
				t.Fatal("source was modified")
			}
		})
	}
}

func TestSubstrateProjectionWriterPropagatesFileErrors(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	writer := substrateProjectionWriter{file: file}
	for _, body := range [][]byte{{0}, {1}} {
		if n, err := writer.Write(body); n != 0 || !errors.Is(err, os.ErrClosed) {
			t.Fatalf("closed file write = %d, %v", n, err)
		}
	}
}

func TestRuntimeSubstrateProjectionKeepsZeroSpansSparseOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("allocation assertion requires Linux")
	}
	arena := t.TempDir()
	var filesystem unix.Statfs_t
	if err := unix.Statfs(arena, &filesystem); err != nil {
		t.Fatal(err)
	}
	const ext4Magic, tmpfsMagic = 0xef53, 0x01021994
	if filesystem.Type != ext4Magic && filesystem.Type != tmpfsMagic {
		t.Skip("allocation assertion requires an ext4 or tmpfs test directory")
	}
	body := make([]byte, (8<<20)+7)
	body[0], body[len(body)-1] = 1, 2
	cachePath := arena + "/dense.ext4"
	if err := os.WriteFile(cachePath, body, 0o444); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	source := &DiskSource{path: cachePath, digest: sha256sum.FormatDigest(sum[:]), sizeBytes: int64(len(body))}
	projected, err := source.MaterializeInto(context.Background(), arena, "substrate.ext4", os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	var stat unix.Stat_t
	if err := unix.Stat(projected, &stat); err != nil {
		t.Fatal(err)
	}
	var dense unix.Stat_t
	if err := unix.Stat(cachePath, &dense); err != nil {
		t.Fatal(err)
	}
	t.Logf("filesystem=%#x logical=%d dense allocated=%d sparse allocated=%d", filesystem.Type, stat.Size, dense.Blocks*512, stat.Blocks*512)
	if dense.Size != int64(len(body)) || dense.Blocks*512 < dense.Size {
		t.Fatalf("dense control was not fully allocated: size=%d, allocated=%d", dense.Size, dense.Blocks*512)
	}
	if stat.Size != dense.Size || stat.Blocks*512 >= dense.Blocks*512/2 {
		t.Fatalf("zero spans were allocated: size=%d, allocated=%d", stat.Size, stat.Blocks*512)
	}
	if got, err := os.ReadFile(projected); err != nil || !bytes.Equal(got, body) {
		t.Fatal("sparse output contents differ")
	}
}

// Cancellation is triggered by an actual read, rather than by timing a fast
// local copy. The next read must stop before consuming a second chunk.
func TestProjectionReaderStopsAfterCancellationDuringCopy(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	input := &cancelProjectionInput{cancel: cancel}
	output, err := os.CreateTemp(t.TempDir(), "partial")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	n, err := io.Copy(substrateProjectionWriter{file: output}, &projectionReader{ctx: ctx, reader: input})
	if !errors.Is(err, context.Canceled) || n != 1 || input.reads != 1 {
		t.Fatalf("copy = %d, %v; reads=%d", n, err, input.reads)
	}
	body, err := os.ReadFile(output.Name())
	if err != nil || string(body) != "x" {
		t.Fatalf("first write = %q, %v", body, err)
	}
}

type cancelProjectionInput struct {
	cancel context.CancelFunc
	reads  int
}

func (r *cancelProjectionInput) Read(p []byte) (int, error) {
	r.reads++
	p[0] = 'x'
	r.cancel()
	return 1, nil
}
