package computer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// grantSigner records what the broker asked one keyspace to sign.
type grantSigner struct {
	keyspace  string
	mu        sync.Mutex
	signed    []string
	stored    map[string]cas.Descriptor
	integrity error
	aborted   []string
}

func newGrantSigner(keyspace string) *grantSigner {
	return &grantSigner{keyspace: keyspace, stored: map[string]cas.Descriptor{}}
}

// grantKeyspaces holds one content keyspace per organization.
type grantKeyspaces struct {
	mu   sync.Mutex
	orgs map[uuid.UUID]*grantSigner
}

func (k *grantKeyspaces) of(org uuid.UUID) *grantSigner {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.orgs[org] == nil {
		k.orgs[org] = newGrantSigner("orgs/" + org.String())
	}
	return k.orgs[org]
}

func (k *grantKeyspaces) signer(org uuid.UUID) WriteSigner { return k.of(org) }

func (s *grantSigner) record(op, digest string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signed = append(s.signed, op+" "+digest)
}
func (s *grantSigner) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.signed...)
}
func (s *grantSigner) request(op, digest string, expiresAt time.Time) cas.SignedRequest {
	s.record(op, digest)
	return cas.SignedRequest{Method: op, URL: "https://storage.test/" + s.keyspace + "/" + digest, ExpiresAt: expiresAt}
}
func (s *grantSigner) Stat(_ context.Context, digest string) (cas.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.stored[digest]
	if !ok {
		return cas.Object{}, os.ErrNotExist
	}
	return cas.Object{Digest: d.Digest, SizeBytes: d.SizeBytes, MediaType: d.MediaType}, nil
}
func (s *grantSigner) SignGet(_ context.Context, digest string, expiresAt time.Time) (cas.SignedRequest, error) {
	return s.request("GET", digest, expiresAt), nil
}
func (s *grantSigner) SignHead(_ context.Context, digest string, expiresAt time.Time) (cas.SignedRequest, error) {
	return s.request("HEAD", digest, expiresAt), nil
}
func (s *grantSigner) StatDescriptor(ctx context.Context, expected cas.Descriptor) (cas.Object, error) {
	object, err := s.Stat(ctx, expected.Digest)
	if err == nil && cas.RequireExact(object, expected) != nil {
		err = cas.ErrRemoteIntegrity
	}
	return object, err
}
func (s *grantSigner) UploadLayout(expected cas.Descriptor) (cas.Upload, bool) {
	if expected.SizeBytes < 64<<20 {
		return cas.Upload{}, false
	}
	return cas.Upload{PartSize: 64 << 20, Parts: int32((expected.SizeBytes + 64<<20 - 1) / (64 << 20))}, true
}
func (s *grantSigner) SignPut(_ context.Context, expected cas.Descriptor, expiresAt time.Time) (cas.SignedRequest, error) {
	return s.request("PUT", expected.Digest, expiresAt), nil
}
func (s *grantSigner) CreateUpload(_ context.Context, owner string, expected cas.Descriptor) (cas.Upload, error) {
	s.record("CREATE", expected.Digest)
	upload, _ := s.UploadLayout(expected)
	upload.ID, upload.Owner = "upload-"+expected.Digest[len(expected.Digest)-8:], owner
	return upload, nil
}
func (s *grantSigner) SignPart(_ context.Context, expected cas.Descriptor, upload cas.Upload, part int32, expiresAt time.Time) (cas.SignedRequest, error) {
	if part < 1 || part > upload.Parts {
		return cas.SignedRequest{}, errors.New("part outside upload")
	}
	return s.request("PART", expected.Digest, expiresAt), nil
}
func (s *grantSigner) CompleteUpload(_ context.Context, expected cas.Descriptor, _ cas.Upload, _ []cas.CompletedPart) (cas.Object, error) {
	s.record("COMPLETE", expected.Digest)
	if s.integrity != nil {
		return cas.Object{}, s.integrity
	}
	s.mu.Lock()
	s.stored[expected.Digest] = expected
	s.mu.Unlock()
	return cas.Object{Digest: expected.Digest, SizeBytes: expected.SizeBytes, MediaType: expected.MediaType}, nil
}
func (s *grantSigner) AbortUpload(_ context.Context, upload cas.Upload, digest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aborted = append(s.aborted, upload.Owner+"/"+upload.ID)
	return nil
}

