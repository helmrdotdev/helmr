package computer

import (
	"context"
	"errors"
	"os"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// StorageGrantLifetime bounds every storage grant. A grant is also bounded by
// its authority's own expiry. Signed requests cannot be revoked: a fence stops
// new grants at once, and grants already issued expire within this lifetime.
// Writes they permit are create-only and digest-bound, and publication into an
// organization's graph is decided later by certification under live authority.
const StorageGrantLifetime = 5 * time.Minute

// ReadSigner authorizes reads of one immutable object store.
type ReadSigner interface {
	Stat(ctx context.Context, digest string) (cas.Object, error)
	SignGet(ctx context.Context, digest string, expiresAt time.Time) (cas.SignedRequest, error)
	SignHead(ctx context.Context, digest string, expiresAt time.Time) (cas.SignedRequest, error)
}

// WriteSigner authorizes create-only writes into the content store. Multipart
// creation, completion, whole-object verification and abort stay with it.
type WriteSigner interface {
	ReadSigner
	VerifyDescriptor(ctx context.Context, expected cas.Descriptor) (cas.Object, error)
	// UploadLayout is the deterministic multipart layout of a descriptor, or
	// false when it is published by one request.
	UploadLayout(expected cas.Descriptor) (cas.Upload, bool)
	SignPut(ctx context.Context, expected cas.Descriptor, expiresAt time.Time) (cas.SignedRequest, error)
	CreateUpload(ctx context.Context, owner string, expected cas.Descriptor) (cas.Upload, error)
	SignPart(ctx context.Context, expected cas.Descriptor, upload cas.Upload, part int32, expiresAt time.Time) (cas.SignedRequest, error)
	CompleteUpload(ctx context.Context, expected cas.Descriptor, upload cas.Upload, parts []cas.CompletedPart) (cas.Object, error)
	AbortUpload(ctx context.Context, upload cas.Upload, digest string) error
}

// maxStorageParts bounds one part grant request; the worker wire contract
// batches requests to the same bound.
const maxStorageParts = 32

// StorageScope is the one authority a Host presents for storage grants.
type StorageScope struct {
	Computer          *LeaseIdentity
	Preparation       *ImageBuildExecutor
	BootSeed          *BootSeedExecutor
	PreparationSource *PreparationKey
}

// StorageReadGrant authorizes complete or ranged reads and metadata of one object.
type StorageReadGrant struct {
	Digest    string
	Get, Head cas.SignedRequest
}

// StorageWrite is exactly one of an existing verified object, a single
// create-only PUT, or an owned multipart upload.
type StorageWrite struct {
	Exists bool
	Put    *cas.SignedRequest
	Upload *cas.Upload
}

// StorageBroker issues short-lived storage grants to authenticated Hosts so
// that no Host holds storage credentials. Every grant addresses the keyspace of
// the authority's own organization, so the same digest of another organization
// is a different object. Reads cover only objects of the authority's
// environment graph, objects the same allocation was granted to upload, and
// released platform artifacts.
type StorageBroker struct {
	database db.TxBeginner
	objects  func(org uuid.UUID) WriteSigner
	platform ReadSigner
}

// NewStorageBroker takes the content store of each organization's keyspace and
// the release-only platform store.
func NewStorageBroker(database db.TxBeginner, objects func(org uuid.UUID) WriteSigner, platform ReadSigner) (*StorageBroker, error) {
	if database == nil || objects == nil || platform == nil {
		return nil, errors.New("storage grants require a database, a content store and a platform store")
	}
	return &StorageBroker{database: database, objects: objects, platform: platform}, nil
}

type storageAuthority struct {
	org, environment uuid.UUID
	// computer limits checkpoint reads to the Computer's own checkpoints.
	computer   uuid.UUID
	allocation uuid.UUID
	expiresAt  time.Time
	writable   bool
	hostID     uuid.UUID
	hostEpoch  int64
}

func (s StorageScope) valid() bool {
	n := 0
	if s.Computer != nil {
		n++
		if !s.Computer.Valid() {
			return false
		}
	}
	if s.Preparation != nil {
		n++
		if !s.Preparation.Valid() {
			return false
		}
	}
	if s.BootSeed != nil {
		n++
		if !s.BootSeed.Valid() {
			return false
		}
	}
	if s.PreparationSource != nil {
		n++
		if s.PreparationSource.EnvironmentID == uuid.Nil() || s.PreparationSource.BuildID == uuid.Nil() {
			return false
		}
	}
	return n == 1
}

// authorize resolves live authority under the Host supply gate. Absence and
// lack of authority are indistinguishable.
func (b *StorageBroker) authorize(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, scope StorageScope) (storageAuthority, error) {
	a := storageAuthority{hostID: host.HostID, hostEpoch: host.Epoch}
	if !scope.valid() {
		return a, ErrInvalidInput
	}
	if err := shortLockTimeout(ctx, tx); err != nil {
		return a, err
	}
	if err := LockHost(ctx, tx, host); err != nil {
		return a, err
	}
	var err error
	switch {
	case scope.Computer != nil:
		id := *scope.Computer
		a.environment, a.computer, a.allocation, a.writable = id.EnvironmentID, id.ComputerID, id.InstanceID, true
		err = tx.QueryRow(ctx, `SELECT e.org_id,l.expires_at FROM computer_leases l JOIN environments e ON e.id=l.environment_id
 WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3 AND l.allocation_id=$4 AND l.worker_host_id=$5 AND l.worker_epoch=$6
 AND l.status IN ('acquiring','active','releasing') AND l.delivered_at IS NOT NULL AND l.fenced_at IS NULL AND l.expires_at>clock_timestamp()`,
			id.EnvironmentID, id.ComputerID, id.Epoch, id.InstanceID, host.HostID, host.Epoch).Scan(&a.org, &a.expiresAt)
	case scope.Preparation != nil:
		ref := *scope.Preparation
		a.environment, a.allocation, a.writable = ref.EnvironmentID, ref.InstanceID, true
		if err = CheckImageBuildExecutor(ctx, tx, host, ref, true); err == nil {
			err = tx.QueryRow(ctx, `SELECT e.org_id,b.executor_expires_at FROM image_builds b JOIN environments e ON e.id=b.environment_id WHERE b.environment_id=$1 AND b.id=$2`,
				ref.EnvironmentID, ref.ImageBuildID).Scan(&a.org, &a.expiresAt)
		}
	case scope.BootSeed != nil:
		ref := *scope.BootSeed
		a.environment, a.allocation, a.writable = ref.EnvironmentID, ref.InstanceID, true
		if err = CheckBootSeedExecutor(ctx, tx, host, ref, true); err == nil {
			err = tx.QueryRow(ctx, `SELECT e.org_id,b.executor_expires_at FROM boot_seed_builds b JOIN environments e ON e.id=b.environment_id WHERE b.environment_id=$1 AND b.id=$2`,
				ref.EnvironmentID, ref.BuildID).Scan(&a.org, &a.expiresAt)
		}
	default:
		key := *scope.PreparationSource
		a.environment = key.EnvironmentID
		// The same eligibility and lifetime as the preparation source itself.
		err = tx.QueryRow(ctx, `SELECT e.org_id,clock_timestamp()+`+preparationSourceLifetimeSQL+preparationSourceJoinsSQL+`
 WHERE b.environment_id=$4 AND b.id=$5 AND `+eligiblePreparationSourceSQL, host.HostID, host.GroupID, host.Epoch, key.EnvironmentID, key.BuildID).Scan(&a.org, &a.expiresAt)
	}
	if err != nil {
		return a, hideMissing(err)
	}
	if limit := time.Now().Add(StorageGrantLifetime); limit.Before(a.expiresAt) {
		a.expiresAt = limit
	}
	if !time.Now().Before(a.expiresAt) {
		return a, ErrDenied
	}
	return a, nil
}

// diskClosureHead and diskClosureTail enclose a subquery of disk_roots ids in
// environment $2; together they hold when o.digest is reached from one of those
// roots. The walk goes upward from the requested object, so a read visits only
// that object's ancestors.
const (
	diskClosureHead = `EXISTS(WITH RECURSIVE ancestors(digest) AS (
  SELECT o.digest
  UNION SELECT e.parent_digest FROM disk_object_edges e JOIN ancestors a ON e.child_digest=a.digest WHERE e.environment_id=$2)
 SELECT 1 FROM ancestors JOIN disk_roots r ON r.pack_digest=ancestors.digest WHERE r.environment_id=$2 AND r.id IN (`
	diskClosureTail = `))`
)

// deploymentObjectSQL admits the environment's deployment artifacts, which
// every allocation of the environment may execute or seed from.
const deploymentObjectSQL = `EXISTS(SELECT 1 FROM deployment_objects d WHERE d.environment_id=$2 AND d.digest=o.digest)`

// Object graphs by authority. Parameters: $1 org, $2 environment, $3 digest,
// $4 owner (Computer, image build or boot seed build), $5 Computer lease epoch.
// Possession of a digest outside the graph grants nothing.
const (
	// A Computer reads the disk closure of its lease's base root and of the
	// saves this lease published, objects its pending saves registered, its own
	// checkpoints and the boot seed its lease selected.
	computerGraphSQL = diskClosureHead + `SELECT l.base_root_id FROM computer_leases l WHERE l.environment_id=$2 AND l.computer_id=$4 AND l.epoch=$5
 UNION SELECT s.root_id FROM computer_saves s WHERE s.environment_id=$2 AND s.computer_id=$4 AND s.computer_lease_epoch=$5 AND s.root_id IS NOT NULL` + diskClosureTail + `
 OR EXISTS(SELECT 1 FROM disk_object_pins p JOIN computer_saves s ON (s.environment_id,s.id)=(p.environment_id,p.save_id)
  WHERE p.environment_id=$2 AND p.digest=o.digest AND s.computer_id=$4 AND s.computer_lease_epoch=$5
  AND EXISTS(SELECT 1 FROM disk_objects d WHERE d.environment_id=$2 AND d.digest=o.digest AND d.certified))
 OR EXISTS(SELECT 1 FROM checkpoint_objects c JOIN checkpoints p ON (p.environment_id,p.id)=(c.environment_id,c.checkpoint_id)
  WHERE c.environment_id=$2 AND c.digest=o.digest AND p.computer_id=$4)
 OR EXISTS(SELECT 1 FROM boot_seed_objects s JOIN computer_leases l ON (l.environment_id,l.boot_seed_build_id)=(s.environment_id,s.boot_seed_build_id)
  WHERE l.environment_id=$2 AND l.computer_id=$4 AND l.epoch=$5 AND s.digest=o.digest)
 OR ` + deploymentObjectSQL
	// An image build starts from a deployment seed and reads the graph it registered.
	preparationGraphSQL = `EXISTS(SELECT 1 FROM disk_object_pins p WHERE p.environment_id=$2 AND p.image_build_id=$4 AND p.digest=o.digest
  AND EXISTS(SELECT 1 FROM disk_objects d WHERE d.environment_id=$2 AND d.digest=o.digest AND d.certified))
 OR ` + deploymentObjectSQL
	// A boot seed build and its preparation source start from the build's image
	// root and read the seed's own objects.
	bootSeedGraphSQL = diskClosureHead + `SELECT i.root_id FROM boot_seed_builds b JOIN images i ON (i.environment_id,i.id)=(b.environment_id,b.image_id) WHERE b.environment_id=$2 AND b.id=$4` + diskClosureTail + `
 OR EXISTS(SELECT 1 FROM boot_seed_objects s WHERE s.environment_id=$2 AND s.boot_seed_build_id=$4 AND s.digest=o.digest)
 OR ` + deploymentObjectSQL
)

// readGrantHead and readGrantTail enclose an object graph. Every parameter is
// typed, so each graph prepares whichever parameters it references.
const (
	readGrantHead = `SELECT EXISTS(SELECT 1 FROM cas_objects o WHERE o.org_id=$1::uuid AND o.digest=$3::text AND $4::uuid IS NOT NULL AND $5::bigint IS NOT NULL AND (`
	readGrantTail = `)) OR ` + ownReceiptSQL
)

// ownReceiptSQL admits an object this same allocation was granted to upload, so
// it can read its own upload back before certification. Parameters continue the
// graph's: $6 host, $7 host epoch, $8 allocation.
const ownReceiptSQL = `EXISTS(SELECT 1 FROM allocation_storage_writes w WHERE w.worker_host_id=$6 AND w.worker_epoch=$7 AND w.allocation_id=$8 AND w.org_id=$1 AND w.digest=$3)`

func (b *StorageBroker) readAuthority(ctx context.Context, host workergroup.HostPrincipal, scope StorageScope, store, digest string) (storageAuthority, error) {
	var a storageAuthority
	if !sha256sum.ValidDigest(digest) {
		return a, ErrInvalidInput
	}
	err := db.RunTx(ctx, b.database, func(tx pgx.Tx) error {
		var err error
		if a, err = b.authorize(ctx, tx, host, scope); err != nil {
			return err
		}
		if store == "platform" {
			return nil
		}
		// Possession of a digest grants nothing: the object must belong to the
		// authority's object graph, or have been granted for upload to this
		// same allocation.
		var authorized bool
		var row pgx.Row
		switch {
		case scope.Computer != nil:
			row = tx.QueryRow(ctx, readGrantHead+computerGraphSQL+readGrantTail, a.org, a.environment, digest, scope.Computer.ComputerID, scope.Computer.Epoch, a.hostID, a.hostEpoch, a.allocation)
		case scope.Preparation != nil:
			row = tx.QueryRow(ctx, readGrantHead+preparationGraphSQL+readGrantTail, a.org, a.environment, digest, scope.Preparation.ImageBuildID, int64(0), a.hostID, a.hostEpoch, a.allocation)
		case scope.BootSeed != nil:
			row = tx.QueryRow(ctx, readGrantHead+bootSeedGraphSQL+readGrantTail, a.org, a.environment, digest, scope.BootSeed.BuildID, int64(0), a.hostID, a.hostEpoch, a.allocation)
		default:
			row = tx.QueryRow(ctx, readGrantHead+bootSeedGraphSQL+readGrantTail, a.org, a.environment, digest, scope.PreparationSource.BuildID, int64(0), a.hostID, a.hostEpoch, a.allocation)
		}
		err = row.Scan(&authorized)
		if err == nil && !authorized {
			err = ErrDenied
		}
		return err
	})
	return a, notReadyOnLock(err)
}

// Read grants GET and HEAD of one object. Platform reads cover only objects
// present in the release-only platform store.
func (b *StorageBroker) Read(ctx context.Context, host workergroup.HostPrincipal, scope StorageScope, store, digest string) (StorageReadGrant, error) {
	if store != "cas" && store != "platform" {
		return StorageReadGrant{}, ErrInvalidInput
	}
	a, err := b.readAuthority(ctx, host, scope, store, digest)
	if err != nil {
		return StorageReadGrant{}, err
	}
	var signer ReadSigner = b.platform
	if store == "cas" {
		signer = b.objects(a.org)
	}
	if store == "platform" {
		if _, err := signer.Stat(ctx, digest); errors.Is(err, os.ErrNotExist) {
			return StorageReadGrant{}, ErrDenied
		} else if err != nil {
			return StorageReadGrant{}, SaveStorageUnavailable(err)
		}
	}
	grant := StorageReadGrant{Digest: digest}
	if grant.Get, err = signer.SignGet(ctx, digest, a.expiresAt); err != nil {
		return StorageReadGrant{}, SaveStorageUnavailable(err)
	}
	if grant.Head, err = signer.SignHead(ctx, digest, a.expiresAt); err != nil {
		return StorageReadGrant{}, SaveStorageUnavailable(err)
	}
	return grant, nil
}

// writeAuthority records a create-only write receipt for this allocation. A
// preparation source carries no write authority.
func (b *StorageBroker) writeAuthority(ctx context.Context, host workergroup.HostPrincipal, scope StorageScope, object cas.Descriptor, record func(tx pgx.Tx, a storageAuthority) error) (storageAuthority, error) {
	var a storageAuthority
	if err := cas.ValidateDescriptor(object); err != nil || object.SizeBytes == 0 {
		return a, ErrInvalidInput
	}
	err := db.RunTx(ctx, b.database, func(tx pgx.Tx) error {
		var err error
		if a, err = b.authorize(ctx, tx, host, scope); err != nil {
			return err
		}
		if !a.writable {
			return ErrDenied
		}
		return record(tx, a)
	})
	return a, notReadyOnLock(err)
}

// recordStorageWrite records the receipt of an upload this allocation is about
// to make. The receipt binds a multipart upload to this allocation, lets it read
// its own upload back before certification, and registers the upload's
// lifetime so an abandoned one is reclaimed once the reclaimer has removed
// receipts of ended allocations.
func recordStorageWrite(ctx context.Context, tx pgx.Tx, a storageAuthority, object cas.Descriptor) error {
	var available bool
	var size int64
	if _, err := tx.Exec(ctx, `INSERT INTO cas_blobs(org_id,digest,size_bytes) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, a.org, object.Digest, object.SizeBytes); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT size_bytes,not_retired FROM cas_blobs WHERE org_id=$1 AND digest=$2 FOR KEY SHARE`, a.org, object.Digest).Scan(&size, &available); err != nil {
		return err
	}
	if size != object.SizeBytes || !available {
		return ErrConflict
	}
	tag, err := tx.Exec(ctx, `INSERT INTO allocation_storage_writes(worker_host_id,worker_epoch,allocation_id,org_id,digest,size_bytes,media_type) VALUES($1,$2,$3,$4,$5,$6,$7)
 ON CONFLICT (worker_host_id,allocation_id,digest) DO UPDATE SET upload_id=allocation_storage_writes.upload_id
 WHERE allocation_storage_writes.worker_epoch=EXCLUDED.worker_epoch AND allocation_storage_writes.org_id=EXCLUDED.org_id
 AND allocation_storage_writes.size_bytes=EXCLUDED.size_bytes AND allocation_storage_writes.media_type=EXCLUDED.media_type`,
		a.hostID, a.hostEpoch, a.allocation, a.org, object.Digest, object.SizeBytes, object.MediaType)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

// releaseFinishedUploads removes an organization's finished upload receipts for
// a digest once certification admits it into the organization's CAS
// membership. Membership now carries the object; a lingering receipt would only
// keep a released payload from reclamation. A multipart upload still in
// progress keeps its receipt until it completes or its allocation ends.
func releaseFinishedUploads(ctx context.Context, tx pgx.Tx, org uuid.UUID, digest string) error {
	_, err := tx.Exec(ctx, `DELETE FROM allocation_storage_writes WHERE org_id=$1 AND digest=$2 AND upload_id IS NULL`, org, digest)
	return err
}

// Write grants one create-only publication of exactly the descriptor's bytes
// into the organization's keyspace. An object already stored is verified rather
// than replaced. Finding it records no receipt, so it grants no read: a later
// read still requires the object to be in the authority's graph. Save and
// checkpoint publishers register membership before uploading, which keeps the
// object's lifetime available through certification. A request that fails or only
// finds a stored object widens no access.
func (b *StorageBroker) Write(ctx context.Context, host workergroup.HostPrincipal, scope StorageScope, object cas.Descriptor) (StorageWrite, error) {
	a, err := b.writeAuthority(ctx, host, scope, object, func(pgx.Tx, storageAuthority) error { return nil })
	if err != nil {
		return StorageWrite{}, err
	}
	objects := b.objects(a.org)
	switch _, err := objects.VerifyDescriptor(ctx, object); {
	case err == nil:
		return StorageWrite{Exists: true}, nil
	case errors.Is(err, cas.ErrRemoteIntegrity):
		return StorageWrite{}, errors.Join(ErrConflict, err)
	case !errors.Is(err, os.ErrNotExist):
		return StorageWrite{}, SaveStorageUnavailable(err)
	}
	a, err = b.writeAuthority(ctx, host, scope, object, func(tx pgx.Tx, a storageAuthority) error { return recordStorageWrite(ctx, tx, a, object) })
	if err != nil {
		return StorageWrite{}, err
	}
	if _, multipart := objects.UploadLayout(object); !multipart {
		put, err := objects.SignPut(ctx, object, a.expiresAt)
		if err != nil {
			return StorageWrite{}, err
		}
		return StorageWrite{Put: &put}, nil
	}
	upload, err := objects.CreateUpload(ctx, a.allocation.String(), object)
	if err != nil {
		return StorageWrite{}, SaveStorageUnavailable(err)
	}
	_, err = b.writeAuthority(ctx, host, scope, object, func(tx pgx.Tx, a storageAuthority) error {
		tag, err := tx.Exec(ctx, `UPDATE allocation_storage_writes SET upload_id=$5 WHERE worker_host_id=$1 AND worker_epoch=$2 AND allocation_id=$3 AND digest=$4`, a.hostID, a.hostEpoch, a.allocation, object.Digest, upload.ID)
		if err == nil && tag.RowsAffected() != 1 {
			err = ErrDenied
		}
		return err
	})
	if err != nil {
		return StorageWrite{}, errors.Join(err, objects.AbortUpload(context.WithoutCancel(ctx), upload, object.Digest))
	}
	return StorageWrite{Upload: &upload}, nil
}

// ownedUpload requires the upload to be the one recorded for this allocation's
// write of the exact descriptor, and returns the store of the authority's
// organization that holds it.
func (b *StorageBroker) ownedUpload(ctx context.Context, host workergroup.HostPrincipal, scope StorageScope, object cas.Descriptor, uploadID string) (storageAuthority, WriteSigner, cas.Upload, error) {
	if uploadID == "" {
		return storageAuthority{}, nil, cas.Upload{}, ErrInvalidInput
	}
	a, err := b.writeAuthority(ctx, host, scope, object, func(tx pgx.Tx, a storageAuthority) error {
		var owned bool
		err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM allocation_storage_writes WHERE worker_host_id=$1 AND worker_epoch=$2 AND allocation_id=$3 AND digest=$4 AND size_bytes=$5 AND media_type=$6 AND upload_id=$7)`,
			a.hostID, a.hostEpoch, a.allocation, object.Digest, object.SizeBytes, object.MediaType, uploadID).Scan(&owned)
		if err == nil && !owned {
			err = ErrDenied
		}
		return err
	})
	if err != nil {
		return a, nil, cas.Upload{}, err
	}
	objects := b.objects(a.org)
	upload, multipart := objects.UploadLayout(object)
	if !multipart {
		return a, nil, upload, ErrInvalidInput
	}
	upload.ID, upload.Owner = uploadID, a.allocation.String()
	return a, objects, upload, nil
}

// Parts grants bounded part uploads of an owned multipart publication. A
// renewed request re-signs the same parts.
func (b *StorageBroker) Parts(ctx context.Context, host workergroup.HostPrincipal, scope StorageScope, object cas.Descriptor, uploadID string, parts []int32) ([]cas.SignedRequest, error) {
	if len(parts) == 0 || len(parts) > maxStorageParts {
		return nil, ErrInvalidInput
	}
	a, objects, upload, err := b.ownedUpload(ctx, host, scope, object, uploadID)
	if err != nil {
		return nil, err
	}
	grants := make([]cas.SignedRequest, len(parts))
	for i, part := range parts {
		if part < 1 || part > upload.Parts {
			return nil, ErrInvalidInput
		}
		if grants[i], err = objects.SignPart(ctx, object, upload, part, a.expiresAt); err != nil {
			return nil, SaveStorageUnavailable(err)
		}
	}
	return grants, nil
}

// Complete publishes an owned multipart upload create-only and authenticates
// the whole object before acknowledging it.
func (b *StorageBroker) Complete(ctx context.Context, host workergroup.HostPrincipal, scope StorageScope, object cas.Descriptor, uploadID string, parts []cas.CompletedPart) error {
	_, objects, upload, err := b.ownedUpload(ctx, host, scope, object, uploadID)
	if err != nil {
		return err
	}
	if len(parts) != int(upload.Parts) {
		return ErrInvalidInput
	}
	if _, err := objects.CompleteUpload(ctx, object, upload, parts); err != nil {
		if errors.Is(err, cas.ErrRemoteIntegrity) {
			return errors.Join(ErrConflict, err)
		}
		return SaveStorageUnavailable(err)
	}
	// The published object no longer depends on the upload.
	_, err = b.writeAuthority(ctx, host, scope, object, func(tx pgx.Tx, a storageAuthority) error {
		_, err := tx.Exec(ctx, `UPDATE allocation_storage_writes SET upload_id=NULL WHERE worker_host_id=$1 AND worker_epoch=$2 AND allocation_id=$3 AND digest=$4 AND upload_id=$5`, a.hostID, a.hostEpoch, a.allocation, object.Digest, uploadID)
		return err
	})
	return err
}

// Abort releases an owned multipart upload.
func (b *StorageBroker) Abort(ctx context.Context, host workergroup.HostPrincipal, scope StorageScope, object cas.Descriptor, uploadID string) error {
	_, objects, upload, err := b.ownedUpload(ctx, host, scope, object, uploadID)
	if err != nil {
		return err
	}
	if err := objects.AbortUpload(ctx, upload, object.Digest); err != nil {
		return SaveStorageUnavailable(err)
	}
	return nil
}
