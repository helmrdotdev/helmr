package session_test

import (
	"bytes"
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
	"os"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/dispatch"
)

// A boot seed's data key is created by the seed allocation that builds it and
// is refused to another build, to a Computer that did not select it, and to the
// builder once its authority ends.
func TestBootSeedKeyFollowsSeedAuthority(t *testing.T) {
	p, a, id := bootSeedFixture(t)
	f := p.f
	r, err := a.AllocateBootSeed(t.Context(), f.env, id, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := a.DeliverBootSeed(t.Context(), *f.host(), bootSeedReceipt(r))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := computer.NewKeyBroker(f.pool, preparationWrapper(t))
	if err != nil {
		t.Fatal(err)
	}
	builder := computer.StorageScope{BootSeed: &delivery.Executor}
	key, err := keys.BootSeedKey(t.Context(), *f.host(), builder, id)
	if err != nil || len(key) != 32 {
		t.Fatalf("builder key = %d bytes, %v", len(key), err)
	}
	again, err := keys.BootSeedKey(t.Context(), *f.host(), builder, id)
	if err != nil || !bytes.Equal(again, key) {
		t.Fatalf("builder key changed on retry: %v", err)
	}
	if _, err := keys.BootSeedKey(t.Context(), *f.host(), builder, uuid.NewV7()); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("other build = %v", err)
	}
	forged := delivery.Executor
	forged.ChannelCredential = bytes.Repeat([]byte{1}, 32)
	if _, err := keys.BootSeedKey(t.Context(), *f.host(), computer.StorageScope{BootSeed: &forged}, id); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("forged executor credential = %v", err)
	}
	// The builder reads its image root's disk graph and nothing outside it.
	grants, err := computer.NewStorageBroker(f.pool, nullSigner{}, nullSigner{})
	if err != nil {
		t.Fatal(err)
	}
	var root, unrelated string
	if err := f.pool.QueryRow(t.Context(), `SELECT r.pack_digest FROM boot_seed_builds b JOIN images i ON (i.environment_id,i.id)=(b.environment_id,b.image_id) JOIN disk_roots r ON (r.environment_id,r.id)=(i.environment_id,i.root_id) WHERE b.environment_id=$1 AND b.id=$2`, f.env, id).Scan(&root); err != nil {
		t.Fatal(err)
	}
	if _, err := grants.Read(t.Context(), *f.host(), builder, "cas", root); err != nil {
		t.Fatalf("builder image root = %v", err)
	}
	unrelated = "sha256:" + strings.Repeat("e", 64)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($2,7);
 INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT org_id,$2,7,'application/octet-stream' FROM environments WHERE id=$1;
 INSERT INTO disk_objects(environment_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at) SELECT id,$2,org_id,project_id,7,'application/octet-stream','segment',0,'{}',clock_timestamp() FROM environments WHERE id=$1`,
		pgx.QueryExecModeSimpleProtocol, f.env, unrelated)
	if _, err := grants.Read(t.Context(), *f.host(), builder, "cas", unrelated); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("builder read outside its image graph = %v", err)
	}
	lease := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, InstanceID: f.computer, Epoch: 1}
	if _, err := keys.BootSeedKey(t.Context(), *f.host(), computer.StorageScope{Computer: &lease}, id); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("Computer that did not select the seed = %v", err)
	}
	if err := computer.FailBootSeedBuild(t.Context(), f.pool, *f.host(), delivery.Executor, "test_failure"); err != nil {
		t.Fatal(err)
	}
	if err := dispatch.ObserveBootSeedStopped(t.Context(), f.pool, *f.host(), bootSeedReceipt(r)); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.BootSeedKey(t.Context(), *f.host(), builder, id); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("builder after its authority ended = %v", err)
	}
}

// nullSigner signs placeholder requests; these tests assert authorization only.
type nullSigner struct{}

func (nullSigner) Stat(context.Context, string) (cas.Object, error) {
	return cas.Object{}, os.ErrNotExist
}
func (nullSigner) SignGet(_ context.Context, digest string, expiresAt time.Time) (cas.SignedRequest, error) {
	return cas.SignedRequest{Method: "GET", URL: "https://storage.test/" + digest, ExpiresAt: expiresAt}, nil
}
func (nullSigner) SignHead(_ context.Context, digest string, expiresAt time.Time) (cas.SignedRequest, error) {
	return cas.SignedRequest{Method: "HEAD", URL: "https://storage.test/" + digest, ExpiresAt: expiresAt}, nil
}
func (nullSigner) VerifyDescriptor(context.Context, cas.Descriptor) (cas.Object, error) {
	return cas.Object{}, os.ErrNotExist
}
func (nullSigner) UploadLayout(cas.Descriptor) (cas.Upload, bool) { return cas.Upload{}, false }
func (nullSigner) SignPut(_ context.Context, d cas.Descriptor, expiresAt time.Time) (cas.SignedRequest, error) {
	return cas.SignedRequest{Method: "PUT", URL: "https://storage.test/" + d.Digest, ExpiresAt: expiresAt}, nil
}
func (nullSigner) CreateUpload(context.Context, string, cas.Descriptor) (cas.Upload, error) {
	return cas.Upload{}, errors.New("no multipart")
}
func (nullSigner) SignPart(context.Context, cas.Descriptor, cas.Upload, int32, time.Time) (cas.SignedRequest, error) {
	return cas.SignedRequest{}, errors.New("no multipart")
}
func (nullSigner) CompleteUpload(context.Context, cas.Descriptor, cas.Upload, []cas.CompletedPart) (cas.Object, error) {
	return cas.Object{}, errors.New("no multipart")
}
func (nullSigner) AbortUpload(context.Context, cas.Upload, string) error { return nil }
