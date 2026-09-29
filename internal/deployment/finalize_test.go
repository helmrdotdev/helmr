package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/bundle"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestFinalizeStopsBeforeTransactionAfterDisconnect(t *testing.T) {
	image := finalizeDiskFixture(t)
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(image))
	descriptor := cas.Descriptor{
		Digest: digest, SizeBytes: int64(len(image)), MediaType: bundle.ComputerImageMediaType,
	}
	store := &finalizeObjectStore{descriptor: descriptor, body: image}
	finalizer := NewFinalizer(store, store, bundle.Admission{}, discardLogger())
	_, err := finalizer.Finalize(
		t.Context(), finalizePossessionStore{}, nil,
		Finalization{
			orgID: uuid.UUID{15: 1}, projectID: pgvalue.UUID(uuid.UUID{15: 2}), environmentID: pgvalue.UUID(uuid.UUID{15: 3}),
			bundle: preparedBundle{
				root:    cas.Descriptor{Digest: "sha256:" + strings.Repeat("a", 64)},
				bundle:  bundle.Manifest{},
				objects: []cas.Descriptor{descriptor},
			},
		},
		func(string) error { return context.Canceled },
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation before transaction", err)
	}
}

type finalizePossessionStore struct{ db.Querier }

func (finalizePossessionStore) GetCasObject(context.Context, db.GetCasObjectParams) (db.CasObject, error) {
	return db.CasObject{}, pgx.ErrNoRows
}

func (finalizePossessionStore) GetDeploymentByBundleDigest(
	context.Context,
	db.GetDeploymentByBundleDigestParams,
) (db.Deployment, error) {
	return db.Deployment{}, pgx.ErrNoRows
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
	descriptor := cas.Descriptor{Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(body)), SizeBytes: int64(len(body)), MediaType: bundle.ComputerImageMediaType}
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
	descriptor := cas.Descriptor{Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(body)), SizeBytes: int64(len(body)), MediaType: bundle.ComputerImageMediaType}
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

func TestCreateDefinitionsBuildsOneCanonicalBatch(t *testing.T) {
	specID := pgvalue.UUID(uuid.NewV7())
	creator := &recordingDefinitionCreator{}
	definitions := []preparedDefinition{
		{kind: "task", declaredID: "task", manifest: []byte(`{"task":true}`), manifestDigest: []byte{1}},
		{
			kind: "sandbox", declaredID: "sandbox", manifest: []byte(`{"sandbox":true}`),
			manifestDigest: []byte{2},
		},
	}
	if err := createDefinitions(
		t.Context(), creator, pgvalue.UUID(uuid.NewV7()), pgvalue.UUID(uuid.NewV7()), definitions,
		map[string]db.ComputerSpec{"sandbox": {ID: specID}},
	); err != nil {
		t.Fatal(err)
	}
	if creator.calls != 1 {
		t.Fatalf("bulk calls = %d, want 1", creator.calls)
	}
	params := creator.params
	if len(params.Ids) != 2 || len(params.Kinds) != 2 || len(params.DeclaredIds) != 2 ||
		len(params.Manifests) != 2 || len(params.ManifestDigests) != 2 || len(params.ComputerSpecIds) != 2 {
		t.Fatalf("array cardinalities do not match: %+v", params)
	}
	firstID, firstErr := pgvalue.UUIDValue(params.Ids[0])
	secondID, secondErr := pgvalue.UUIDValue(params.Ids[1])
	if firstErr != nil || secondErr != nil || firstID == secondID {
		t.Fatalf("generated IDs = %v/%v errors=%v/%v", firstID, secondID, firstErr, secondErr)
	}
	if params.ComputerSpecIds[0].Valid || params.ComputerSpecIds[1] != specID {
		t.Fatalf("computer spec IDs = %+v, want null then %v", params.ComputerSpecIds, specID)
	}
}

func TestCreateDefinitionsRejectsMissingSpecMapping(t *testing.T) {
	creator := &recordingDefinitionCreator{}
	err := createDefinitions(
		t.Context(), creator, pgtype.UUID{}, pgtype.UUID{},
		[]preparedDefinition{{
			kind: "sandbox", declaredID: "sandbox",
		}},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), `computer spec for "sandbox" is not registered`) {
		t.Fatalf("error = %v", err)
	}
	if creator.calls != 0 {
		t.Fatalf("bulk calls = %d, want 0", creator.calls)
	}
}

func TestCreateDefinitionsRejectsResultCardinality(t *testing.T) {
	creator := &recordingDefinitionCreator{inserted: 1, insertedSet: true}
	err := createDefinitions(
		t.Context(), creator, pgtype.UUID{}, pgtype.UUID{},
		[]preparedDefinition{{kind: "task"}, {kind: "actor"}}, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "inserted 1 of 2 rows") {
		t.Fatalf("error = %v", err)
	}
}

func TestCreateDefinitionsAttributesDatabaseFailure(t *testing.T) {
	want := errors.New("database failure")
	creator := &recordingDefinitionCreator{err: want}
	err := createDefinitions(
		t.Context(), creator, pgtype.UUID{}, pgtype.UUID{}, nil, nil,
	)
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "create deployment definition") {
		t.Fatalf("error = %v", err)
	}
}

type recordingDefinitionCreator struct {
	params      db.CreateDeploymentDefinitionsParams
	inserted    int64
	insertedSet bool
	err         error
	calls       int
}

func (c *recordingDefinitionCreator) CreateDeploymentDefinitions(
	_ context.Context,
	params db.CreateDeploymentDefinitionsParams,
) (int64, error) {
	c.calls++
	c.params = params
	if c.err != nil {
		return 0, c.err
	}
	if c.insertedSet {
		return c.inserted, nil
	}
	return int64(len(params.Ids)), nil
}
