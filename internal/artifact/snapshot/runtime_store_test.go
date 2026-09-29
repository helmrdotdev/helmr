package snapshot

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestSnapshotRuntimeObjectBindsStoreMetadataAndBytes(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("runtime snapshots require Linux")
	}
	body := []byte("managed runtime")
	descriptor := testRuntimeDescriptor()
	descriptor.Digest = sha256sum.DigestBytes(body)
	descriptor.SizeBytes = int64(len(body))
	store := runtimeObjectStore{
		object: cas.Object{
			Digest:    descriptor.Digest,
			SizeBytes: descriptor.SizeBytes,
			MediaType: descriptor.MediaType,
		},
		body: body,
	}
	snapshot, err := ReadRuntime(
		context.Background(),
		store,
		t.TempDir(),
		descriptor,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}

	store.body = []byte("divergent bytes")
	if _, err := ReadRuntime(
		context.Background(),
		store,
		t.TempDir(),
		descriptor,
	); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("error = %v", err)
	}
}

func TestSnapshotRuntimeObjectRejectsDivergentMetadata(t *testing.T) {
	body := []byte("managed runtime")
	descriptor := testRuntimeDescriptor()
	descriptor.Digest = sha256sum.DigestBytes(body)
	descriptor.SizeBytes = int64(len(body))
	for name, mutate := range map[string]func(*cas.Object){
		"digest": func(object *cas.Object) { object.Digest = sha256sum.DigestBytes([]byte("other")) },
		"size":   func(object *cas.Object) { object.SizeBytes++ },
		"type":   func(object *cas.Object) { object.MediaType = "application/octet-stream" },
	} {
		t.Run(name, func(t *testing.T) {
			object := cas.Object{
				Digest:    descriptor.Digest,
				SizeBytes: descriptor.SizeBytes,
				MediaType: descriptor.MediaType,
			}
			mutate(&object)
			_, err := ReadRuntime(
				context.Background(),
				runtimeObjectStore{object: object, body: body},
				t.TempDir(),
				descriptor,
			)
			if err == nil || !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSnapshotRuntimeObjectRequiresStore(t *testing.T) {
	if _, err := ReadRuntime(
		context.Background(),
		nil,
		t.TempDir(),
		testRuntimeDescriptor(),
	); err == nil {
		t.Fatal("nil runtime store was accepted")
	}
}

type runtimeObjectStore struct {
	object cas.Object
	body   []byte
	err    error
}

func (s runtimeObjectStore) Stat(context.Context, string) (cas.Object, error) {
	return s.object, s.err
}

func (s runtimeObjectStore) Get(context.Context, string) (io.ReadCloser, error) {
	if s.err != nil {
		return nil, s.err
	}
	return io.NopCloser(bytes.NewReader(s.body)), nil
}

var _ cas.Reader = runtimeObjectStore{}

func testRuntimeDescriptor() artifact.RuntimeDescriptor {
	return artifact.RuntimeDescriptor{
		Architecture:    definition.ArchitectureX8664,
		Digest:          "sha256:" + strings.Repeat("a", 64),
		FormatVersion:   artifact.RuntimeDescriptorFormatVersion,
		MediaType:       artifact.RuntimeArtifactMediaType,
		RuntimeContract: definition.RuntimeContract,
		SizeBytes:       artifact.SquashFSPhysicalAlign,
	}
}
