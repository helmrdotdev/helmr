package computer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ObjectStore confirms the stored bytes of an uploaded object.
type ObjectStore interface {
	Stat(ctx context.Context, digest string) (cas.Object, error)
}

// Publisher records a worker host's disk objects and publishes the disk
// versions they compose: the initial version of an Instance, the objects of
// a capture checkpoint and the saves of an Instance. Each operation fences
// its own authority. Object storage is confirmed outside any transaction and
// the authority is rechecked afterwards; a fence that no longer holds reports
// ErrAuthorityChanged.
type Publisher struct {
	db      db.TxDB
	objects ObjectStore
}

// NewPublisher returns a publisher over the database and object storage.
func NewPublisher(database db.TxDB, objects ObjectStore) (Publisher, error) {
	if database == nil || objects == nil {
		return Publisher{}, errors.New("computer publication database and object storage are required")
	}
	return Publisher{db: database, objects: objects}, nil
}

// Publication identifies a published disk version of a Computer.
type Publication struct {
	ComputerID uuid.UUID
	VersionID  uuid.UUID
}

func publicationOf(computerID, versionID pgtype.UUID) Publication {
	return Publication{ComputerID: pgvalue.MustUUIDValue(computerID), VersionID: pgvalue.MustUUIDValue(versionID)}
}

// objects is the initial preparation's object scope: its initial
// publication key and the Instance's pinned write key, the only key an
// initial version may use.
func (p initialPreparation) objects(ctx context.Context) (objectScope, error) {
	q := db.New(p.tx)
	runtimeID := p.instance.ID
	key, err := q.GetRuntimeComputerWriteKey(ctx, db.GetRuntimeComputerWriteKeyParams{ComputerInstanceID: runtimeID, EnvironmentID: p.environmentID, ComputerID: p.computerID})
	if err != nil {
		return objectScope{}, err
	}
	var pinned bool
	if err = p.tx.QueryRow(ctx, `SELECT write_key_id=$2 FROM computer_instances WHERE id=$1`, runtimeID, key.ID).Scan(&pinned); err != nil {
		return objectScope{}, err
	}
	if !pinned {
		return objectScope{}, objectConflict("initial writer key is not pinned")
	}
	return objectScope{
		objectRetention: objectRetention{environmentID: p.environmentID, computerID: p.computerID, instanceID: runtimeID, desiredVersion: p.instance.DesiredVersion, key: initialPublicationKey(pgvalue.MustUUIDValue(runtimeID))},
		orgID:           p.orgID, projectID: p.projectID, logicalBytes: p.logicalBytes, allowedKeys: map[string]bool{pgvalue.UUIDString(key.ID): true},
	}, nil
}

// RegisterInitialObject registers an initial preparation's disk object before
// its upload. The worker host may upload only after registration succeeds.
// No version head is advanced.
func (p Publisher) RegisterInitialObject(ctx context.Context, principal workergroup.HostPrincipal, ref PreparationRef, inspection blockformat.ObjectInspection) error {
	if _, err := describeObject(inspection); err != nil {
		return err
	}
	return p.inInitialPreparation(ctx, principal, ref, func(tx pgx.Tx, scope objectScope) error {
		return scope.registerObject(ctx, tx, inspection)
	})
}

