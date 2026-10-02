package computer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// SeedPreparation reports either an adopted initial root, another Instance's
// conversion in progress, or this Instance's exclusive conversion key. It is
// host-only material. A conversion owner clears Key.Key after use.
type SeedPreparation struct {
	Status string
	Key    KeyMaterial
}

type seedPin struct {
	status, scope string
	key           db.ComputerDataKey
	absent        bool
}

// PrepareSeed joins a conversion under the preparing Instance's ordinary
// deadline. The Computer's private write key remains separate from the seed key.
// Provider work occurs outside SQL locks; key release rechecks the exact claim.
func (b *KeyBroker) PrepareSeed(ctx context.Context, principal workergroup.HostPrincipal, ref PreparationRef) (SeedPreparation, error) {
	_, err := b.ensureInitialKey(ctx, principal, ref)
	if err != nil {
		return b.seedPreparationReplay(ctx, principal, ref, err)
	}
	pin, err := b.pinSeed(ctx, principal, ref, nil, "")
	if err != nil {
		return b.seedPreparationReplay(ctx, principal, ref, err)
	}
	if pin.absent {
		plain := make([]byte, computerkey.Size)
		if _, err = rand.Read(plain); err != nil {
			return SeedPreparation{}, err
		}
		id := pgvalue.UUID(uuid.NewV7())
		envelope, wrapErr := b.wrapper.Wrap(ctx, pin.scope, pgvalue.UUIDString(id), plain)
		clear(plain)
		if wrapErr != nil {
			return SeedPreparation{}, providerFailure(ctx, "wrap seed key", wrapErr)
		}
		candidate := db.ComputerDataKey{ID: id, WrappingKeyID: envelope.WrappingKeyID, WrappedKey: envelope.Ciphertext}
		pin, err = b.pinSeed(ctx, principal, ref, &candidate, pin.scope)
		if err != nil {
			return b.seedPreparationReplay(ctx, principal, ref, err)
		}
	}
	if pin.status != "convert" {
		return SeedPreparation{Status: pin.status}, nil
	}
	id := pgvalue.UUIDString(pin.key.ID)
	plain, err := b.wrapper.Unwrap(ctx, pin.scope, id, computerkey.Envelope{WrappingKeyID: pin.key.WrappingKeyID, Ciphertext: pin.key.WrappedKey})
	if err != nil {
		clear(plain)
		return SeedPreparation{}, providerFailure(ctx, "unwrap seed key", err)
	}
	if err = providerKeyLength(plain); err != nil {
		clear(plain)
		return SeedPreparation{}, err
	}
	current, err := b.pinSeed(ctx, principal, ref, nil, "")
	if err != nil || current.status != "convert" || current.absent || current.scope != pin.scope || current.key.ID != pin.key.ID || current.key.WrappingKeyID != pin.key.WrappingKeyID || !bytes.Equal(current.key.WrappedKey, pin.key.WrappedKey) {
		clear(plain)
		if err != nil {
			return SeedPreparation{}, err
		}
		return SeedPreparation{}, ErrAuthorityChanged
	}
	return SeedPreparation{Status: "convert", Key: KeyMaterial{Scope: pin.scope, ID: id, Key: plain}}, nil
}

// A lost response or concurrent exact request may already have adopted a root.
// Only that initial receipt at this desired version permits a ready replay.
func (b *KeyBroker) seedPreparationReplay(ctx context.Context, principal workergroup.HostPrincipal, ref PreparationRef, original error) (SeedPreparation, error) {
	if !errors.Is(original, ErrAuthorityChanged) && !errors.Is(original, pgx.ErrNoRows) {
		return SeedPreparation{}, original
	}
	err := db.RunTx(ctx, b.txb, func(tx pgx.Tx) error {
		fence, err := lockSourceFence(ctx, tx, principal, ref)
		if err != nil {
			return err
		}
		p, err := fence.claim(ctx, principal)
		if err != nil {
			return err
		}
		if !p.instance.InitialDiskVersionID.Valid || p.instance.InitialPublicationDesiredVersion.Int64 != ref.DesiredVersion {
			return pgx.ErrNoRows
		}
		return p.checkDeadlines(ctx)
	})
	if err != nil {
		return SeedPreparation{}, original
	}
	return SeedPreparation{Status: "ready"}, nil
}

