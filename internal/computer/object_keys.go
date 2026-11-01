package computer

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// objectKeyAuthority is one authorized holder of an object-set data key.
type objectKeyAuthority struct {
	// bootSeed selects boot_seed_builds; otherwise checkpoints owns the key.
	bootSeed         bool
	environment, id  uuid.UUID
	scope            string
	mayCreate        bool
	authorizedHolder bool
}

// objectKey delivers the data key of one memory checkpoint or boot seed. Only
// the capturing allocation may create it; a restoring authority receives an
// existing key or nothing. Provider operations run without SQL locks, and the
// authority and envelope are checked again before plaintext leaves.
func (b *KeyBroker) objectKey(ctx context.Context, keyID string, authorize func(tx pgx.Tx) (objectKeyAuthority, error)) ([]byte, error) {
	read := func(candidate *computerkey.Envelope) (computerkey.Envelope, objectKeyAuthority, error) {
		var envelope computerkey.Envelope
		var authority objectKeyAuthority
		err := db.RunTx(ctx, b.database, func(tx pgx.Tx) error {
			if err := shortLockTimeout(ctx, tx); err != nil {
				return err
			}
			var err error
			if authority, err = authorize(tx); err != nil {
				return err
			}
			if !authority.authorizedHolder {
				return ErrDenied
			}
			if candidate != nil {
				if !authority.mayCreate {
					return ErrDenied
				}
				var err error
				if authority.bootSeed {
					_, err = tx.Exec(ctx, `UPDATE boot_seed_builds SET data_key_wrapping_key_id=$3,data_key_wrapped=$4 WHERE environment_id=$1 AND id=$2 AND data_key_wrapped IS NULL`,
						authority.environment, authority.id, candidate.WrappingKeyID, candidate.Ciphertext)
				} else {
					_, err = tx.Exec(ctx, `UPDATE checkpoints SET data_key_wrapping_key_id=$3,data_key_wrapped=$4 WHERE environment_id=$1 AND id=$2 AND data_key_wrapped IS NULL`,
						authority.environment, authority.id, candidate.WrappingKeyID, candidate.Ciphertext)
				}
				if err != nil {
					return err
				}
			}
			var wrappingKey *string
			if authority.bootSeed {
				err = tx.QueryRow(ctx, `SELECT data_key_wrapping_key_id,data_key_wrapped FROM boot_seed_builds WHERE environment_id=$1 AND id=$2 FOR SHARE`,
					authority.environment, authority.id).Scan(&wrappingKey, &envelope.Ciphertext)
			} else {
				err = tx.QueryRow(ctx, `SELECT data_key_wrapping_key_id,data_key_wrapped FROM checkpoints WHERE environment_id=$1 AND id=$2 FOR SHARE`,
					authority.environment, authority.id).Scan(&wrappingKey, &envelope.Ciphertext)
			}
			if err != nil {
				return err
			}
			if wrappingKey != nil {
				envelope.WrappingKeyID = *wrappingKey
			} else if !authority.mayCreate {
				return ErrDenied
			}
			return nil
		})
		return envelope, authority, notReadyOnLock(err)
	}
	envelope, authority, err := read(nil)
	if err != nil {
		return nil, err
	}
	if len(envelope.Ciphertext) == 0 {
		key := make([]byte, computerkey.Size)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		candidate, err := b.wrapper.Wrap(ctx, authority.scope, keyID, key)
		clear(key)
		if err != nil {
			return nil, err
		}
		if envelope, authority, err = read(&candidate); err != nil {
			return nil, err
		}
	}
	plaintext, err := b.wrapper.Unwrap(ctx, authority.scope, keyID, envelope)
	if err != nil || len(plaintext) != computerkey.Size {
		clear(plaintext)
		return nil, errors.Join(err, ErrDenied)
	}
	current, _, err := read(nil)
	if err != nil || current.WrappingKeyID != envelope.WrappingKeyID || !bytes.Equal(current.Ciphertext, envelope.Ciphertext) {
		clear(plaintext)
		return nil, errors.Join(err, ErrConflict)
	}
	return plaintext, nil
}

