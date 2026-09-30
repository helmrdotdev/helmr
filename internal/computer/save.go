package computer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// SaveRef addresses one save of an Instance: the save the worker host names
// by its operation ID and sequence, at the writer generation it writes for.
type SaveRef struct {
	EnvironmentID    uuid.UUID
	InstanceID       uuid.UUID
	WriterGeneration int64
	SaveID           uuid.UUID
	Sequence         int64
}

func (r SaveRef) validate() error {
	if r.Sequence <= 0 || r.WriterGeneration <= 0 {
		return invalidInput("positive save sequence and writer generation required")
	}
	return nil
}

// receipt addresses the save's committed receipt on the principal's host
// epoch.
func (r SaveRef) receipt(principal workergroup.HostPrincipal) db.GetWorkerComputerSaveParams {
	return db.GetWorkerComputerSaveParams{
		EnvironmentID: pgvalue.UUID(r.EnvironmentID), ComputerInstanceID: pgvalue.UUID(r.InstanceID),
		SaveID: pgvalue.UUID(r.SaveID), Sequence: pgtype.Int8{Int64: r.Sequence, Valid: true},
		WorkerHostID: pgvalue.UUID(principal.HostID), WorkerGroupID: pgvalue.UUID(principal.GroupID),
		WorkerEpoch: principal.Epoch, WriterGeneration: r.WriterGeneration,
	}
}

// saveFingerprintRequest is the save request as a committed save's
// publication fingerprint encodes it. Its field names and tags are part of
// persisted fingerprints and must not change.
type saveFingerprintRequest struct {
	EnvironmentID      string `json:"environment_id"`
	ComputerInstanceID string `json:"computer_instance_id"`
	WriterGeneration   int64  `json:"writer_generation"`
	SaveID             string `json:"save_id"`
	Sequence           int64  `json:"sequence"`
}

// saveFingerprint is the persisted publication fingerprint of a save and its
// root.
func saveFingerprint(ref SaveRef, root disk.GenerationRoot) ([32]byte, error) {
	raw, err := json.Marshal(struct {
		Request saveFingerprintRequest
		Root    disk.GenerationRoot
	}{saveFingerprintRequest{EnvironmentID: ref.EnvironmentID.String(), ComputerInstanceID: ref.InstanceID.String(), WriterGeneration: ref.WriterGeneration, SaveID: ref.SaveID.String(), Sequence: ref.Sequence}, root})
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(raw), nil
}

// lockedSave is an Instance whose save writer fence holds in the owning
// transaction: the Computer's Secrets, the principal's Group and Host with
// current claims, the Computer and the Instance are locked, and the Instance
// is the ready, mounted writer the save names.
type lockedSave struct {
	tx        pgx.Tx
	principal workergroup.HostPrincipal
	ref       SaveRef
	computer  db.Computer
	instance  db.ComputerInstance
	// pending reports whether the Instance's save slot holds this save.
	pending bool
}

// saveBegin is the authority of a save's admission: the save holds the
// Instance's save slot, begun by this or an earlier admission.
type saveBegin struct {
	lockedSave
	predecessor pgtype.UUID
}

// unpublishedSave is the authority of a pending save that is not published:
// it records objects, publishes or is abandoned.
type unpublishedSave struct{ lockedSave }

// publishedSave is the authority of a pending save that is published: the
// worker host acknowledges its adoption.
type publishedSave struct{ lockedSave }