type grantFixture struct {
	fixture
	broker    *StorageBroker
	keyspaces *grantKeyspaces
	// signer is the keyspace of the fixture's organization.
	signer *grantSigner
	org    uuid.UUID
	host   workergroup.HostPrincipal
	lease  LeaseIdentity
	own    string
}

func newGrantFixture(t *testing.T) grantFixture {
	t.Helper()
	f := newFixture(t)
	var org uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT org_id FROM environments WHERE id=$1`, f.env).Scan(&org); err != nil {
		t.Fatal(err)
	}
	keyspaces := &grantKeyspaces{orgs: map[uuid.UUID]*grantSigner{}}
	broker, err := NewStorageBroker(f.pool, keyspaces.signer, newGrantSigner("platform"))
	if err != nil {
		t.Fatal(err)
	}
	var own string
	if err := f.pool.QueryRow(t.Context(), `SELECT digest FROM disk_objects WHERE environment_id=$1 AND certified`, f.env).Scan(&own); err != nil {
		t.Fatal(err)
	}
	// The lease mounts the Computer's initial root, which roots its disk graph.
	mountInitialRoot(t, f, f.computer)
	return grantFixture{fixture: f, broker: broker, keyspaces: keyspaces, signer: keyspaces.of(org), org: org, own: own,
		host:  workergroup.HostPrincipal{HostID: f.base.Worker, GroupID: f.base.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1},
		lease: LeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, InstanceID: f.computer, Epoch: 1}}
}

// mountInitialRoot binds computer's lease to its initial root with its own write key.
func mountInitialRoot(t *testing.T, f fixture, computer uuid.UUID) {
	t.Helper()
	key := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO disk_encryption_keys(id,environment_id,writer_computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'test','\x01');
 UPDATE computer_leases l SET base_root_id=c.initial_root_id,write_key_id=$1 FROM computers c WHERE (c.environment_id,c.id)=(l.environment_id,l.computer_id) AND l.environment_id=$2 AND l.computer_id=$3`,
		pgx.QueryExecModeSimpleProtocol, key, f.env, computer)
}

func (g grantFixture) scope() StorageScope { lease := g.lease; return StorageScope{Computer: &lease} }

func digestOf(fill string) string { return "sha256:" + strings.Repeat(fill, 64) }

// otherOrganizationObject stores digest in another organization's keyspace and
// environment graph, and returns that organization.
func (g grantFixture) otherOrganizationObject(t *testing.T, digest string) uuid.UUID {
	t.Helper()
	org, project, env := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), g.pool, `INSERT INTO organizations(id,name,slug) VALUES($1,'Other','other');
 INSERT INTO projects(id,org_id,default_region_id,slug,name) VALUES($2,$1,'test','other','Other');
 INSERT INTO environments(id,org_id,project_id,slug,name,color_hex,admission_tokens,admission_refilled_at) VALUES($3,$1,$2,'other','Other','#112233',1,clock_timestamp());
 INSERT INTO cas_blobs(org_id,digest,size_bytes) VALUES($1,$4,7);
 INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$4,7,'application/octet-stream');
 INSERT INTO disk_objects(environment_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at) VALUES($3,$4,$1,$2,7,'application/octet-stream','segment',0,'{}',clock_timestamp())`,
		pgx.QueryExecModeSimpleProtocol, org, project, env, digest)
	g.keyspaces.of(org).stored[digest] = cas.Descriptor{Digest: digest, SizeBytes: 7, MediaType: "application/octet-stream"}
	return org
}

