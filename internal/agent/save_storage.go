package agent

import (
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// SaveObjectStore confirms immutable uploaded descriptors. Byte authentication
// and complete physical-pack inspection precede registration on the owned
// publication pipeline; an existence claim from a guest is not an inspection.
type SaveObjectStore interface {
	Stat(context.Context, string) (cas.Object, error)
}

// SavePublisher owns disk graph retention and durable publication. It is an
// internal reconciliation operation, not execution authority or a public API.
// Its caller must authenticate operation-bound capture/inspection receipts.
// New publication requires the original writer to remain authorized. An already
// committed publication can be reconciled separately after that owner is fenced.
type SavePublisher struct {
	pool    db.TxBeginner
	objects SaveObjectStore
}

func NewSavePublisher(pool db.TxBeginner, objects SaveObjectStore) (*SavePublisher, error) {
	if pool == nil || objects == nil {
		return nil, errors.New("save database and object store are required")
	}
	return &SavePublisher{pool: pool, objects: objects}, nil
}

type SavePublication struct {
	EnvironmentID uuid.UUID
	SaveID        uuid.UUID
	LeaseEpoch    int64
	Host          workergroup.HostPrincipal
}

// Capture accepts the owned worker's flush/capture receipt under the same
// Host, Computer and Save locks used for publication. A foreign or expired
// writer cannot reserve the captured identity before the legitimate owner.
func (p *SavePublisher) Capture(ctx context.Context, ref SavePublication, root disk.VersionRoot, evidence string) error {
	identity, err := root.Digest()
	if err != nil || evidence == "" {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, p.pool, func(tx pgx.Tx) error {
		scope, state, err := lockSaveIdentity(ctx, tx, ref)
		if err != nil {
			return err
		}
		if err = recordCapture(ctx, tx, CaptureEvidence{EnvironmentID: ref.EnvironmentID, SaveID: ref.SaveID, LeaseEpoch: ref.LeaseEpoch, DiskRoot: identity, Evidence: evidence}); err != nil {
			return err
		}
		if state == "published" {
			return nil
		}
		return scope.requireWriter(ctx, tx, ref.Host)
	}))
}

// Register retains an authenticated object's complete graph before its upload.
// Children are certified first. Every physical page counts, including pages
// outside the logical tree selected by this save's root.
func (p *SavePublisher) Register(ctx context.Context, ref SavePublication, inspection blockformat.ObjectInspection) error {
	return hideMissing(db.RunTx(ctx, p.pool, func(tx pgx.Tx) error {
		scope, state, err := lockSaveStorage(ctx, tx, ref)
		if err != nil {
			return err
		}
		if state == "published" {
			return scope.verifyRegistered(ctx, tx, inspection)
		}
		if err = scope.registerObject(ctx, tx, inspection); err != nil {
			return err
		}
		return scope.requireWriter(ctx, tx, ref.Host)
	}))
}

// Certify performs storage I/O outside database locks and rechecks the exact
// retained registration afterwards. Errors and unavailable storage leave the
// candidate pending; they never establish permanent absence.
func (p *SavePublisher) Certify(ctx context.Context, ref SavePublication, inspection blockformat.ObjectInspection) error {
	object, err := describeSaveObject(inspection)
	if err != nil {
		return err
	}
	var committed bool
	err = db.RunTx(ctx, p.pool, func(tx pgx.Tx) error {
		scope, state, err := lockSaveStorage(ctx, tx, ref)
		if err != nil {
			return err
		}
		committed = state == "published"
		if committed {
			return scope.verifyCertified(ctx, tx, inspection)
		}
		return scope.verifyRegistered(ctx, tx, inspection)
	})
	if err != nil {
		return hideMissing(err)
	}
	if committed {
		return nil
	}
	stored, err := p.objects.Stat(ctx, object.digest)
	if err != nil {
		return saveStorageUnavailable(err)
	}
	return hideMissing(db.RunTx(ctx, p.pool, func(tx pgx.Tx) error {
		scope, state, err := lockSaveStorage(ctx, tx, ref)
		if err != nil {
			return err
		}
		if state == "published" {
			return scope.verifyCertified(ctx, tx, inspection)
		}
		if err = scope.certifyObject(ctx, tx, inspection, stored); err != nil {
			return err
		}
		return scope.requireWriter(ctx, tx, ref.Host)
	}))
}