// lockSave locks the Computer's Secrets, which must be active, the
// principal's Group and Host with current claims, the Computer and then the
// Instance, and checks the save writer fence. Its caller validated the
// reference before any database access.
func lockSave(ctx context.Context, tx pgx.Tx, principal workergroup.HostPrincipal, ref SaveRef) (lockedSave, error) {
	params := ref.receipt(principal)
	q := db.New(tx)
	locator, err := q.GetComputerInstance(ctx, db.GetComputerInstanceParams{ID: params.ComputerInstanceID, EnvironmentID: params.EnvironmentID})
	if err != nil {
		return lockedSave{}, err
	}
	bindings, err := q.LockComputerSecretsForAdmission(ctx, locator.ComputerID)
	if err != nil {
		return lockedSave{}, err
	}
	for _, binding := range bindings {
		if binding.SecretStatus != "active" || !binding.CurrentVersionID.Valid {
			return lockedSave{}, fmt.Errorf("%w: %s", pgx.ErrNoRows, "computer secrets revoked")
		}
	}
	if _, err = workergroup.LockHost(ctx, q, principal); err != nil {
		return lockedSave{}, err
	}
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: params.EnvironmentID, ID: locator.ComputerID})
	if err != nil {
		return lockedSave{}, err
	}
	instance, err := q.LockWorkerComputerInstance(ctx, db.LockWorkerComputerInstanceParams{ID: params.ComputerInstanceID, OrgID: locator.OrgID, WorkerHostID: params.WorkerHostID, WorkerGroupID: params.WorkerGroupID, WorkerEpoch: params.WorkerEpoch})
	if err != nil {
		return lockedSave{}, err
	}
	if instance.EnvironmentID != params.EnvironmentID || instance.ComputerID != c.ID || instance.WriterGeneration != params.WriterGeneration || instance.WriterGeneration != c.WriterGeneration ||
		c.Status != "active" || c.DesiredState != "active" || instance.DesiredState != "ready" || instance.ObservedState != "ready" || instance.ObservedDesiredVersion != instance.DesiredVersion || instance.MountState != "mounted" || instance.ReclaimedAt.Valid ||
		(instance.AdmissionState != "open" && instance.AdmissionState != "draining") {
		return lockedSave{}, fmt.Errorf("%w: %s", pgx.ErrNoRows, "computer save writer is no longer active")
	}
	pending := instance.SaveDiskVersionID == params.SaveID && instance.SaveSequence == ref.Sequence
	return lockedSave{tx: tx, principal: principal, ref: ref, computer: c, instance: instance, pending: pending}, nil
}

// lockSaveBegin admits the save into the Instance's empty save slot, or
// replays an admission that already holds it.
func lockSaveBegin(ctx context.Context, tx pgx.Tx, principal workergroup.HostPrincipal, ref SaveRef) (saveBegin, error) {
	s, err := lockSave(ctx, tx, principal, ref)
	if err != nil {
		return saveBegin{}, err
	}
	predecessor := s.instance.SaveBaseDiskVersionID
	if !s.pending {
		i := s.instance
		row, err := db.New(tx).BeginComputerInstanceSave(ctx, db.BeginComputerInstanceSaveParams{Sequence: ref.Sequence, SaveID: pgvalue.UUID(ref.SaveID), PredecessorID: s.computer.HeadDiskVersionID, ComputerInstanceID: i.ID, EnvironmentID: i.EnvironmentID, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, WriterGeneration: i.WriterGeneration, WriterTokenHash: i.WriterTokenHash, DesiredVersion: i.DesiredVersion})
		if err != nil {
			return saveBegin{}, err
		}
		predecessor = row.SaveBaseDiskVersionID
	}
	return saveBegin{lockedSave: s, predecessor: predecessor}, nil
}

// lockPendingSave locks a save that holds the Instance's save slot and whose
// committed receipt exists exactly when published.
func lockPendingSave(ctx context.Context, tx pgx.Tx, principal workergroup.HostPrincipal, ref SaveRef, published bool) (lockedSave, error) {
	s, err := lockSave(ctx, tx, principal, ref)
	if err != nil {
		return lockedSave{}, err
	}
	if !s.pending {
		return lockedSave{}, fmt.Errorf("%w: %s", pgx.ErrNoRows, "save operation is not pending")
	}
	_, err = db.New(tx).GetWorkerComputerSave(ctx, ref.receipt(principal))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return lockedSave{}, err
	}
	if (err == nil) != published {
		return lockedSave{}, fmt.Errorf("%w: %s", pgx.ErrNoRows, "save publication state differs from operation")
	}
	return s, nil
}

func lockUnpublishedSave(ctx context.Context, tx pgx.Tx, principal workergroup.HostPrincipal, ref SaveRef) (unpublishedSave, error) {
	s, err := lockPendingSave(ctx, tx, principal, ref, false)
	return unpublishedSave{s}, err
}

