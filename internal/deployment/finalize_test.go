package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/definition"
	"io"
	"log/slog"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/jackc/pgx/v5"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestVerificationStopsAfterDisconnectedProgress(t *testing.T) {
	image := finalizeDiskFixture(t)
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(image))
	descriptor := cas.Descriptor{Digest: digest, SizeBytes: int64(len(image)), MediaType: definition.ComputerSeedMediaType}
	store := &finalizeObjectStore{descriptor: descriptor, body: image}
	finalizer := NewFinalizer(store, store, bundle.Admission{}, discardLogger())
	err := finalizer.verifyObjects(t.Context(), finalizePossessionStore{}, uuid.NewV7(), preparedBundle{objects: []cas.Descriptor{descriptor}}, func(string) error { return context.Canceled })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}

type finalizePossessionStore struct{ db.Querier }

func (finalizePossessionStore) GetCasObject(context.Context, db.GetCasObjectParams) (db.CasObject, error) {
	return db.CasObject{}, pgx.ErrNoRows
}

type finalizeObjectStore struct {
	cas.UploadStore
	descriptor cas.Descriptor
	body       []byte
}

func (s *finalizeObjectStore) Stat(context.Context, string) (cas.Object, error) {
	return cas.Object{
		Digest: s.descriptor.Digest, SizeBytes: s.descriptor.SizeBytes, MediaType: s.descriptor.MediaType,
	}, nil
}

func (s *finalizeObjectStore) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.body)), nil
}

func (s *finalizeObjectStore) PromoteQuarantine(
	context.Context,
	string,
	cas.Descriptor,
) (cas.Object, error) {
	return s.Stat(context.Background(), s.descriptor.Digest)
}

func finalizeDiskFixture(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	output.WriteString("helmr-firecracker-filepack-v0\n")
	header := []byte(fmt.Sprintf(`{"version":0,"role":"computer-seed","logical_size":%d,"chunk_size":4194304,"codec":"zstd"}`, disk.SeedCapacity))
	if err := binary.Write(&output, binary.BigEndian, uint32(len(header))); err != nil {
		t.Fatal(err)
	}
	output.Write(header)
	output.WriteByte(255)
	return output.Bytes()
}

func TestVerifyObjectDisk(t *testing.T) {
	body := finalizeDiskFixture(t)
	descriptor := cas.Descriptor{Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(body)), SizeBytes: int64(len(body)), MediaType: definition.ComputerSeedMediaType}
	for _, kind := range []string{"valid", "digest", "size", "truncated", "trailing"} {
		t.Run(kind, func(t *testing.T) {
			data := bytes.Clone(body)
			object := descriptor
			switch kind {
			case "digest":
				object.Digest = "sha256:" + strings.Repeat("a", 64)
			case "size":
				object.SizeBytes++
			case "truncated":
				data = data[:len(data)-1]
			case "trailing":
				data = append(data, 0)
			}
			store := &finalizeObjectStore{descriptor: object, body: data}
			err := NewFinalizer(store, store, bundle.Admission{}, discardLogger()).verifyObject(t.Context(), bundle.Manifest{}, object)
			if kind == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("invalid disk accepted")
			}
		})
	}
}

func TestVerifyObjectDiskCancellationIsNotInvalid(t *testing.T) {
	body := finalizeDiskFixture(t)
	descriptor := cas.Descriptor{Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(body)), SizeBytes: int64(len(body)), MediaType: definition.ComputerSeedMediaType}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store := cancellingObjectStore{body: body, cancel: cancel}
	err := NewFinalizer(store, store, bundle.Admission{}, discardLogger()).verifyObject(ctx, bundle.Manifest{}, descriptor)
	var invalid InvalidObjectError
	if !errors.Is(err, context.Canceled) || errors.As(err, &invalid) {
		t.Fatalf("cancellation misclassified: %v", err)
	}
}

type cancellingObjectStore struct {
	cas.UploadStore
	body   []byte
	cancel context.CancelFunc
}

func (s cancellingObjectStore) Get(context.Context, string) (io.ReadCloser, error) {
	return cancellingObjectReader{Reader: bytes.NewReader(s.body), cancel: s.cancel}, nil
}

type cancellingObjectReader struct {
	io.Reader
	cancel context.CancelFunc
}

func (r cancellingObjectReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.cancel()
	return n, err
}
func (r cancellingObjectReader) Close() error { return nil }

func TestPrepareRejectsInvalidRetryKeysBeforeReadingObjects(t *testing.T) {
	principal := auth.Principal{OrgID: uuid.NewV7(), Kind: auth.PrincipalKindAPIKey, Role: auth.RoleDeveloper, ProjectID: uuid.NewV7().String(), EnvironmentID: uuid.NewV7().String(), Permissions: []auth.Permission{auth.PermissionDeploymentsWrite}}
	scope := auth.Scope{OrgID: principal.OrgID, ProjectID: principal.ProjectID, EnvironmentID: principal.EnvironmentID}
	finalizer := NewFinalizer(nil, nil, bundle.Admission{}, discardLogger())
	for _, key := range []string{"", " ", "a\x00b", "a\nb", string([]byte{0xff}), strings.Repeat("x", 513)} {
		_, err := finalizer.Prepare(t.Context(), principal, scope, "unused", key)
		var invalid InputError
		if !errors.As(err, &invalid) {
			t.Fatalf("key %q: %v", key, err)
		}
	}
}
