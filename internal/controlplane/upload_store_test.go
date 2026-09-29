package controlplane

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
)

var errTestQuarantine = errors.New("test CAS has no upload quarantine")

// testUploadStore gives Computer fixtures the server's cas.UploadStore over a
// file CAS. Those fixtures publish and stat objects; they never admit uploads.
type testUploadStore struct {
	*cas.File
}

func newTestUploadStore(t *testing.T) testUploadStore {
	t.Helper()
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return testUploadStore{File: store}
}

func (testUploadStore) PutQuarantine(context.Context, string, cas.Descriptor, io.Reader) error {
	return errTestQuarantine
}

func (testUploadStore) HasExactQuarantine(context.Context, string, cas.Descriptor) (bool, error) {
	return false, errTestQuarantine
}

func (testUploadStore) PresignQuarantine(context.Context, string, cas.Descriptor, time.Duration) (cas.PresignedUpload, error) {
	return cas.PresignedUpload{}, errTestQuarantine
}

func (testUploadStore) PromoteQuarantine(context.Context, string, cas.Descriptor) (cas.Object, error) {
	return cas.Object{}, errTestQuarantine
}