// CheckpointKey delivers a memory checkpoint's data key to the live allocation
// capturing it, or to the live allocation restoring it.
func (b *KeyBroker) CheckpointKey(ctx context.Context, host workergroup.HostPrincipal, identity LeaseIdentity, checkpoint uuid.UUID) ([]byte, error) {
	if !identity.Valid() || checkpoint == uuid.Nil() {
		return nil, ErrInvalidInput
	}
	return b.objectKey(ctx, "checkpoint:"+checkpoint.String(), func(tx pgx.Tx) (objectKeyAuthority, error) {
		a := objectKeyAuthority{environment: identity.EnvironmentID, id: checkpoint}
		if err := LockHost(ctx, tx, host); err != nil {
			return a, err
		}
		if err := checkComputerDiskAuthority(ctx, tx, host, identity); err != nil {
			return a, err
		}
		var org uuid.UUID
		var capturing, restoring bool
		err := tx.QueryRow(ctx, `SELECT e.org_id,
 cp.source_lease_epoch=l.epoch AND cp.status IN ('capturing','sealed'),
 cp.disk_save_id=l.restored_from_save_id AND (cp.target_lease_epoch IS NULL OR cp.target_lease_epoch=l.epoch) AND cp.status IN ('ready','restoring')
 FROM checkpoints cp JOIN environments e ON e.id=cp.environment_id
 JOIN computer_leases l ON (l.environment_id,l.computer_id)=(cp.environment_id,cp.computer_id) AND l.epoch=$3
 WHERE cp.environment_id=$1 AND cp.id=$2 AND cp.computer_id=$4`, identity.EnvironmentID, checkpoint, identity.Epoch, identity.ComputerID).Scan(&org, &capturing, &restoring)
		if err != nil {
			return a, hideMissing(err)
		}
		a.mayCreate, a.authorizedHolder = capturing, capturing || restoring
		a.scope, err = computerkey.EncryptionScope(org.String(), identity.EnvironmentID.String())
		return a, err
	})
}

// BootSeedKey delivers a boot seed's data key to the live seed allocation that
// builds it, to a live Computer allocation that selected it, or under the
// preparation source offered for it.
func (b *KeyBroker) BootSeedKey(ctx context.Context, host workergroup.HostPrincipal, scope StorageScope, build uuid.UUID) ([]byte, error) {
	if !scope.valid() || scope.Preparation != nil || build == uuid.Nil() {
		return nil, ErrInvalidInput
	}
	return b.objectKey(ctx, "boot-seed:"+build.String(), func(tx pgx.Tx) (objectKeyAuthority, error) {
		a := objectKeyAuthority{bootSeed: true, id: build}
		if err := LockHost(ctx, tx, host); err != nil {
			return a, err
		}
		var err error
		switch {
		case scope.BootSeed != nil:
			ref := *scope.BootSeed
			a.environment = ref.EnvironmentID
			if ref.BuildID != build {
				return a, ErrDenied
			}
			if err = CheckBootSeedExecutor(ctx, tx, host, ref, true); err != nil {
				return a, err
			}
			a.mayCreate, a.authorizedHolder = true, true
		case scope.Computer != nil:
			identity := *scope.Computer
			a.environment = identity.EnvironmentID
			if err = checkComputerDiskAuthority(ctx, tx, host, identity); err != nil {
				return a, err
			}
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_leases l JOIN boot_seed_builds b ON (b.environment_id,b.id)=(l.environment_id,l.boot_seed_build_id)
 WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3 AND l.boot_seed_build_id=$4 AND b.status='succeeded')`,
				identity.EnvironmentID, identity.ComputerID, identity.Epoch, build).Scan(&a.authorizedHolder)
		default:
			key := *scope.PreparationSource
			a.environment = key.EnvironmentID
			if key.BuildID != build {
				return a, ErrDenied
			}
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1`+preparationSourceJoinsSQL+` WHERE b.environment_id=$4 AND b.id=$5 AND `+eligiblePreparationSourceSQL+`)`,
				host.HostID, host.GroupID, host.Epoch, key.EnvironmentID, key.BuildID).Scan(&a.authorizedHolder)
		}
		if err != nil {
			return a, err
		}
		var org uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, a.environment).Scan(&org); err != nil {
			return a, hideMissing(err)
		}
		a.scope, err = computerkey.EncryptionScope(org.String(), a.environment.String())
		return a, err
	})
}