func (b *KeyBroker) pinSeed(ctx context.Context, principal workergroup.HostPrincipal, ref PreparationRef, candidate *db.ComputerDataKey, expectedScope string) (seedPin, error) {
	var pin seedPin
	err := db.RunTx(ctx, b.txb, func(tx pgx.Tx) error {
		p, err := lockInitialPreparation(ctx, tx, principal, ref)
		if err != nil {
			return err
		}
		pin.scope, err = p.encryptionScope()
		if err != nil {
			return err
		}
		if expectedScope != "" && expectedScope != pin.scope {
			return ErrAuthorityChanged
		}
		seed, err := p.lockSeed(ctx)
		if err != nil {
			return err
		}
		if seed.RootID.Valid {
			if p.instance.SeedID.Valid {
				return ErrAuthorityChanged
			}
			if err = p.adoptSeed(ctx, seed); err != nil {
				return err
			}
			pin.status = "ready"
			return p.checkDeadlines(ctx)
		}
		if seed.PreparationInstanceID == p.instance.ID {
			if err = p.checkSeedClaim(ctx, seed); err != nil {
				return err
			}
			pin.key, err = db.New(tx).GetComputerSeedKey(ctx, p.instance.ID)
			if err != nil {
				return err
			}
			pin.status = "convert"
			return p.checkDeadlines(ctx)
		}
		if p.instance.SeedID.Valid {
			return ErrAuthorityChanged
		}
		var occupied bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_instances i WHERE i.id=$1
   AND i.reclaimed_at IS NULL AND i.desired_state='ready' AND i.observed_state='allocated'
   AND i.writer_expires_at>clock_timestamp() AND i.preparation_expires_at>clock_timestamp()
   AND $2::timestamptz>clock_timestamp())`, seed.PreparationInstanceID, seed.LeaseExpiresAt).Scan(&occupied)
		if err != nil {
			return err
		}
		if occupied {
			pin.status = "waiting"
			return p.checkDeadlines(ctx)
		}
		// One immutable attempt per physical Instance. A superseded claimant must
		// reconcile and be replaced; it cannot reuse an old publication identity.
		if candidate == nil {
			pin.absent = true
			return p.checkDeadlines(ctx)
		}
		_, err = tx.Exec(ctx, `INSERT INTO computer_data_keys(id,environment_id,wrapping_key_id,wrapped_key)
   VALUES($1,$2,$3,$4)`, candidate.ID, p.environmentID, candidate.WrappingKeyID, candidate.WrappedKey)
		if err != nil {
			return err
		}
		generation := seed.PreparationGeneration + 1
		_, err = tx.Exec(ctx, `UPDATE computer_instances SET seed_id=$2,seed_preparation_generation=$3,seed_key_id=$4 WHERE id=$1`, p.instance.ID, seed.ID, generation, candidate.ID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE computer_seeds SET preparation_generation=$2,preparation_instance_id=$3,
   preparation_key_id=$4,lease_expires_at=$5 WHERE id=$1`, seed.ID, generation, p.instance.ID, candidate.ID, p.instance.PreparationExpiresAt)
		if err != nil {
			return err
		}
		pin.status = "convert"
		pin.key = *candidate
		return p.checkDeadlines(ctx)
	})
	return pin, authorityChanged(err)
}

func (p initialPreparation) lockSeed(ctx context.Context) (db.ComputerSeed, error) {
	return db.New(p.tx).LockComputerSeed(ctx, db.LockComputerSeedParams{EnvironmentID: p.environmentID, ComputerSpecID: p.instance.ComputerSpecID, LogicalBytes: p.logicalBytes})
}

func (p initialPreparation) checkSeedClaim(ctx context.Context, seed db.ComputerSeed) error {
	if seed.RootID.Valid || seed.PreparationInstanceID != p.instance.ID || seed.PreparationGeneration != p.instance.SeedPreparationGeneration.Int64 || seed.PreparationKeyID != p.instance.SeedKeyID {
		return ErrAuthorityChanged
	}
	var valid bool
	if err := p.tx.QueryRow(ctx, `SELECT $1::timestamptz>clock_timestamp()`, seed.LeaseExpiresAt).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return ErrAuthorityChanged
	}
	return p.checkDeadlines(ctx)
}

// initialConfig is resolved from the admitted immutable specification, not from
// the converter or a different Computer that happened to use the same seed.
func (p initialPreparation) initialConfig(ctx context.Context) ([]byte, error) {
	var raw []byte
	if err := p.tx.QueryRow(ctx, `SELECT config FROM computer_specs WHERE environment_id=$1 AND id=$2`, p.environmentID, p.instance.ComputerSpecID).Scan(&raw); err != nil {
		return nil, err
	}
	config, err := definition.ParseComputerConfig(raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(config.Image)
}

func (p initialPreparation) adoptSeed(ctx context.Context, seed db.ComputerSeed) error {
	q := db.New(p.tx)
	var raw []byte
	if err := p.tx.QueryRow(ctx, `SELECT locator FROM computer_disk_roots WHERE environment_id=$1 AND id=$2 FOR KEY SHARE`, p.environmentID, seed.RootID).Scan(&raw); err != nil {
		return err
	}
	root, err := disk.ParseVersionRoot(raw, p.logicalBytes)
	if err != nil {
		return err
	}
	config, err := p.initialConfig(ctx)
	if err != nil {
		return err
	}
	var input InitialVersion
	input.Root = root
	if err = json.Unmarshal(config, &input.Config); err != nil {
		return err
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return err
	}
	fingerprint := sha256.Sum256(encoded)
	version, err := q.PublishInitialComputerDiskVersion(ctx, db.PublishInitialComputerDiskVersionParams{EnvironmentID: p.environmentID, ComputerID: p.computerID, VersionID: p.versionID, RootID: seed.RootID, ComputerInstanceID: p.instance.ID, DesiredVersion: pgtype.Int8{Int64: p.instance.DesiredVersion, Valid: true}, Fingerprint: fingerprint[:], InitialConfig: config})
	if err != nil {
		return err
	}
	n, err := q.PinInstanceComputerSource(ctx, db.PinInstanceComputerSourceParams{ComputerInstanceID: p.instance.ID, EnvironmentID: p.environmentID, ComputerID: p.computerID, VersionID: version.ID})
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("adopted seed has no matching Instance source")
	}
	return nil
}
