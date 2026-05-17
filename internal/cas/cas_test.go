package cas

import (
	"io"
	"strings"
	"testing"
)

func TestDigestReader(t *testing.T) {
	_, digest, err := DigestReader(strings.NewReader("helmr"))
	if err != nil {
		t.Fatal(err)
	}
	if digest != "sha256:9d06c282b54c131bd2981a2e45b4345c1f3d52d83fddac0fba7d616cc0d61cd3" {
		t.Fatalf("digest = %s", digest)
	}
}

func TestObjectKey(t *testing.T) {
	key, err := ObjectKey("tenant-a", "sha256:7b927bbd759163db342b22ac0329b49998afa33e911c060e112998b1a7d5339e")
	if err != nil {
		t.Fatal(err)
	}
	if key != "tenant-a/sha256/7b927bbd759163db342b22ac0329b49998afa33e911c060e112998b1a7d5339e" {
		t.Fatalf("key = %s", key)
	}
}

func TestObjectTaggingKeepsTaskSourcesNonExpirable(t *testing.T) {
	if got := objectTagging(TaskSourceArtifactMediaType); got != "" {
		t.Fatalf("task source tagging = %q", got)
	}
	if got := objectTagging(CheckpointVMStateMediaType); got != "helmr-expirable=true" {
		t.Fatalf("checkpoint tagging = %q", got)
	}
}

func TestFileStoreRoundTrip(t *testing.T) {
	store, err := NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.Put(t.Context(), "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if object.MediaType != "text/plain" || object.SizeBytes != 5 {
		t.Fatalf("object = %+v", object)
	}
	stat, err := store.Stat(t.Context(), object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if stat.MediaType != "text/plain" || stat.SizeBytes != 5 {
		t.Fatalf("stat = %+v", stat)
	}
	body, err := store.Get(t.Context(), object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	bytes, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(bytes) != "hello" {
		t.Fatalf("body = %q", bytes)
	}
	if err := store.Delete(t.Context(), object.Digest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stat(t.Context(), object.Digest); err == nil {
		t.Fatal("expected deleted object to be missing")
	}
}
