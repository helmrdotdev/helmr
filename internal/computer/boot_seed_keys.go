package computer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// BootSeedSource exposes only the published image's existing key closure.
// Qualification writes are disposable and use a fresh local key, never a
// durable image-build or Computer write key.
func (b *KeyBroker) BootSeedSource(ctx context.Context, host workergroup.HostPrincipal, ref BootSeedExecutor) (_ DiskSource, resultErr error) {
	pin, err := b.pinBootSeed(ctx, host, ref)
	if err != nil {
		return DiskSource{}, err
	}
	source := pin.source
	defer func() {
		if resultErr != nil {
			source.Clear()
		}
	}()
	for _, wrapped := range pin.envelopes {
		key, err := b.wrapper.Unwrap(ctx, source.Scope, wrapped.id.String(), wrapped.envelope)
		if err != nil || len(key) != computerkey.Size {
			clear(key)
			return DiskSource{}, errors.Join(err, ErrDenied)
		}
		source.Keys = append(source.Keys, DiskKey{ID: wrapped.id, Key: key})
	}
	current, err := b.pinBootSeed(ctx, host, ref)
	if err != nil {
		return DiskSource{}, err
	}
	if current.rootID != pin.rootID || current.source.Root != source.Root || current.source.Scope != source.Scope || len(current.envelopes) != len(pin.envelopes) {
		return DiskSource{}, ErrConflict
	}
	for i, before := range pin.envelopes {
		after := current.envelopes[i]
		if before.id != after.id || before.envelope.WrappingKeyID != after.envelope.WrappingKeyID || !bytes.Equal(before.envelope.Ciphertext, after.envelope.Ciphertext) {
			return DiskSource{}, ErrConflict
		}
	}
	return source, nil
}

func (b *KeyBroker) pinBootSeed(ctx context.Context, host workergroup.HostPrincipal, ref BootSeedExecutor) (computerDiskPin, error) {
	var result computerDiskPin
	err := withBootSeedExecutor(ctx, b.database, host, ref, func(tx pgx.Tx) error {
		var org uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT e.org_id,r.id,`+DiskRootColumns+` FROM boot_seed_builds b
 JOIN environments e ON e.id=b.environment_id JOIN images i ON (i.environment_id,i.id)=(b.environment_id,b.image_id)
 JOIN disk_roots r ON (r.environment_id,r.id)=(i.environment_id,i.root_id)
 WHERE b.environment_id=$1 AND b.id=$2`, ref.EnvironmentID, ref.BuildID).
			Scan(append([]any{&org, &result.rootID}, DiskRootFields(&result.source.Root)...)...); err != nil {
			return err
		}
		var err error
		result.source.Scope, err = computerkey.EncryptionScope(org.String(), ref.EnvironmentID.String())
		if err != nil {
			return err
		}
		result.source.BaseVersion, err = result.source.Root.Digest()
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT k.id,k.wrapping_key_id,k.wrapped_key,k.available FROM disk_encryption_keys k
 WHERE k.environment_id=$1 AND k.id IN (SELECT key_id FROM disk_object_keys WHERE environment_id=$1 AND digest=$2)
 ORDER BY k.id FOR KEY SHARE`, ref.EnvironmentID, result.source.Root.Pack.Digest)
		if err != nil {
			return err
		}
		result.envelopes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (computerKeyEnvelope, error) {
			var key computerKeyEnvelope
			var available bool
			if err := row.Scan(&key.id, &key.envelope.WrappingKeyID, &key.envelope.Ciphertext, &available); err != nil {
				return key, err
			}
			if !available || len(key.envelope.Ciphertext) == 0 {
				return key, ErrDenied
			}
			return key, nil
		})
		if err != nil {
			return err
		}
		for _, key := range result.envelopes {
			if key.id.String() == result.source.Root.Page.KeyID {
				return nil
			}
		}
		return ErrDenied
	})
	return result, err
}

type BootSeedStart struct {
	DeploymentID uuid.UUID
	BundleDigest string
	RootfsDigest string
	ImageConfig  oci.RuntimeConfig
}

func ReadBootSeedStart(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, ref BootSeedExecutor) (BootSeedStart, error) {
	var result BootSeedStart
	err := withBootSeedExecutor(ctx, database, host, ref, func(tx pgx.Tx) error {
		var seedRaw []byte
		if err := tx.QueryRow(ctx, `SELECT b.deployment_id,d.bundle_digest,p.rootfs_digest,s.seed FROM boot_seed_builds b
 JOIN deployments d ON (d.environment_id,d.id)=(b.environment_id,b.deployment_id)
 JOIN image_specs s ON (s.environment_id,s.id)=(b.environment_id,b.image_spec_id)
 JOIN vm_platforms p ON p.id=b.vm_platform_id WHERE b.environment_id=$1 AND b.id=$2`, ref.EnvironmentID, ref.BuildID).
			Scan(&result.DeploymentID, &result.BundleDigest, &result.RootfsDigest, &seedRaw); err != nil {
			return err
		}
		var seed definition.ComputerSeedManifest
		if err := json.Unmarshal(seedRaw, &seed); err != nil {
			return err
		}
		result.ImageConfig = seed.Config
		return nil
	})
	return result, err
}

func StartBootSeedBuild(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, ref BootSeedExecutor) error {
	return withBootSeedExecutor(ctx, database, host, ref, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE boot_seed_builds SET started_at=COALESCE(started_at,clock_timestamp()) WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, ref.BuildID)
		return err
	})
}

func withBootSeedExecutor(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, ref BootSeedExecutor, fn func(pgx.Tx) error) error {
	if !ref.Valid() {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := shortLockTimeout(ctx, tx); err != nil {
			return err
		}
		if err := LockHost(ctx, tx, host); err != nil {
			return err
		}
		if _, err := LockBootSeedBuild(ctx, tx, ref.EnvironmentID, ref.BuildID); err != nil {
			return err
		}
		if err := CheckBootSeedExecutor(ctx, tx, host, ref, true); err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return err
		}
		return CheckBootSeedExecutor(ctx, tx, host, ref, true)
	}))
}