// Publish commits the exact captured locator and its certified root together
// with publication and recovery-head advancement. A matching pack alone does
// not permit selecting a different page or substituting another cut.
func (p *SavePublisher) Publish(ctx context.Context, ref SavePublication, root disk.VersionRoot, evidence string) error {
	identity, err := root.Digest()
	if err != nil || evidence == "" {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, p.pool, func(tx pgx.Tx) error {
		scope, state, err := lockSaveIdentity(ctx, tx, ref)
		if err != nil {
			return err
		}
		if state == "requested" {
			return ErrNotReady
		}
		var captured string
		if err = tx.QueryRow(ctx, `SELECT 'sha256:'||encode(captured_root_digest,'hex') FROM computer_saves WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, ref.SaveID).Scan(&captured); err != nil {
			return err
		}
		if identity != captured {
			return ErrConflict
		}
		if state == "published" {
			return nil
		}
		if err = scope.loadSaveStorage(ctx, tx); err != nil {
			return err
		}
		locator, err := root.Locator(scope.logicalBytes)
		if err != nil {
			return saveObjectConflict("root capacity differs from mounted source: %v", err)
		}
		stored, err := db.New(tx).LockComputerObject(ctx, db.LockComputerObjectParams{EnvironmentID: scope.environmentID, Digest: root.Pack.Digest})
		if err != nil {
			return saveObjectMissing(err)
		}
		var inspection blockformat.ObjectInspection
		if !stored.Certified.Bool || json.Unmarshal(stored.Inspection, &inspection) != nil || inspection.Pack == nil {
			return ErrNotReady
		}
		if err = inspection.Pack.CheckRoot(locator, scope.logicalBytes); err != nil {
			return saveObjectConflict("root is absent from the authenticated pack: %v", err)
		}
		object, err := describeSaveObject(inspection)
		if err != nil {
			return err
		}
		if err = scope.requireRetained(ctx, tx, object); err != nil {
			return err
		}
		if err = scope.checkKeyClosure(ctx, tx, object); err != nil {
			return err
		}
		raw, err := json.Marshal(root)
		if err != nil {
			return err
		}
		rootID, err := db.New(tx).RetainComputerDiskRoot(ctx, db.RetainComputerDiskRootParams{ID: pgvalue.NewUUIDv7(), EnvironmentID: scope.environmentID, Locator: raw})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE computer_saves SET status='published',publication_evidence=$3,root_id=$4 WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, ref.SaveID, evidence, rootID); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE computers c SET recovery_save_id=$3 FROM computer_saves s
          WHERE c.environment_id=$1 AND c.id=$2 AND s.environment_id=c.environment_id AND s.id=$3
          AND (c.recovery_save_id IS NULL OR s.seq>(SELECT old.seq FROM computer_saves old WHERE old.environment_id=c.environment_id AND old.id=c.recovery_save_id))`, scope.environmentID, scope.computerID, scope.saveID); err != nil {
			return err
		}
		return scope.requireWriter(ctx, tx, ref.Host)
	}))
}

// Computer then Save is the publication lock order. The Save's immutable lease
// chooses the mounted base and write key; the current recovery head must never
// silently change that source. No current execution lease is granted or renewed.
func lockSaveStorage(ctx context.Context, tx pgx.Tx, ref SavePublication) (saveObjectScope, string, error) {
	s, state, err := lockSaveIdentity(ctx, tx, ref)
	if err == nil {
		err = s.loadSaveStorage(ctx, tx)
	}
	return s, state, err
}