// sameOrgObject stores digest as a certified disk object of the fixture's own
// environment that no root of the fixture Computer reaches.
func (g grantFixture) sameOrgObject(t *testing.T, digest string) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), g.pool, `INSERT INTO cas_blobs(org_id,digest,size_bytes) SELECT org_id,$2,7 FROM environments WHERE id=$1;
 INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT org_id,$2,7,'application/octet-stream' FROM environments WHERE id=$1;
 INSERT INTO disk_objects(environment_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection,certified_at) SELECT id,$2,org_id,project_id,7,'application/octet-stream','segment',0,'{}',clock_timestamp() FROM environments WHERE id=$1`,
		pgx.QueryExecModeSimpleProtocol, g.env, digest)
}

// Reads cover only the allocation's own object graph within its organization;
// a digest stored for another organization, or for another Computer of the same
// environment, is indistinguishable from a missing one.
func TestStorageReadsRequireOwnGraph(t *testing.T) {
	g := newGrantFixture(t)
	grant, err := g.broker.Read(t.Context(), g.host, g.scope(), "cas", g.own)
	if err != nil || grant.Digest != g.own || grant.Get.Method != "GET" || grant.Head.Method != "HEAD" {
		t.Fatalf("own read = %+v, %v", grant, err)
	}
	if limit := time.Now().Add(StorageGrantLifetime + time.Second); !grant.Get.ExpiresAt.Before(limit) {
		t.Fatalf("grant outlives its lifetime: %v", grant.Get.ExpiresAt)
	}
	foreign, unowned, sibling := digestOf("a"), digestOf("b"), digestOf("c")
	g.otherOrganizationObject(t, foreign)
	dbtest.MustExec(t, t.Context(), g.pool, `INSERT INTO cas_blobs(org_id,digest,size_bytes) VALUES($1,$2,7)`, g.org, unowned)
	g.sameOrgObject(t, sibling)
	before := len(g.signer.calls())
	for name, digest := range map[string]string{"foreign": foreign, "blob only": unowned, "same environment, other graph": sibling, "unknown": digestOf("d")} {
		if _, err := g.broker.Read(t.Context(), g.host, g.scope(), "cas", digest); !errors.Is(err, ErrDenied) {
			t.Fatalf("%s read = %v", name, err)
		}
	}
	if after := len(g.signer.calls()); after != before {
		t.Fatalf("denied reads were signed: %v", g.signer.calls()[before:])
	}
	if _, err := g.broker.Read(t.Context(), g.host, g.scope(), "cas", "sha256:../../other"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("malformed digest = %v", err)
	}
	if !strings.HasPrefix(grant.Get.URL, "https://storage.test/orgs/"+g.org.String()+"/") {
		t.Fatalf("read signed outside the organization's keyspace: %s", grant.Get.URL)
	}
}

