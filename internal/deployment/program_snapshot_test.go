package deployment

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
)

func TestSnapshotProgramObjectsBindsBothDescriptorsAndDriveSources(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Program snapshots require Linux")
	}
	codeBody := []byte("Program code")
	dependencyBody := []byte("Program dependencies")
	code := ProgramDescriptor{
		Digest:    digestBytes(codeBody),
		SizeBytes: int64(len(codeBody)),
		MediaType: ProgramCodeArtifactMediaType,
	}
	dependencies := ProgramDescriptor{
		Digest:    digestBytes(dependencyBody),
		SizeBytes: int64(len(dependencyBody)),
		MediaType: ProgramDependencyArtifactMediaType,
	}
	store := programObjectStore{objects: map[string]programObject{
		code.Digest:         {descriptor: code, body: codeBody},
		dependencies.Digest: {descriptor: dependencies, body: dependencyBody},
	}}
	snapshot, err := SnapshotProgramObjects(
		context.Background(),
		store,
		t.TempDir(),
		code,
		dependencies,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	jail := t.TempDir()
	if err := snapshot.Code.LinkInto(jail, "code.squashfs", os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Dependencies.LinkInto(
		jail,
		"dependencies.squashfs",
		os.Getuid(),
		os.Getgid(),
	); err != nil {
		t.Fatal(err)
	}
	gotCode, err := os.ReadFile(filepath.Join(jail, "code.squashfs"))
	if err != nil {
		t.Fatal(err)
	}
	gotDependencies, err := os.ReadFile(filepath.Join(jail, "dependencies.squashfs"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotCode, codeBody) || !bytes.Equal(gotDependencies, dependencyBody) {
		t.Fatal("linked Program drives do not match their descriptors")
	}
}

type programObject struct {
	descriptor ProgramDescriptor
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
