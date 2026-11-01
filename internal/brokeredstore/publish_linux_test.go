package brokeredstore

import (
	"bytes"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// A single write is create-only; losing the create race re-checks the stored
// object through a new grant instead of replacing it.
func TestPublishSingleIsCreateOnly(t *testing.T) {
	g := newGrantServer(t)
	g.partSize = 1 << 20
	store, _ := New(g, workerapi.StorageCAS, g.storage.Client())
	file, descriptor := publishedFile(t, []byte("checkpoint"))
	object, err := store.Publish(scoped(t), descriptor, file)
	if err != nil || object.Digest != descriptor.Digest || !bytes.Equal(g.objects[descriptor.Digest], []byte("checkpoint")) {
		t.Fatalf("publish = %+v, %v", object, err)
	}
	if _, err := store.Publish(scoped(t), descriptor, file); err != nil || g.writes != 2 {
		t.Fatalf("republish = %v after %d grants", err, g.writes)
	}
	if _, err := store.Publish(t.Context(), descriptor, file); err == nil {
		t.Fatal("publish without authority")
	}
}

// Multipart parts are uploaded through part grants and completed in order; a
// failed part aborts the owned upload instead of completing it.
func TestPublishMultipartCompletesOrAborts(t *testing.T) {
	g := newGrantServer(t)
	store, _ := New(g, workerapi.StorageCAS, g.storage.Client())
	file, descriptor := publishedFile(t, []byte("0123456789"))
	if _, err := store.Publish(scoped(t), descriptor, file); err != nil {
		t.Fatal(err)
	}
	if len(g.completed) != 3 || g.completed[2].Number != 3 || !bytes.Equal(g.objects[descriptor.Digest], []byte("0123456789")) || g.aborted {
		t.Fatalf("completed %+v, aborted %v", g.completed, g.aborted)
	}
	failed, other := publishedFile(t, []byte("abcdefghij"))
	g.failPart = true
	if _, err := store.Publish(scoped(t), other, failed); err == nil || !g.aborted {
		t.Fatalf("failed part = %v, aborted %v", err, g.aborted)
	}
}
