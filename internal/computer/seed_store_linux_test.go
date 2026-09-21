//go:build linux

package computer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
)

func TestSeedTransferAndComputerSeparation(t *testing.T) {
	dir := t.TempDir()
	objects, err := cas.NewFile(filepath.Join(dir, "cas"))
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := checkpoint.New(bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	store := SeedStore{CAS: objects}
	const size = 8 << 20
	source := filepath.Join(dir, "source")
	content := make([]byte, size)
	copy(content[5<<20:], []byte("authored initial image"))
	if err := os.WriteFile(source, content, 0600); err != nil {
		t.Fatal(err)
	}
	packed := filepath.Join(dir, "packed")
	artifact, err := EncodeSeed(t.Context(), source, packed)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(packed)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	publisher := diskTestPublisher{objects}
	if _, err := publisher.Publish(t.Context(), artifact.Object, file); err != nil {
		t.Fatal(err)
	}
	if artifact.Object.SizeBytes >= size {
		t.Fatal("sparse seed was not compressed")
	}
	target := filepath.Join(dir, "target")
	if err := store.Decode(t.Context(), artifact, target, size); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatalf("roundtrip: %v", err)
	}
	if err := store.Decode(t.Context(), artifact, target, size); !errors.Is(err, os.ErrExist) {
		t.Fatalf("collision: %v", err)
	}
	actual, err = os.ReadFile(target)
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatal("collision changed existing file")
	}
	for _, tc := range []struct {
		name     string
		artifact SeedArtifact
	}{
		{"digest", SeedArtifact{Object: cas.Descriptor{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SizeBytes: artifact.Object.SizeBytes, MediaType: SeedMediaType}, LogicalBytes: size}},
		{"logical-size", SeedArtifact{Object: artifact.Object, LogicalBytes: 2 * size}},
		{"encoded-size", SeedArtifact{Object: cas.Descriptor{Digest: artifact.Object.Digest, SizeBytes: artifact.Object.SizeBytes + 1, MediaType: SeedMediaType}, LogicalBytes: size}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(dir, tc.name)
			if err := store.Decode(t.Context(), tc.artifact, out, size); err == nil {
				t.Fatal("invalid seed accepted")
			}
			if _, err := os.Lstat(out); !os.IsNotExist(err) {
				t.Fatalf("invalid disk exposed: %v", err)
			}
		})
	}
	// Even relabeling the outer media type cannot make a seed into a Computer disk.
	forged := DiskArtifact{Object: artifact.Object, LogicalBytes: size}
	forged.Object.MediaType = DiskMediaType
	out := filepath.Join(dir, "as-computer")
	if err := (DiskStore{CAS: objects, Cipher: cipher}).Restore(t.Context(), diskTestComputer, forged, out, size); err == nil {
		t.Fatal("seed accepted as Computer disk")
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		t.Fatal("failed Computer restore exposed disk")
	}
	computer, err := (DiskStore{Cipher: cipher}).Capture(t.Context(), diskTestComputer, source, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer computer.Close()
	if err := computer.Upload(t.Context(), publisher); err != nil {
		t.Fatal(err)
	}
	reverse := SeedArtifact{Object: computer.Artifact().Object, LogicalBytes: size}
	reverse.Object.MediaType = SeedMediaType
	if err := store.Decode(t.Context(), reverse, filepath.Join(dir, "as-seed"), size); err == nil {
		t.Fatal("Computer disk accepted as seed")
	}
	// Malformed content must remove decoded output even with a matching digest.
	body, err := objects.Get(t.Context(), artifact.Object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		t.Fatal(err)
	}
	encoded[len(encoded)-1] ^= 1
	corrupt, err := objects.Put(t.Context(), SeedMediaType, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	bad := SeedArtifact{Object: cas.Descriptor{Digest: corrupt.Digest, SizeBytes: corrupt.SizeBytes, MediaType: corrupt.MediaType}, LogicalBytes: size}
	out = filepath.Join(dir, "corrupt")
	if err := store.Decode(t.Context(), bad, out, size); err == nil {
		t.Fatal("corrupt seed accepted")
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		t.Fatal("corrupt seed exposed")
	}
	// Capacity and format failures must precede any storage I/O.
	rejecting := SeedStore{CAS: seedNoRead{t: t}}
	if err := rejecting.Decode(t.Context(), artifact, filepath.Join(dir, "small"), size/2); err == nil {
		t.Fatal("capacity accepted")
	}
	wrong := artifact
	wrong.Object.MediaType = DiskMediaType
	if err := rejecting.Decode(t.Context(), wrong, filepath.Join(dir, "format"), size); err == nil {
		t.Fatal("format accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := EncodeSeed(ctx, source, filepath.Join(dir, "cancelled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled encode: %v", err)
	}
}

type seedNoRead struct {
	cas.Reader
	t *testing.T
}

func (s seedNoRead) Get(context.Context, string) (io.ReadCloser, error) {
	s.t.Fatal("unexpected CAS read")
	return nil, errors.New("unexpected read")
}
