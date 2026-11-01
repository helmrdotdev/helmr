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

func (testUploadStore) StreamQuarantine(context.Context, string, cas.Descriptor, io.Reader) error {
	return errTestQuarantine
}

func (testUploadStore) PromoteQuarantine(context.Context, string, cas.Descriptor) (cas.Object, error) {
	return cas.Object{}, errTestQuarantine
}

// testStorageSigner signs nothing real; routes that reach storage grants in
// construction-level tests only need a complete configuration.
type testStorageSigner struct{ testUploadStore }

func (testStorageSigner) SignGet(context.Context, string, time.Time) (cas.SignedRequest, error) {
	return cas.SignedRequest{}, errTestQuarantine
}
func (testStorageSigner) SignHead(context.Context, string, time.Time) (cas.SignedRequest, error) {
	return cas.SignedRequest{}, errTestQuarantine
}
func (testStorageSigner) VerifyDescriptor(context.Context, cas.Descriptor) (cas.Object, error) {
	return cas.Object{}, errTestQuarantine
}
func (testStorageSigner) UploadLayout(cas.Descriptor) (cas.Upload, bool) { return cas.Upload{}, false }
func (testStorageSigner) SignPut(context.Context, cas.Descriptor, time.Time) (cas.SignedRequest, error) {
	return cas.SignedRequest{}, errTestQuarantine
}
func (testStorageSigner) CreateUpload(context.Context, string, cas.Descriptor) (cas.Upload, error) {
	return cas.Upload{}, errTestQuarantine
}
func (testStorageSigner) SignPart(context.Context, cas.Descriptor, cas.Upload, int32, time.Time) (cas.SignedRequest, error) {
	return cas.SignedRequest{}, errTestQuarantine
}
func (testStorageSigner) CompleteUpload(context.Context, cas.Descriptor, cas.Upload, []cas.CompletedPart) (cas.Object, error) {
	return cas.Object{}, errTestQuarantine
}
func (testStorageSigner) AbortUpload(context.Context, cas.Upload, string) error {
	return errTestQuarantine
}