func lockPublishedSave(ctx context.Context, tx pgx.Tx, principal workergroup.HostPrincipal, ref SaveRef) (publishedSave, error) {
	s, err := lockPendingSave(ctx, tx, principal, ref, true)
	return publishedSave{s}, err
}

// recheckWriter re-evaluates, after the save's writes and before commit, the
// host epoch and statuses and the writer deadline: locks prevent credential
// changes, but elapsed time can still expire a writer.
func (s lockedSave) recheckWriter(ctx context.Context) error {
	var authorized bool
	err := s.tx.QueryRow(ctx, `SELECT w.current_epoch=$3 AND w.status IN ('active','draining')
 AND g.status IN ('active','paused','draining') AND clock_timestamp()<$4
 FROM worker_hosts w JOIN worker_groups g ON g.id=w.worker_group_id WHERE w.id=$1 AND g.id=$2`, pgvalue.UUID(s.principal.HostID), pgvalue.UUID(s.principal.GroupID), s.principal.Epoch, s.instance.WriterExpiresAt).Scan(&authorized)
	if err != nil {
		return err
	}
	if !authorized {
		return fmt.Errorf("%w: %s", pgx.ErrNoRows, "computer save writer expired or revoked")
	}
	return nil
}

// retention is where the save's candidate pins live.
func (s lockedSave) retention() objectRetention {
	i := s.instance
	return objectRetention{environmentID: i.EnvironmentID, computerID: i.ComputerID, instanceID: i.ID, desiredVersion: i.DesiredVersion, key: savePublicationKey(pgvalue.MustUUIDValue(i.ID), i.SaveSequence, pgvalue.MustUUIDValue(i.SaveDiskVersionID))}
}

// objects is the save's object scope: the Instance's retained source keys
// and its retained write key.
func (s unpublishedSave) objects(ctx context.Context) (objectScope, error) {
	i := s.instance
	q := db.New(s.tx)
	keys, err := q.ListInstanceComputerSourceKeys(ctx, i.ID)
	if err != nil {
		return objectScope{}, err
	}
	allowed := make(map[string]bool, len(keys)+1)
	for _, k := range keys {
		allowed[pgvalue.UUIDString(k.ID)] = true
	}
	write, err := q.GetRuntimeComputerWriteKey(ctx, db.GetRuntimeComputerWriteKeyParams{ComputerInstanceID: i.ID, EnvironmentID: i.EnvironmentID, ComputerID: i.ComputerID})
	if err != nil {
		return objectScope{}, err
	}
	if !i.WriteKeyID.Valid || i.WriteKeyID != write.ID {
		return objectScope{}, objectConflict("save write key is not retained")
	}
	allowed[pgvalue.UUIDString(write.ID)] = true
	return objectScope{objectRetention: s.retention(), orgID: i.OrgID, projectID: i.ProjectID, logicalBytes: i.ReservedGuestEphemeralDiskBytes, allowedKeys: allowed}, nil
}

// SaveBegun is an admitted save: the version it saves on top of and the
// Instance desired version it writes at.
type SaveBegun struct {
	PredecessorID  pgtype.UUID
	DesiredVersion int64
}

// BeginSave admits a save into the Instance's save slot, or replays the
// admission that already holds it. It outlives the Runs of the Instance: only
// the writer fence and deadline bound it.
func (p Publisher) BeginSave(ctx context.Context, principal workergroup.HostPrincipal, ref SaveRef) (SaveBegun, error) {
	if err := ref.validate(); err != nil {
		return SaveBegun{}, err
	}
	var begun SaveBegun
	err := db.RunTx(ctx, p.db, func(tx pgx.Tx) error {
		s, err := lockSaveBegin(ctx, tx, principal, ref)
		if err != nil {
			return err
		}
		if err = s.recheckWriter(ctx); err != nil {
			return err
		}
		begun = SaveBegun{PredecessorID: s.predecessor, DesiredVersion: s.instance.DesiredVersion}
		return nil
	})
	if err != nil {
		return SaveBegun{}, authorityChanged(err)
	}
	return begun, nil
}

