package agent

import (
	"context"
	"encoding/json"
	"errors"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// PreparationPublisher retains a full encrypted disk graph owned by a private
// preparation. The trusted host supplies authenticated inspections from its
// quiesced capture pipeline. It must keep the executor alive through Publish,
// then join physical stop separately; capture never supplies fence evidence.
type PreparationPublisher struct {
	pool    db.TxBeginner
	objects SaveObjectStore
}

func NewPreparationPublisher(pool db.TxBeginner, objects SaveObjectStore) (*PreparationPublisher, error) {
	if pool == nil || objects == nil {
		return nil, errors.New("preparation database and object store are required")
	}
	return &PreparationPublisher{pool: pool, objects: objects}, nil
}

// BeginCapture seals credential delivery after authored preparation and fixes
// the full disk capacity. It must precede object registration; a repeated call
// cannot change capacity or authorize another authored execution.
func (p *PreparationPublisher) BeginCapture(ctx context.Context, host workergroup.HostPrincipal, ref PreparationExecutor, logicalBytes int64) error {
	if !ref.valid() || logicalBytes != disk.SeedCapacity {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, p.pool, func(tx pgx.Tx) error {
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		if err := lockPreparationSecretOwners(ctx, tx, ref); err != nil {
			return err
		}
		if err := lockPreparationExecutorOnHost(ctx, tx, host, ref, true); err != nil {
			return err
		}
		var complete bool
		if err := tx.QueryRow(ctx, `SELECT p.write_key_id IS NOT NULL AND NOT EXISTS (
    SELECT 1 FROM computer_secret_bindings binding WHERE binding.environment_id=s.environment_id AND binding.preparation_spec_id=s.id AND NOT EXISTS (
      SELECT 1 FROM secret_exposures x WHERE x.environment_id=p.environment_id AND x.preparation_id=p.id AND x.secret_id=binding.secret_id))
   FROM computer_preparations p JOIN computer_preparation_specs s ON (s.environment_id,s.id)=(p.environment_id,p.preparation_spec_id)
   WHERE p.environment_id=$1 AND p.id=$2`, ref.EnvironmentID, ref.PreparationID).Scan(&complete); err != nil {
			return err
		}
		if !complete {
			return ErrNotReady
		}
		var capacity int64
		if err := tx.QueryRow(ctx, `UPDATE computer_preparations SET proxy_ca_certificate=NULL,proxy_ca_private_key_nonce=NULL,proxy_ca_private_key_ciphertext=NULL,proxy_ca_not_after=NULL,logical_bytes=COALESCE(logical_bytes,$3) WHERE environment_id=$1 AND id=$2 RETURNING logical_bytes`, ref.EnvironmentID, ref.PreparationID, logicalBytes).Scan(&capacity); err != nil {
			return err
		}
		if capacity != logicalBytes {
			return ErrConflict
		}
		return checkPreparationExecutor(ctx, tx, host, ref, true)
	}))
}

func (p *PreparationPublisher) Register(ctx context.Context, host workergroup.HostPrincipal, ref PreparationExecutor, inspection blockformat.ObjectInspection) error {
	return hideMissing(db.RunTx(ctx, p.pool, func(tx pgx.Tx) error {
		scope, done, err := lockPreparationStorage(ctx, tx, host, ref)
		if err != nil {
			return err
		}
		if done {
			return scope.verifyRegistered(ctx, tx, inspection)
		}
		if err = scope.registerObject(ctx, tx, inspection); err != nil {
			return err
		}
		return checkPreparationExecutor(ctx, tx, host, ref, true)
	}))
}
func (p *PreparationPublisher) Certify(ctx context.Context, host workergroup.HostPrincipal, ref PreparationExecutor, inspection blockformat.ObjectInspection) error {
	object, err := describeSaveObject(inspection)
	if err != nil {
		return err
	}
	var done bool
	err = db.RunTx(ctx, p.pool, func(tx pgx.Tx) error {
		scope, committed, err := lockPreparationStorage(ctx, tx, host, ref)
		if err != nil {
			return err
		}
		done = committed
		if done {
			return scope.verifyCertified(ctx, tx, inspection)
		}
		return scope.verifyRegistered(ctx, tx, inspection)
	})
	if err != nil {
		return hideMissing(err)
	}
	if done {
		return nil
	}
	stored, err := p.objects.Stat(ctx, object.digest)
	if err != nil {
		return saveStorageUnavailable(err)
	}
	return hideMissing(db.RunTx(ctx, p.pool, func(tx pgx.Tx) error {
		scope, done, err := lockPreparationStorage(ctx, tx, host, ref)
		if err != nil {
			return err
		}
		if done {
			return scope.verifyCertified(ctx, tx, inspection)
		}
		if err = scope.certifyObject(ctx, tx, inspection, stored); err != nil {
			return err
		}
		return checkPreparationExecutor(ctx, tx, host, ref, true)
	}))
}