// CertifyInitialObject certifies a registered initial object after its
// upload. A registration of this Instance's Computer must exist before object
// storage is consulted; storage confirms the exact bytes outside any
// transaction, and the recording transaction revalidates the whole
// preparation. A storage failure reports ErrStorageUnavailable.
func (p Publisher) CertifyInitialObject(ctx context.Context, principal workergroup.HostPrincipal, ref PreparationRef, inspection blockformat.ObjectInspection) error {
	object, err := describeObject(inspection)
	if err != nil {
		return err
	}
	// Restrict storage lookup to an exact registration belonging to this
	// physical Instance's Computer. This is not a commit grant: the recording
	// transaction rechecks live preparation authority and exact facts.
	runtimeID := pgvalue.UUID(ref.InstanceID)
	registered, err := db.New(p.db).HasRegisteredInitialComputerObject(ctx, db.HasRegisteredInitialComputerObjectParams{RuntimeID: runtimeID, PublicationKey: initialPublicationKey(ref.InstanceID), WorkerID: pgvalue.UUID(principal.HostID), WorkerGroupID: pgvalue.UUID(principal.GroupID), WorkerEpoch: principal.Epoch, DesiredVersion: ref.DesiredVersion, Digest: object.digest, Inspection: object.encoded})
	if err != nil {
		return fmt.Errorf("read initial computer object registration: %w", err)
	}
	if !registered {
		return objectConflict("computer object registration is unavailable")
	}
	stored, err := p.objects.Stat(ctx, object.digest)
	if err != nil {
		return storageUnavailable(err)
	}
	if !object.uploadedMatches(stored) {
		return objectConflict("stored computer object differs from registration")
	}
	return p.inInitialPreparation(ctx, principal, ref, func(tx pgx.Tx, scope objectScope) error {
		return scope.certifyObject(ctx, tx, inspection, stored)
	})
}

// inInitialPreparation runs fn in one transaction under the initial
// preparation authority and its object scope, then rechecks the preparation
// deadlines.
func (p Publisher) inInitialPreparation(ctx context.Context, principal workergroup.HostPrincipal, ref PreparationRef, fn func(pgx.Tx, objectScope) error) error {
	err := db.RunTx(ctx, p.db, func(tx pgx.Tx) error {
		prepared, err := lockInitialPreparation(ctx, tx, principal, ref)
		if err != nil {
			return err
		}
		scope, err := prepared.objects(ctx)
		if err != nil {
			return err
		}
		if err = fn(tx, scope); err != nil {
			return err
		}
		return prepared.checkDeadlines(ctx)
	})
	return authorityChanged(err)
}

// InitialVersion is the initial disk version an Instance publishes: its
// certified root and the image configuration the Computer keeps.
type InitialVersion struct {
	Root   disk.GenerationRoot `json:"root"`
	Config oci.RuntimeConfig   `json:"config"`
}

// PublishInitialVersion publishes the initializing head of an initial
// preparation from its certified, retained root and pins it as the Instance's
// source in the same commit. It uploads nothing, releases no pins and grants
// no execution. An exact committed publication replays, before the
// transaction and again when the preparation fence no longer holds, since a
// competing exact publisher may have committed while the locks waited. A
// malformed root reports an InputError.
func (p Publisher) PublishInitialVersion(ctx context.Context, principal workergroup.HostPrincipal, ref PreparationRef, input InitialVersion) (Publication, error) {
	locator, err := input.Root.Locator(input.Root.LogicalBytes)
	if err != nil {
		return Publication{}, invalidInput("invalid computer generation root: %v", err)
	}
	canonical, err := json.Marshal(input)
	if err != nil {
		return Publication{}, fmt.Errorf("encode initial computer version: %w", err)
	}
	fingerprint := sha256.Sum256(canonical)
	runtimeID := pgvalue.UUID(ref.InstanceID)
	replay := func() (Publication, error) {
		v, err := db.New(p.db).GetWorkerInitialComputerDiskVersion(ctx, db.GetWorkerInitialComputerDiskVersionParams{ComputerInstanceID: runtimeID, DesiredVersion: pgtype.Int8{Int64: ref.DesiredVersion, Valid: true}, WorkerHostID: pgvalue.UUID(principal.HostID), WorkerGroupID: pgvalue.UUID(principal.GroupID), WorkerEpoch: principal.Epoch})
		if err != nil {
			return Publication{}, fmt.Errorf("read initial computer publication: %w", err)
		}
		if !bytes.Equal(v.PublicationRequestFingerprint, fingerprint[:]) {
			return Publication{}, objectConflict("initial Computer publication differs from committed request")
		}
		return publicationOf(v.ComputerID, v.ID), nil
	}
	if result, err := replay(); !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	var published Publication
	fenceFailed := false
	err = db.RunTx(ctx, p.db, func(tx pgx.Tx) error {
		fence, err := lockInitialFence(ctx, tx, principal, ref)
		if err != nil {
			fenceFailed = true
			return err
		}
		if input.Root.LogicalBytes != fence.logicalBytes {
			return objectConflict("initial root capacity differs from preparation")
		}
		prepared, err := fence.claim(ctx, principal)
		if err != nil {
			return err
		}
		published, err = prepared.publishVersion(ctx, input, locator, fingerprint[:])
		return err
	})
	if err != nil {
		if fenceFailed {
			if result, replayErr := replay(); !errors.Is(replayErr, pgx.ErrNoRows) {
				return result, replayErr
			}
		}
		return Publication{}, authorityChanged(err)
	}
	return published, nil
}