// Grants end with their authority: a fenced, expired or superseded allocation,
// or another Host epoch, receives nothing, and a grant never outlives its lease.
func TestStorageGrantsEndWithAuthority(t *testing.T) {
	g := newGrantFixture(t)
	dbtest.MustExec(t, t.Context(), g.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()+interval '30 seconds' WHERE environment_id=$1 AND computer_id=$2`, g.env, g.computer)
	grant, err := g.broker.Read(t.Context(), g.host, g.scope(), "cas", g.own)
	if err != nil || !grant.Get.ExpiresAt.Before(time.Now().Add(31*time.Second)) {
		t.Fatalf("grant ignores lease expiry: %+v, %v", grant, err)
	}
	other := g.host
	other.Epoch = 2
	if _, err := g.broker.Read(t.Context(), other, g.scope(), "cas", g.own); !errors.Is(err, ErrDenied) {
		t.Fatalf("other Host epoch = %v", err)
	}
	stale := g.scope()
	stale.Computer.Epoch = 2
	if _, err := g.broker.Write(t.Context(), g.host, stale, cas.Descriptor{Digest: digestOf("d"), SizeBytes: 7, MediaType: "application/octet-stream"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("other lease epoch write = %v", err)
	}
	dbtest.MustExec(t, t.Context(), g.pool, `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='test fence' WHERE environment_id=$1 AND computer_id=$2`, g.env, g.computer)
	if _, err := g.broker.Read(t.Context(), g.host, g.scope(), "cas", g.own); !errors.Is(err, ErrDenied) {
		t.Fatalf("read after fence = %v", err)
	}
	if _, err := g.broker.Write(t.Context(), g.host, g.scope(), cas.Descriptor{Digest: digestOf("d"), SizeBytes: 7, MediaType: "application/octet-stream"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("write after fence = %v", err)
	}
}

// Writes are create-only receipts of one allocation in its organization's
// keyspace. The same allocation may read back its upload; another allocation
// gets nothing, and an existing object is verified rather than re-signed.
func TestStorageWritesRecordUploadReceipts(t *testing.T) {
	g := newGrantFixture(t)
	fresh := cas.Descriptor{Digest: digestOf("e"), SizeBytes: 7, MediaType: cas.CheckpointVMStateMediaType}
	grant, err := g.broker.Write(t.Context(), g.host, g.scope(), fresh)
	if err != nil || grant.Put == nil || grant.Put.Method != "PUT" || grant.Exists || grant.Upload != nil {
		t.Fatalf("write = %+v, %v", grant, err)
	}
	if !strings.HasPrefix(grant.Put.URL, "https://storage.test/orgs/"+g.org.String()+"/") {
		t.Fatalf("write signed outside the organization's keyspace: %s", grant.Put.URL)
	}
	if _, err := g.broker.Read(t.Context(), g.host, g.scope(), "cas", fresh.Digest); err != nil {
		t.Fatalf("own upload read back = %v", err)
	}
	if _, err := g.broker.Write(t.Context(), g.host, g.scope(), cas.Descriptor{Digest: fresh.Digest, SizeBytes: 8, MediaType: fresh.MediaType}); !errors.Is(err, ErrConflict) {
		t.Fatalf("descriptor change = %v", err)
	}
	var lifetime int
	if err := g.pool.QueryRow(t.Context(), `SELECT count(*) FROM cas_blobs WHERE org_id=$1 AND digest=$2 AND retired_at IS NULL`, g.org, fresh.Digest).Scan(&lifetime); err != nil || lifetime != 1 {
		t.Fatalf("fresh upload lifetime = %d, %v", lifetime, err)
	}
	// Certification releases the finished receipt: the read back ends with it.
	if err := db.RunTx(t.Context(), g.pool, func(tx pgx.Tx) error { return releaseFinishedUploads(t.Context(), tx, g.org, fresh.Digest) }); err != nil {
		t.Fatal(err)
	}
	if _, err := g.broker.Read(t.Context(), g.host, g.scope(), "cas", fresh.Digest); !errors.Is(err, ErrDenied) {
		t.Fatalf("read back after certification released the receipt = %v", err)
	}
	// Finding an object already stored, correct or not, records no receipt and
	// widens no read: a later read still requires the object in the graph.
	sibling := digestOf("7")
	g.sameOrgObject(t, sibling)
	g.signer.stored[sibling] = cas.Descriptor{Digest: sibling, SizeBytes: 7, MediaType: "application/octet-stream"}
	for size, want := range map[int64]error{7: nil, 8: ErrConflict} {
		grant, err := g.broker.Write(t.Context(), g.host, g.scope(), cas.Descriptor{Digest: sibling, SizeBytes: size, MediaType: "application/octet-stream"})
		if !errors.Is(err, want) || (want == nil && (!grant.Exists || grant.Put != nil)) {
			t.Fatalf("write of a stored object of size %d = %+v, %v", size, grant, err)
		}
		if _, err := g.broker.Read(t.Context(), g.host, g.scope(), "cas", sibling); !errors.Is(err, ErrDenied) {
			t.Fatalf("write request of size %d minted a read = %v", size, err)
		}
	}
	var receipts int
	if err := g.pool.QueryRow(t.Context(), `SELECT count(*) FROM allocation_upload_receipts WHERE digest=$1`, sibling).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatalf("finding a stored object recorded %d receipts, %v", receipts, err)
	}
	g.signer.stored[g.own] = cas.Descriptor{Digest: g.own, SizeBytes: 7, MediaType: "application/octet-stream"}
	if grant, err := g.broker.Write(t.Context(), g.host, g.scope(), cas.Descriptor{Digest: g.own, SizeBytes: 7, MediaType: "application/octet-stream"}); err != nil || !grant.Exists || grant.Put != nil {
		t.Fatalf("existing own object = %+v, %v", grant, err)
	}
	g.signer.stored[g.own] = cas.Descriptor{Digest: g.own, SizeBytes: 9, MediaType: "application/octet-stream"}
	if _, err := g.broker.Write(t.Context(), g.host, g.scope(), cas.Descriptor{Digest: g.own, SizeBytes: 7, MediaType: "application/octet-stream"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatched stored object = %v", err)
	}
	source := StorageScope{PreparationSource: &PreparationKey{EnvironmentID: g.env, BuildID: uuid.NewV7()}}
	if _, err := g.broker.Write(t.Context(), g.host, source, fresh); err == nil {
		t.Fatal("preparation source wrote storage")
	}
	// A second allocation of the same Host cannot read the first one's upload.
	upload := cas.Descriptor{Digest: digestOf("8"), SizeBytes: 7, MediaType: "application/octet-stream"}
	if _, err := g.broker.Write(t.Context(), g.host, g.scope(), upload); err != nil {
		t.Fatal(err)
	}
	second := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), g.pool, `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes)
 SELECT environment_id,$2,initial_root_id,initial_root_digest,origin_template_id,origin_deployment_id,cpu_millis,memory_bytes,disk_bytes,storage_reservation_bytes FROM computers WHERE environment_id=$1 AND id=$3;
 INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,status,allocation_id,channel_credential_digest,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at)
 SELECT environment_id,$2,1,worker_host_id,worker_epoch,expires_at,status,$2,channel_credential_digest,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM computer_leases WHERE environment_id=$1 AND computer_id=$3`, pgx.QueryExecModeSimpleProtocol, g.env, second, g.computer)
	mountInitialRoot(t, g.fixture, second)
	other := StorageScope{Computer: &LeaseIdentity{EnvironmentID: g.env, ComputerID: second, InstanceID: second, Epoch: 1}}
	if _, err := g.broker.Read(t.Context(), g.host, other, "cas", g.own); err != nil {
		t.Fatalf("second allocation own graph = %v", err)
	}
	if _, err := g.broker.Read(t.Context(), g.host, other, "cas", upload.Digest); !errors.Is(err, ErrDenied) {
		t.Fatalf("second allocation read first allocation's upload = %v", err)
	}
}

// The same digest in two organizations is two independent objects: each is
// written, read and retired in its own keyspace, and neither organization's
// authority reaches the other's.
func TestStorageOrganizationsHoldIndependentObjects(t *testing.T) {
	g := newGrantFixture(t)
	shared := digestOf("f")
	other := g.otherOrganizationObject(t, shared)
	descriptor := cas.Descriptor{Digest: shared, SizeBytes: 7, MediaType: "application/octet-stream"}
	// Another organization holding the digest neither blocks this upload nor
	// stands in for it: this keyspace has no such object yet.
	grant, err := g.broker.Write(t.Context(), g.host, g.scope(), descriptor)
	if err != nil || grant.Put == nil || grant.Exists {
		t.Fatalf("write of a digest another organization holds = %+v, %v", grant, err)
	}
	if !strings.HasPrefix(grant.Put.URL, "https://storage.test/orgs/"+g.org.String()+"/") {
		t.Fatalf("write signed outside the organization's keyspace: %s", grant.Put.URL)
	}
	read, err := g.broker.Read(t.Context(), g.host, g.scope(), "cas", shared)
	if err != nil || !strings.HasPrefix(read.Get.URL, "https://storage.test/orgs/"+g.org.String()+"/") {
		t.Fatalf("own upload read = %+v, %v", read, err)
	}
	if calls := g.keyspaces.of(other).calls(); len(calls) != 0 {
		t.Fatalf("another organization's keyspace was signed: %v", calls)
	}
	var lifetimes int
	if err := g.pool.QueryRow(t.Context(), `SELECT count(DISTINCT org_id) FROM cas_blobs WHERE digest=$1 AND org_id IN ($2,$3)`, shared, g.org, other).Scan(&lifetimes); err != nil || lifetimes != 2 {
		t.Fatalf("lifetimes = %d, %v", lifetimes, err)
	}
	// Retiring one organization's abandoned lifetime leaves the other's.
	dbtest.MustExec(t, t.Context(), g.pool, `DELETE FROM allocation_upload_receipts WHERE org_id=$1 AND digest=$2`, g.org, shared)
	if n, err := db.New(g.pool).RetireAbandonedCasBlob(t.Context(), db.RetireAbandonedCasBlobParams{OrgID: pgvalue.UUID(g.org), Digest: shared}); err != nil || n != 1 {
		t.Fatalf("retire own lifetime = %d, %v", n, err)
	}
	var available bool
	if err := g.pool.QueryRow(t.Context(), `SELECT retired_at IS NULL FROM cas_blobs WHERE org_id=$1 AND digest=$2`, other, shared).Scan(&available); err != nil || !available {
		t.Fatalf("retirement crossed organizations: %v, %v", available, err)
	}
	// With its receipt gone and the object outside its graph, this
	// organization reads nothing, whatever the other one holds.
	if _, err := g.broker.Read(t.Context(), g.host, g.scope(), "cas", shared); !errors.Is(err, ErrDenied) {
		t.Fatalf("read of another organization's object = %v", err)
	}
}

// Multipart uploads stay owned by the allocation that created them; parts,
// completion and abort require that exact upload, and a completion whose bytes
// differ from the digest is refused.
func TestStorageMultipartIsAllocationOwned(t *testing.T) {
	g := newGrantFixture(t)
	large := cas.Descriptor{Digest: digestOf("9"), SizeBytes: 3*(64<<20) + 1, MediaType: cas.CheckpointMemoryMediaType}
	grant, err := g.broker.Write(t.Context(), g.host, g.scope(), large)
	if err != nil || grant.Upload == nil || grant.Upload.Parts != 4 || grant.Put != nil {
		t.Fatalf("multipart write = %+v, %v", grant, err)
	}
	upload := grant.Upload.ID
	// The organization certifying the same digest elsewhere does not cancel
	// this upload in progress.
	if err := db.RunTx(t.Context(), g.pool, func(tx pgx.Tx) error { return releaseFinishedUploads(t.Context(), tx, g.org, large.Digest) }); err != nil {
		t.Fatal(err)
	}
	if parts, err := g.broker.Parts(t.Context(), g.host, g.scope(), large, upload, []int32{1, 4}); err != nil || len(parts) != 2 {
		t.Fatalf("parts = %+v, %v", parts, err)
	}
	for name, test := range map[string]struct {
		upload string
		parts  []int32
		want   error
	}{"guessed upload": {"upload-other", []int32{1}, ErrDenied}, "outside upload": {upload, []int32{5}, ErrInvalidInput}, "unbounded": {upload, make([]int32, 33), ErrInvalidInput}} {
		if _, err := g.broker.Parts(t.Context(), g.host, g.scope(), large, test.upload, test.parts); !errors.Is(err, test.want) {
			t.Fatalf("%s = %v", name, err)
		}
	}
	completed := []cas.CompletedPart{{Number: 1, ETag: "a"}, {Number: 2, ETag: "b"}, {Number: 3, ETag: "c"}, {Number: 4, ETag: "d"}}
	if err := g.broker.Complete(t.Context(), g.host, g.scope(), large, "upload-other", completed); !errors.Is(err, ErrDenied) {
		t.Fatalf("complete foreign upload = %v", err)
	}
	if err := g.broker.Abort(t.Context(), g.host, g.scope(), large, "upload-other"); !errors.Is(err, ErrDenied) || len(g.signer.aborted) != 0 {
		t.Fatalf("abort foreign upload = %v, %v", err, g.signer.aborted)
	}
	g.signer.integrity = cas.ErrRemoteIntegrity
	if err := g.broker.Complete(t.Context(), g.host, g.scope(), large, upload, completed); !errors.Is(err, ErrConflict) {
		t.Fatalf("poisoned completion = %v", err)
	}
	g.signer.integrity = nil
	if err := g.broker.Complete(t.Context(), g.host, g.scope(), large, upload, completed); err != nil {
		t.Fatalf("completion = %v", err)
	}
	dbtest.MustExec(t, t.Context(), g.pool, `UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='test fence' WHERE environment_id=$1 AND computer_id=$2`, g.env, g.computer)
	if _, err := g.broker.Parts(t.Context(), g.host, g.scope(), large, upload, []int32{2}); !errors.Is(err, ErrDenied) {
		t.Fatalf("parts after fence = %v", err)
	}
}

// Checkpoint data keys are created only by the capturing allocation and are
// delivered to it and to the allocation restoring that checkpoint, nothing else.
func TestCheckpointKeysFollowCaptureAndRestore(t *testing.T) {
	g := newGrantFixture(t)
	wrapper, err := computerkey.NewLocal("test-root", make([]byte, computerkey.Size))
	if err != nil {
		t.Fatal(err)
	}
	keys, err := NewKeyBroker(g.pool, wrapper)
	if err != nil {
		t.Fatal(err)
	}
	save := g.requestedSave(t)
	checkpoint, other := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), g.pool, `INSERT INTO checkpoints(id,environment_id,computer_id,source_lease_epoch,control_version,disk_save_id,status,capture_request,capture_expires_at,capture_digest)
 VALUES($1,$2,$3,1,1,$4,'capturing','\x01',clock_timestamp()+interval '1 hour',decode(repeat('11',32),'hex'))`, checkpoint, g.env, g.computer, save)
	captured, err := keys.CheckpointKey(t.Context(), g.host, g.lease, checkpoint)
	if err != nil || len(captured) != computerkey.Size {
		t.Fatalf("capture key = %d bytes, %v", len(captured), err)
	}
	again, err := keys.CheckpointKey(t.Context(), g.host, g.lease, checkpoint)
	if err != nil || string(again) != string(captured) {
		t.Fatalf("capture key changed on retry: %v", err)
	}
	if _, err := keys.CheckpointKey(t.Context(), g.host, g.lease, other); !errors.Is(err, ErrDenied) {
		t.Fatalf("unknown checkpoint = %v", err)
	}
	stale := g.host
	stale.Epoch = 2
	if _, err := keys.CheckpointKey(t.Context(), stale, g.lease, checkpoint); !errors.Is(err, ErrDenied) {
		t.Fatalf("other Host epoch = %v", err)
	}
	// The source becomes ready and is fenced; a later lease restores it.
	dbtest.MustExec(t, t.Context(), g.pool, `UPDATE computer_saves SET status='published',flush_acknowledged_at=clock_timestamp(),captured_at=clock_timestamp(),
 captured_root_digest=decode(repeat('22',32),'hex'),capture_evidence='test',publication_evidence='test',published_at=clock_timestamp(),payload_retired_at=clock_timestamp() WHERE id=$1`, save)
	dbtest.MustExec(t, t.Context(), g.pool, `UPDATE checkpoints SET status='ready',manifest='\x01',vm_platform_id=(SELECT vm_platform_id FROM worker_hosts LIMIT 1),ready_at=clock_timestamp() WHERE id=$1;
 UPDATE computer_leases SET status='lost',fenced_at=clock_timestamp(),fence_evidence='test fence' WHERE environment_id=$2 AND computer_id=$3 AND epoch=1`, pgx.QueryExecModeSimpleProtocol, checkpoint, g.env, g.computer)
	if _, err := keys.CheckpointKey(t.Context(), g.host, g.lease, checkpoint); !errors.Is(err, ErrDenied) {
		t.Fatalf("fenced source = %v", err)
	}
	restore := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), g.pool, `INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,status,allocation_id,channel_credential_digest,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,restored_from_save_id)
 SELECT environment_id,computer_id,2,worker_host_id,worker_epoch,clock_timestamp()+interval '1 hour','acquiring',$3,channel_credential_digest,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,clock_timestamp(),$4 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1`, g.env, g.computer, restore, save)
	restored, err := keys.CheckpointKey(t.Context(), g.host, LeaseIdentity{EnvironmentID: g.env, ComputerID: g.computer, InstanceID: restore, Epoch: 2}, checkpoint)
	if err != nil || string(restored) != string(captured) {
		t.Fatalf("restore key = %v", err)
	}
}

// An image build reads only its deployment artifacts and its own uploads, never
// another Computer's disk in the same environment, and nothing after it ends.
func TestPreparationGrantsCoverOnlyBuildInputs(t *testing.T) {
	g := newGrantFixture(t)
	build, allocation := uuid.NewV7(), uuid.NewV7()
	credential := bytes.Repeat([]byte{21}, 32)
	digest := sha256.Sum256(credential)
	dbtest.MustExec(t, t.Context(), g.pool, `INSERT INTO image_builds(environment_id,id,image_spec_id,idempotency_key,status,deadline_at,executor_epoch,worker_host_id,worker_epoch,allocation_id,channel_credential_digest,executor_expires_at,delivered_at,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest)
 SELECT $1,$2,$3,'grants','running',clock_timestamp()+interval '10 minutes',1,id,1,$4,$5,clock_timestamp()+interval '1 minute',clock_timestamp(),536870912,1073741824,vm_platform_id,1,cpu_environment_digest FROM worker_hosts WHERE id=$6`,
		g.env, build, g.deployment, allocation, digest[:], g.base.Worker)
	scope := StorageScope{Preparation: &ImageBuildExecutor{EnvironmentID: g.env, ImageBuildID: build, InstanceID: allocation, Epoch: 1, ChannelCredential: credential}}
	artifact := digestOf("5")
	dbtest.MustExec(t, t.Context(), g.pool, `INSERT INTO cas_blobs(org_id,digest,size_bytes) SELECT org_id,$3,7 FROM environments WHERE id=$1;
 INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) SELECT org_id,$3,7,'application/octet-stream' FROM environments WHERE id=$1;
 INSERT INTO deployment_objects(environment_id,deployment_id,digest,org_id,project_id,size_bytes,media_type) SELECT id,$2,$3,org_id,project_id,7,'application/octet-stream' FROM environments WHERE id=$1`,
		pgx.QueryExecModeSimpleProtocol, g.env, g.deployment, artifact)
	if _, err := g.broker.Read(t.Context(), g.host, scope, "cas", artifact); err != nil {
		t.Fatalf("deployment artifact = %v", err)
	}
	if _, err := g.broker.Read(t.Context(), g.host, scope, "cas", g.own); !errors.Is(err, ErrDenied) {
		t.Fatalf("another Computer's disk = %v", err)
	}
	upload := cas.Descriptor{Digest: digestOf("6"), SizeBytes: 7, MediaType: "application/octet-stream"}
	if grant, err := g.broker.Write(t.Context(), g.host, scope, upload); err != nil || grant.Put == nil {
		t.Fatalf("write = %+v, %v", grant, err)
	}
	if _, err := g.broker.Read(t.Context(), g.host, scope, "cas", upload.Digest); err != nil {
		t.Fatalf("own upload = %v", err)
	}
	forged := *scope.Preparation
	forged.ChannelCredential = bytes.Repeat([]byte{1}, 32)
	if _, err := g.broker.Read(t.Context(), g.host, StorageScope{Preparation: &forged}, "cas", artifact); !errors.Is(err, ErrDenied) {
		t.Fatalf("forged executor credential = %v", err)
	}
	dbtest.MustExec(t, t.Context(), g.pool, `UPDATE image_builds SET status='failed',error_code='test',fenced_at=clock_timestamp(),fence_evidence='test fence' WHERE environment_id=$1 AND id=$2`, g.env, build)
	if _, err := g.broker.Read(t.Context(), g.host, scope, "cas", artifact); !errors.Is(err, ErrDenied) {
		t.Fatalf("read after the build ended = %v", err)
	}
}