// Capture records the operation-bound receipt for the completed full disk cut.
// It cannot substitute another page in a previously captured physical pack.
func (p *PreparationPublisher) Capture(ctx context.Context, host workergroup.HostPrincipal, ref PreparationExecutor, root disk.VersionRoot, evidence string) error {
	identity, err := root.Digest()
	if err != nil || !preparationEvidence(evidence) {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, p.pool, func(tx pgx.Tx) error {
		scope, done, err := lockPreparationStorage(ctx, tx, host, ref)
		if err != nil {
			return err
		}
		if _, err = root.Locator(scope.logicalBytes); err != nil {
			return ErrConflict
		}
		var prior *string
		if err = tx.QueryRow(ctx, `SELECT capture_root FROM computer_preparations WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, ref.PreparationID).Scan(&prior); err != nil {
			return err
		}
		if prior != nil {
			if *prior != identity {
				return ErrConflict
			}
			return nil
		}
		if done {
			return ErrConflict
		}
		if _, err = tx.Exec(ctx, `UPDATE computer_preparations SET capture_root=$3,capture_evidence=$4 WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, ref.PreparationID, identity, evidence); err != nil {
			return err
		}
		return checkPreparationExecutor(ctx, tx, host, ref, true)
	}))
}

func (p *PreparationPublisher) Publish(ctx context.Context, host workergroup.HostPrincipal, ref PreparationExecutor, root disk.VersionRoot, evidence string) error {
	identity, err := root.Digest()
	if err != nil || !preparationEvidence(evidence) {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, p.pool, func(tx pgx.Tx) error {
		scope, done, err := lockPreparationStorage(ctx, tx, host, ref)
		if err != nil {
			return err
		}
		var captured *string
		if err = tx.QueryRow(ctx, `SELECT capture_root FROM computer_preparations WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, ref.PreparationID).Scan(&captured); err != nil {
			return err
		}
		if captured == nil {
			return ErrNotReady
		}
		if *captured != identity {
			return ErrConflict
		}
		if done {
			return nil
		}
		locator, err := root.Locator(scope.logicalBytes)
		if err != nil {
			return ErrConflict
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
			return saveObjectConflict("root is absent from authenticated pack: %v", err)
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
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `INSERT INTO computer_images(environment_id,id,preparation_spec_id,preparation_id,seq,root_id,publication_evidence)
 SELECT p.environment_id,$5,p.preparation_spec_id,p.id,COALESCE((SELECT max(i.seq) FROM computer_images i WHERE i.environment_id=p.environment_id AND i.preparation_spec_id=p.preparation_spec_id),0)+1,$3,$4
 FROM computer_preparations p WHERE p.environment_id=$1 AND p.id=$2`, ref.EnvironmentID, ref.PreparationID, rootID, evidence, uuid.NewV7()); err != nil {
			return err
		}
		// Recheck expiry after any graph/FK lock waits, before changing status.
		if err = checkPreparationExecutor(ctx, tx, host, ref, true); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE computer_preparations SET proxy_ca_certificate=NULL,proxy_ca_private_key_nonce=NULL,proxy_ca_private_key_ciphertext=NULL,proxy_ca_not_after=NULL,status='succeeded' WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, ref.PreparationID)
		return err
	}))
}

func preparationEvidence(s string) bool { return len(s) > 0 && len(s) <= 4096 && utf8.ValidString(s) }

func lockPreparationStorage(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, ref PreparationExecutor) (saveObjectScope, bool, error) {
	var scope saveObjectScope
	if !ref.valid() {
		return scope, false, ErrInvalidInput
	}
	if err := lockComputerHost(ctx, tx, host); err != nil {
		return scope, false, err
	}
	if err := lockPreparationSecretOwners(ctx, tx, ref); err != nil {
		return scope, false, err
	}
	if err := lockPreparationExecutorOnHost(ctx, tx, host, ref, false); err != nil {
		return scope, false, err
	}
	var state string
	var capacity *int64
	var key *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT p.status,p.logical_bytes,p.write_key_id,e.org_id,e.project_id FROM computer_preparations p JOIN environments e ON e.id=p.environment_id WHERE p.environment_id=$1 AND p.id=$2`, ref.EnvironmentID, ref.PreparationID).Scan(&state, &capacity, &key, &scope.orgID, &scope.projectID); err != nil {
		return scope, false, err
	}
	done := state == "succeeded"
	if !done {
		if err := checkPreparationExecutor(ctx, tx, host, ref, true); err != nil {
			return scope, false, err
		}
	}
	if capacity == nil || key == nil {
		return scope, false, ErrNotReady
	}
	scope.environmentID = pgvalue.UUID(ref.EnvironmentID)
	scope.preparationID = pgvalue.UUID(ref.PreparationID)
	scope.logicalBytes = *capacity
	scope.writeKey = key.String()
	scope.allowedKeys = map[string]bool{scope.writeKey: true}
	return scope, done, nil
}