// RegisterSaveObject registers a save's disk object before its upload.
func (p Publisher) RegisterSaveObject(ctx context.Context, principal workergroup.HostPrincipal, ref SaveRef, inspection blockformat.ObjectInspection) error {
	if err := ref.validate(); err != nil {
		return err
	}
	if _, err := describeObject(inspection); err != nil {
		return err
	}
	return p.inUnpublishedSave(ctx, principal, ref, func(s unpublishedSave) error {
		scope, err := s.objects(ctx)
		if err != nil {
			return err
		}
		return scope.registerObject(ctx, s.tx, inspection)
	})
}

// ReuseSaveObject pins an already certified object of the Computer for the
// save.
func (p Publisher) ReuseSaveObject(ctx context.Context, principal workergroup.HostPrincipal, ref SaveRef, inspection blockformat.ObjectInspection) error {
	if err := ref.validate(); err != nil {
		return err
	}
	if _, err := describeObject(inspection); err != nil {
		return err
	}
	return p.inUnpublishedSave(ctx, principal, ref, func(s unpublishedSave) error {
		scope, err := s.objects(ctx)
		if err != nil {
			return err
		}
		return scope.reuseObject(ctx, s.tx, inspection)
	})
}

// CertifySaveObject certifies a registered save object after its upload,
// revalidating the save on both sides of the storage lookup: a first
// transaction verifies the exact registration and pin, object storage
// confirms the bytes outside any transaction, and a second transaction
// certifies them. Only the exact save may certify its retained bytes. A
// storage failure reports ErrStorageUnavailable.
func (p Publisher) CertifySaveObject(ctx context.Context, principal workergroup.HostPrincipal, ref SaveRef, inspection blockformat.ObjectInspection) error {
	if err := ref.validate(); err != nil {
		return err
	}
	object, err := describeObject(inspection)
	if err != nil {
		return err
	}
	if err = p.inUnpublishedSave(ctx, principal, ref, func(s unpublishedSave) error {
		return s.retention().verifyRegistered(ctx, s.tx, inspection)
	}); err != nil {
		return err
	}
	stored, err := p.objects.Stat(ctx, object.digest)
	if err != nil {
		return storageUnavailable(err)
	}
	return p.inUnpublishedSave(ctx, principal, ref, func(s unpublishedSave) error {
		scope, err := s.objects(ctx)
		if err != nil {
			return err
		}
		return scope.certifyObject(ctx, s.tx, inspection, stored)
	})
}

// inUnpublishedSave runs fn in one transaction under the unpublished save
// authority, then rechecks the writer.
func (p Publisher) inUnpublishedSave(ctx context.Context, principal workergroup.HostPrincipal, ref SaveRef, fn func(unpublishedSave) error) error {
	err := db.RunTx(ctx, p.db, func(tx pgx.Tx) error {
		s, err := lockUnpublishedSave(ctx, tx, principal, ref)
		if err != nil {
			return err
		}
		if err = fn(s); err != nil {
			return err
		}
		return s.recheckWriter(ctx)
	})
	return authorityChanged(err)
}

// PublishSave atomically records the save's committed receipt, its retained
// version and the Computer's saved head from its certified, pinned root. It
// leaves the save slot and object pins in place: upload success does not
// prove that the worker host adopted its durable source. An exact committed
// publication replays, before the transaction and again after it fails.
func (p Publisher) PublishSave(ctx context.Context, principal workergroup.HostPrincipal, ref SaveRef, root disk.GenerationRoot) (Publication, error) {
	if err := ref.validate(); err != nil {
		return Publication{}, err
	}
	locator, err := root.Locator(root.LogicalBytes)
	if err != nil {
		return Publication{}, err
	}
	fingerprint, err := saveFingerprint(ref, root)
	if err != nil {
		return Publication{}, err
	}
	receipt := ref.receipt(principal)
	replay := func() (Publication, error) {
		v, err := db.New(p.db).GetWorkerComputerSave(ctx, receipt)
		if err != nil {
			return Publication{}, err
		}
		if !bytes.Equal(v.PublicationRequestFingerprint, fingerprint[:]) {
			return Publication{}, objectConflict("save publication differs from committed request")
		}
		return publicationOf(v.ComputerID, v.ID), nil
	}
	if result, err := replay(); !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	var result Publication
	err = p.inUnpublishedSave(ctx, principal, ref, func(s unpublishedSave) error {
		var err error
		result, err = s.publish(ctx, root, locator, fingerprint[:])
		return err
	})
	if err != nil {
		if historical, e := replay(); !errors.Is(e, pgx.ErrNoRows) {
			return historical, e
		}
		return Publication{}, err
	}
	return result, nil
}

