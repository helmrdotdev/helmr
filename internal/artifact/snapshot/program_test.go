package snapshot

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestSnapshotProgramBindsDescriptorAndDriveSource(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Program snapshots require Linux")
	}
	body := []byte("Program")
	program := artifact.ProgramDescriptor{
		Digest:    sha256sum.DigestBytes(body),
		SizeBytes: int64(len(body)),
		MediaType: artifact.ProgramArtifactMediaType,
	}
	store := programObjectStore{objects: map[string]programObject{
		program.Digest: {descriptor: program, body: body},
	}}
	snapshot, err := ReadProgram(
		context.Background(),
		store,
		t.TempDir(),
		program,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	jail := t.TempDir()
	if err := snapshot.LinkInto(jail, "program.squashfs", os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(jail, "program.squashfs"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("linked Program drive does not match its descriptor")
	}
}

type programObject struct {
	descriptor artifact.ProgramDescriptor
	body       []byte
}

type programObjectStore struct {
	objects map[string]programObject
}

func (s programObjectStore) Stat(_ context.Context, digest string) (cas.Object, error) {
	object := s.objects[digest]
	return cas.Object{
		Digest:    object.descriptor.Digest,
		SizeBytes: object.descriptor.SizeBytes,
		MediaType: object.descriptor.MediaType,
	}, nil
}

func (s programObjectStore) Get(_ context.Context, digest string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.objects[digest].body)), nil
}

var _ cas.Reader = programObjectStore{}
