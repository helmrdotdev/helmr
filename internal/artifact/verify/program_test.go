package verify

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/snapshot"
	"github.com/helmrdotdev/helmr/internal/cas"
)

func TestVerifyProgramAcceptsOneSnapshotBeforeCgroupSetup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Program verification requires Linux")
	}
	body := []byte("Program")
	program := artifact.ProgramDescriptor{
		Digest:    digestBytes(body),
		SizeBytes: int64(len(body)),
		MediaType: artifact.ProgramArtifactMediaType,
	}
	store := programObjectStore{objects: map[string]programObject{
		program.Digest: {descriptor: program, body: body},
	}}
	programSnapshot, err := snapshot.ProgramObject(
		context.Background(),
		store,
		t.TempDir(),
		program,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer programSnapshot.Close()

	_, err = Program(
		context.Background(),
		filepath.Join(t.TempDir(), "missing-cgroup"),
		"lease-123",
		programSnapshot,
	)
	if err == nil {
		t.Fatal("expected cgroup setup to fail")
	}
	if strings.Contains(err.Error(), "descriptor count") {
		t.Fatalf("single Program snapshot rejected: %v", err)
	}
	if !strings.Contains(err.Error(), "open artifact verifier unit cgroup root") {
		t.Fatalf("error = %v, want cgroup root open failure", err)
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