func (s unpublishedSave) publish(ctx context.Context, root disk.GenerationRoot, locator blockformat.Locator, fingerprint []byte) (Publication, error) {
	r := s.instance
	q := db.New(s.tx)
	if root.LogicalBytes != r.ReservedGuestEphemeralDiskBytes {
		return Publication{}, objectConflict("save capacity differs from Computer")
	}
	object, err := q.LockComputerObject(ctx, db.LockComputerObjectParams{EnvironmentID: r.EnvironmentID, ComputerID: r.ComputerID, Digest: root.Pack.Digest})
	if err != nil {
		return Publication{}, err
	}
	if !object.Certified.Bool {
		return Publication{}, objectConflict("save root is not certified")
	}
	var inspected blockformat.ObjectInspection
	if err = json.Unmarshal(object.Inspection, &inspected); err != nil {
		return Publication{}, err
	}
	if inspected.Pack == nil {
		return Publication{}, objectConflict("save root is not a pack")
	}
	if err = inspected.Pack.CheckRoot(locator, root.LogicalBytes); err != nil {
		return Publication{}, err
	}
	retention := s.retention()
	if _, err = q.RequireComputerObjectPin(ctx, db.RequireComputerObjectPinParams{ComputerInstanceID: r.ID, PublicationKey: retention.key, InstanceDesiredVersion: r.DesiredVersion, Digest: root.Pack.Digest}); err != nil {
		return Publication{}, err
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return Publication{}, err
	}
	v, err := q.PublishComputerInstanceSave(ctx, db.PublishComputerInstanceSaveParams{ComputerInstanceID: r.ID, EnvironmentID: r.EnvironmentID, WorkerHostID: r.WorkerHostID, WorkerEpoch: r.WorkerEpoch, WriterGeneration: r.WriterGeneration, WriterTokenHash: r.WriterTokenHash, DesiredVersion: r.DesiredVersion, SaveID: pgvalue.UUID(s.ref.SaveID), Sequence: s.ref.Sequence, RootPackDigest: pgvalue.Text(root.Pack.Digest), LogicalBytes: root.LogicalBytes, Fingerprint: fingerprint, Locator: encoded})
	if err != nil {
		return Publication{}, err
	}
	return publicationOf(v.ComputerID, v.ID), nil
}

// AdoptSave acknowledges the worker host's durable adoption of a published
// save as its source, after its producers joined: it transfers source
// retention and releases only this save's pins atomically, without rewriting
// execution origins or the Computer head. A historical acknowledgement is
// evidence only and grants no new mutation.
func (p Publisher) AdoptSave(ctx context.Context, principal workergroup.HostPrincipal, ref SaveRef, root disk.GenerationRoot) error {
	if err := ref.validate(); err != nil {
		return err
	}
	params := ref.receipt(principal)
	fingerprint, err := saveFingerprint(ref, root)
	if err != nil {
		return err
	}
	validate := func(v db.ComputerDiskVersion) error {
		if !bytes.Equal(v.PublicationRequestFingerprint, fingerprint[:]) {
			return objectConflict("source adoption differs from committed save")
		}
		return nil
	}
	q := db.New(p.db)
	replayed := func() (bool, error) {
		v, err := q.GetWorkerComputerSave(ctx, params)
		if err != nil {
			return false, err
		}
		if err = validate(v); err != nil {
			return false, err
		}
		// A committed save leaves its pending slot only through adoption. The
		// monotonic sequence excludes an unadmitted future request; this stays
		// true after later saves, without a second acknowledgement ledger.
		acknowledged, err := q.IsComputerInstanceSaveAdopted(ctx, db.IsComputerInstanceSaveAdoptedParams{ComputerInstanceID: params.ComputerInstanceID, EnvironmentID: params.EnvironmentID, WorkerHostID: params.WorkerHostID, WorkerGroupID: params.WorkerGroupID, WorkerEpoch: params.WorkerEpoch, WriterGeneration: params.WriterGeneration, Sequence: params.Sequence, SaveID: params.SaveID})
		return acknowledged.Valid && acknowledged.Bool, err
	}
	if done, err := replayed(); err != nil || done {
		return authorityChanged(err)
	}
	err = db.RunTx(ctx, p.db, func(tx pgx.Tx) error {
		s, err := lockPublishedSave(ctx, tx, principal, ref)
		if err != nil {
			return err
		}
		if err = s.adopt(ctx, params, validate); err != nil {
			return err
		}
		return s.recheckWriter(ctx)
	})
	if err != nil {
		if done, replayErr := replayed(); done && replayErr == nil {
			return nil
		}
	}
	return authorityChanged(err)
}

