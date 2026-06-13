package cas

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestObjectKey(t *testing.T) {
	key, err := ObjectKey("tenant-a", "sha256:7b927bbd759163db342b22ac0329b49998afa33e911c060e112998b1a7d5339e")
	if err != nil {
		t.Fatal(err)
	}
	if key != "tenant-a/sha256/7b927bbd759163db342b22ac0329b49998afa33e911c060e112998b1a7d5339e" {
		t.Fatalf("key = %s", key)
	}
}

func TestObjectTaggingKeepsDeploymentSourcesNonExpirable(t *testing.T) {
	if got := objectTagging(DeploymentSourceArtifactMediaType); got != "" {
		t.Fatalf("deployment source tagging = %q", got)
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

func TestFileStoreGetRejectsTamperedContent(t *testing.T) {
	root := t.TempDir()
	store, err := NewFile(root)
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.Put(t.Context(), "text/plain", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	path, _, err := store.path(object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("HELLO"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err := store.Get(t.Context(), object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(body); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("read error = %v, want digest mismatch", err)
	}
	_ = body.Close()
}

func TestFileStageCommitPublishesFinalDigestAndCleansStage(t *testing.T) {
	store, err := NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stage, err := store.Stage(t.Context(), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	fileStage := stage.(*fileStage)
	stagedPath := fileStage.path
	if _, err := stage.Write([]byte("he")); err != nil {
		t.Fatal(err)
	}
	if _, err := stage.Write([]byte("llo")); err != nil {
		t.Fatal(err)
	}

	object, err := stage.Commit(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	if object.Digest != sha256sum.DigestBytes([]byte("hello")) || object.SizeBytes != 5 || object.MediaType != "text/plain" {
		t.Fatalf("object = %+v", object)
	}
	if object.Key != "sha256/2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("key = %q", object.Key)
	}
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Fatalf("staged file stat error = %v", err)
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
}

func TestFileStageCommitDoesNotPublishDataWithoutMetadata(t *testing.T) {
	root := t.TempDir()
	store, err := NewFile(root)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := store.Stage(t.Context(), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("hello")
	if _, err := stage.Write(content); err != nil {
		t.Fatal(err)
	}
	key, err := ObjectKey("", sha256sum.DigestBytes(content))
	if err != nil {
		t.Fatal(err)
	}
	finalPath := filepath.Join(root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(finalPath+".json", 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := stage.Commit(t.Context()); err == nil {
		t.Fatal("commit succeeded with blocked metadata path")
	}
	if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
		t.Fatalf("final object stat error = %v, want not exist", err)
	}
}

func TestFileStageAbortCleansTempAndDoesNotPublish(t *testing.T) {
	store, err := NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stage, err := store.Stage(t.Context(), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	fileStage := stage.(*fileStage)
	stagedPath := fileStage.path
	content := []byte("discard")
	if _, err := stage.Write(content); err != nil {
		t.Fatal(err)
	}

	if err := stage.Abort(t.Context()); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Fatalf("staged file stat error = %v", err)
	}
	if _, err := store.Stat(t.Context(), sha256sum.DigestBytes(content)); err == nil {
		t.Fatal("expected aborted object to be missing")
	}
}