// publishVersion publishes the initializing head from its certified root,
// retained under the initial publication key, and pins it as the Instance's
// source. Publication and source retention are one commit; otherwise the
// next source or key request has no retained root and the version could be
// reclaimed between publication and preparation.
func (p initialPreparation) publishVersion(ctx context.Context, input InitialVersion, locator blockformat.Locator, fingerprint []byte) (Publication, error) {
	q := db.New(p.tx)
	runtimeID := p.instance.ID
	object, err := q.LockComputerObject(ctx, db.LockComputerObjectParams{EnvironmentID: p.environmentID, ComputerID: p.computerID, Digest: input.Root.Pack.Digest})
	if errors.Is(err, pgx.ErrNoRows) {
		return Publication{}, objectConflict("initial root is not registered")
	}
	if err != nil {
		return Publication{}, fmt.Errorf("lock initial root object: %w", err)
	}
	if !object.Certified.Bool {
		return Publication{}, objectConflict("initial root is not certified")
	}
	var evidence blockformat.ObjectInspection
	if err = json.Unmarshal(object.Inspection, &evidence); err != nil {
		return Publication{}, fmt.Errorf("decode initial root inspection: %w", err)
	}
	if evidence.Pack == nil {
		return Publication{}, objectConflict("initial root is not an inspected pack")
	}
	if err = evidence.Pack.CheckRoot(locator, p.logicalBytes); err != nil {
		return Publication{}, objectConflict("initial root differs from its inspection: %v", err)
	}
	var retained bool
	if err = p.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_object_pins WHERE computer_instance_id=$1 AND publication_key=$4 AND instance_desired_version=$2 AND digest=$3)`, runtimeID, p.instance.DesiredVersion, input.Root.Pack.Digest, []byte(initialPublicationKey(pgvalue.MustUUIDValue(runtimeID)))).Scan(&retained); err != nil {
		return Publication{}, err
	}
	if !retained {
		return Publication{}, objectConflict("initial root is not retained by publisher")
	}
	rawRoot, err := json.Marshal(input.Root)
	if err != nil {
		return Publication{}, err
	}
	rawConfig, err := json.Marshal(input.Config)
	if err != nil {
		return Publication{}, err
	}
	version, err := q.PublishInitialComputerDiskVersion(ctx, db.PublishInitialComputerDiskVersionParams{EnvironmentID: p.environmentID, ComputerID: p.computerID, VersionID: p.versionID, ComputerInstanceID: runtimeID, DesiredVersion: pgtype.Int8{Int64: p.instance.DesiredVersion, Valid: true}, Fingerprint: fingerprint, RootPackDigest: pgvalue.Text(input.Root.Pack.Digest), LogicalBytes: p.logicalBytes, Locator: rawRoot, InitialConfig: rawConfig})
	if err != nil {
		return Publication{}, err
	}
	pinned, err := q.PinInstanceComputerSource(ctx, db.PinInstanceComputerSourceParams{ComputerInstanceID: runtimeID, EnvironmentID: p.environmentID, ComputerID: p.computerID, VersionID: version.ID})
	if err != nil {
		return Publication{}, err
	}
	if pinned != 1 {
		return Publication{}, objectConflict("initial generation has no matching Runtime source reservation")
	}
	if err = p.checkDeadlines(ctx); err != nil {
		return Publication{}, err
	}
	return publicationOf(version.ComputerID, version.ID), nil
}