func (s publishedSave) adopt(ctx context.Context, params db.GetWorkerComputerSaveParams, validate func(db.ComputerDiskVersion) error) error {
	r := s.instance
	q := db.New(s.tx)
	v, err := q.GetWorkerComputerSave(ctx, params)
	if err != nil {
		return err
	}
	if err = validate(v); err != nil {
		return err
	}
	n, err := q.AdoptComputerInstanceSave(ctx, db.AdoptComputerInstanceSaveParams{ComputerInstanceID: r.ID, EnvironmentID: r.EnvironmentID, WriterGeneration: r.WriterGeneration, WriterTokenHash: r.WriterTokenHash, WorkerHostID: r.WorkerHostID, WorkerEpoch: r.WorkerEpoch, Sequence: s.ref.Sequence, SaveID: v.ID})
	if err != nil {
		return err
	}
	if n != 1 {
		return objectConflict("save source changed during adoption")
	}
	_, err = s.tx.Exec(ctx, `DELETE FROM computer_object_pins WHERE computer_instance_id=$1 AND publication_key=$2`, r.ID, []byte(s.retention().key))
	return err
}

// AbandonSave releases the save slot of an unpublished save once the worker
// host cancelled and joined every producer and upload of it; a published save
// requires adoption instead. It acknowledges the desired absence, not that an
// arbitrary save was once admitted, and mutates nothing at a retired
// sequence. The save's object pins stay until the Instance is reclaimed: the
// live disk can still reuse the same ciphertext in a later save or
// checkpoint. Lost execution authority leaves retention to Instance
// reclamation.
func (p Publisher) AbandonSave(ctx context.Context, principal workergroup.HostPrincipal, ref SaveRef) error {
	if err := ref.validate(); err != nil {
		return err
	}
	receipt := ref.receipt(principal)
	params := db.IsComputerSaveAbandonedParams{Sequence: ref.Sequence, ComputerInstanceID: receipt.ComputerInstanceID, EnvironmentID: receipt.EnvironmentID, WorkerHostID: receipt.WorkerHostID, WorkerGroupID: receipt.WorkerGroupID, WorkerEpoch: receipt.WorkerEpoch, WriterGeneration: receipt.WriterGeneration}
	absent := func() bool {
		result, err := db.New(p.db).IsComputerSaveAbandoned(ctx, params)
		return err == nil && result.Valid && result.Bool
	}
	if absent() {
		return nil
	}
	err := p.inUnpublishedSave(ctx, principal, ref, func(s unpublishedSave) error {
		r := s.instance
		cleared, err := db.New(s.tx).AbandonComputerInstanceSave(ctx, db.AbandonComputerInstanceSaveParams{ComputerInstanceID: r.ID, EnvironmentID: r.EnvironmentID, WriterGeneration: r.WriterGeneration, WriterTokenHash: r.WriterTokenHash, WorkerHostID: r.WorkerHostID, WorkerEpoch: r.WorkerEpoch, Sequence: r.SaveSequence, SaveID: r.SaveDiskVersionID})
		if err != nil {
			return err
		}
		if cleared != 1 {
			return objectConflict("save operation changed during abandonment")
		}
		return nil
	})
	if err != nil && absent() {
		return nil
	}
	return err
}