// Immutable publication identity survives retirement of the usable graph.
func lockSaveIdentity(ctx context.Context, tx pgx.Tx, ref SavePublication) (saveObjectScope, string, error) {
	var s saveObjectScope
	if ref.EnvironmentID == uuid.Nil() || ref.SaveID == uuid.Nil() || ref.LeaseEpoch <= 0 {
		return s, "", ErrInvalidInput
	}
	if err := lockComputerHost(ctx, tx, ref.Host); err != nil {
		return s, "", err
	}
	s.environmentID, s.saveID, s.leaseEpoch = pgvalue.UUID(ref.EnvironmentID), pgvalue.UUID(ref.SaveID), ref.LeaseEpoch
	if err := tx.QueryRow(ctx, `SELECT computer_id FROM computer_saves WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, ref.SaveID).Scan(&s.computerID); err != nil {
		return s, "", err
	}
	if _, err := tx.Exec(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, s.environmentID, s.computerID); err != nil {
		return s, "", err
	}
	var state string
	var epoch int64
	var sourceHost uuid.UUID
	var sourceWorkerEpoch int64
	if err := tx.QueryRow(ctx, `SELECT s.status,s.computer_lease_epoch,l.worker_host_id,l.worker_epoch FROM computer_saves s
      JOIN computer_leases l ON (l.environment_id,l.computer_id,l.epoch)=(s.environment_id,s.computer_id,s.computer_lease_epoch)
      WHERE s.environment_id=$1 AND s.id=$2 FOR NO KEY UPDATE OF s`, s.environmentID, s.saveID).Scan(&state, &epoch, &sourceHost, &sourceWorkerEpoch); err != nil {
		return s, "", err
	}
	if epoch != ref.LeaseEpoch || sourceHost != ref.Host.HostID || sourceWorkerEpoch != ref.Host.Epoch {
		return s, "", ErrDenied
	}
	if state == "failed" {
		return s, "", ErrTerminal
	}
	if state != "published" {
		if err := s.requireWriter(ctx, tx, ref.Host); err != nil {
			return s, "", err
		}
	}
	return s, state, nil
}

func (s *saveObjectScope) loadSaveStorage(ctx context.Context, tx pgx.Tx) error {
	var key uuid.UUID
	err := tx.QueryRow(ctx, `SELECT e.org_id,e.project_id,d.base_root_id,d.write_key_id,r.logical_bytes
      FROM computer_leases d JOIN environments e ON e.id=d.environment_id
      JOIN computer_disk_roots r ON (r.environment_id,r.id)=(d.environment_id,d.base_root_id)
      WHERE d.environment_id=$1 AND d.computer_id=$2 AND d.epoch=$3 AND d.disk_released_at IS NULL`, s.environmentID, s.computerID, s.leaseEpoch).Scan(&s.orgID, &s.projectID, &s.baseRoot, &key, &s.logicalBytes)
	if err != nil {
		return err
	}
	s.writeKey = key.String()
	s.allowedKeys = map[string]bool{s.writeKey: true}
	rows, err := tx.Query(ctx, `SELECT k.key_id FROM computer_disk_roots r JOIN computer_object_keys k ON (k.environment_id,k.digest)=(r.environment_id,r.root_pack_digest) WHERE r.environment_id=$1 AND r.id=$2`, s.environmentID, s.baseRoot)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err = rows.Scan(&key); err != nil {
			return err
		}
		s.allowedKeys[key.String()] = true
	}
	return rows.Err()
}

func (s saveObjectScope) requireWriter(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal) error {
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND worker_host_id=$4 AND worker_epoch=$5 AND status IN ('active','releasing') AND fenced_at IS NULL AND expires_at>clock_timestamp())`, s.environmentID, s.computerID, s.leaseEpoch, host.HostID, host.Epoch).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrDenied
	}
	return nil
}
