package computer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/bootseed"
	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type PreparationKey struct{ EnvironmentID, BuildID uuid.UUID }

// PreparationSource grants only a short-lived read of one qualified generation.
// It conveys neither Computer write authority nor permission to execute a Program.
type PreparationSource struct {
	Key                        PreparationKey
	HostID                     uuid.UUID
	HostEpoch, PolicyRevision  int64
	ExpiresAt                  time.Time
	Manifest                   bootseed.Manifest
	Shape                      AllocationShape
	DeploymentID               uuid.UUID
	BundleDigest, RootfsDigest string
	ImageConfig                oci.RuntimeConfig
	Disk                       DiskSource
}

// This predicate is shared by discovery and exact source authorization. The
// policy acknowledgment is evidence of current local bounds, not a VM grant.
const eligiblePreparationSourceSQL = `b.status='succeeded' AND b.payload_retired_at IS NULL
 AND NOT ` + obsoleteBootSeed + ` AND ` + usableBootSeedImageSQL + `
 AND e.current_deployment_id=b.deployment_id AND project.default_region_id=g.region_id
 AND h.id=$1 AND h.worker_group_id=$2 AND h.current_epoch=$3 AND h.status='active'
 AND g.status='active' AND p.status='active' AND h.admission_paused_reason IS NULL
 AND h.resource_policy_revision=p.resource_policy_revision
 AND h.resource_observed_at>statement_timestamp()-interval '60 seconds'
 AND p.optional_cpu_millis>=b.reserved_cpu_millis AND p.optional_memory_bytes>=b.reserved_memory_bytes
 AND p.optional_disk_bytes>0 AND p.optional_vm_slots>0 AND p.optional_max_tasks>0 AND p.optional_io_bytes_per_second>0
 AND p.max_vm_slots>p.local_headroom_slots
 AND h.vm_platform_id=b.vm_platform_id AND h.per_vm_cpu_millis>=b.reserved_cpu_millis
 AND h.per_vm_memory_bytes>=b.reserved_memory_bytes AND h.per_vm_guest_ephemeral_disk_bytes>=b.reserved_scratch_bytes
 AND EXISTS(SELECT 1 FROM worker_pool_cpu_shapes shape WHERE shape.worker_pool_id=p.id
 AND shape.vcpu_count=b.vm_vcpu_count AND shape.cpu_config_digest=b.cpu_config_digest)
 AND EXISTS(SELECT 1 FROM images image WHERE image.environment_id=b.environment_id AND image.id=b.image_id
 AND NOT EXISTS(SELECT 1 FROM template_definitions t WHERE t.environment_id=b.environment_id AND t.deployment_id=b.deployment_id
 AND t.image_spec_id=b.image_spec_id AND t.cpu_millis=b.cpu_millis AND t.memory_bytes=b.memory_bytes
 AND t.max_image_age_ms IS NOT NULL AND extract(epoch FROM statement_timestamp()-image.published_at)*1000>t.max_image_age_ms))`

const preparationSourceJoinsSQL = ` FROM boot_seed_builds b
 JOIN environments e ON e.id=b.environment_id JOIN projects project ON project.id=e.project_id
 JOIN worker_hosts h ON h.id=$1 JOIN worker_groups g ON g.id=h.worker_group_id
 JOIN worker_pools p ON p.id=h.worker_pool_id `

func ListPreparationSources(ctx context.Context, q db.DBTX, host workergroup.HostPrincipal) ([]PreparationKey, error) {
	rows, err := q.Query(ctx, `WITH eligible AS (SELECT b.environment_id,b.id,
 row_number() OVER(ORDER BY b.environment_id,b.id)-1 position,count(*) OVER() total,
 COALESCE(h.resource_observation_sequence,0) sequence`+preparationSourceJoinsSQL+`
 WHERE `+eligiblePreparationSourceSQL+`)
 SELECT environment_id,id FROM eligible ORDER BY (position-sequence%total+total)%total LIMIT 32`, host.HostID, host.GroupID, host.Epoch)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (PreparationKey, error) {
		var k PreparationKey
		err := row.Scan(&k.EnvironmentID, &k.BuildID)
		return k, err
	})
}

type preparationSourcePin struct {
	source PreparationSource
	disk   computerDiskPin
}

func (b *KeyBroker) PreparationSource(ctx context.Context, host workergroup.HostPrincipal, key PreparationKey) (_ PreparationSource, resultErr error) {
	before, err := b.pinPreparationSource(ctx, host, key)
	if err != nil {
		return PreparationSource{}, err
	}
	source := before.source
	source.Disk = before.disk.source
	defer func() {
		if resultErr != nil {
			source.Disk.Clear()
		}
	}()
	for _, wrapped := range before.disk.envelopes {
		plaintext, err := b.wrapper.Unwrap(ctx, source.Disk.Scope, wrapped.id.String(), wrapped.envelope)
		if err != nil || len(plaintext) != computerkey.Size {
			clear(plaintext)
			return PreparationSource{}, errors.Join(err, ErrDenied)
		}
		source.Disk.Keys = append(source.Disk.Keys, DiskKey{ID: wrapped.id, Key: plaintext})
	}
	after, err := b.pinPreparationSource(ctx, host, key)
	if err != nil {
		return PreparationSource{}, err
	}
	if before.disk.rootID != after.disk.rootID || before.disk.source.Root != after.disk.source.Root || source.Disk.Scope != after.disk.source.Scope || source.PolicyRevision != after.source.PolicyRevision || len(before.disk.envelopes) != len(after.disk.envelopes) || !bytes.Equal(source.Manifest.Config, after.source.Manifest.Config) {
		return PreparationSource{}, ErrConflict
	}
	for i, old := range before.disk.envelopes {
		current := after.disk.envelopes[i]
		if old.id != current.id || old.envelope.WrappingKeyID != current.envelope.WrappingKeyID || !bytes.Equal(old.envelope.Ciphertext, current.envelope.Ciphertext) {
			return PreparationSource{}, ErrConflict
		}
	}
	if !time.Now().Before(source.ExpiresAt) {
		return PreparationSource{}, ErrDenied
	}
	return source, nil
}

func (b *KeyBroker) pinPreparationSource(ctx context.Context, host workergroup.HostPrincipal, key PreparationKey) (preparationSourcePin, error) {
	var result preparationSourcePin
	if key.EnvironmentID == uuid.Nil() || key.BuildID == uuid.Nil() {
		return result, ErrInvalidInput
	}
	err := db.RunTx(ctx, b.database, func(tx pgx.Tx) error {
		if err := shortLockTimeout(ctx, tx); err != nil {
			return err
		}
		if err := LockHost(ctx, tx, host); err != nil {
			return err
		}
		if _, err := LockBootSeedBuild(ctx, tx, key.EnvironmentID, key.BuildID); err != nil {
			return err
		}
		var manifest, seed []byte
		var org uuid.UUID
		s := &result.source
		s.Key = key
		s.HostID = host.HostID
		s.HostEpoch = host.Epoch
		err := tx.QueryRow(ctx, `SELECT p.resource_policy_revision,clock_timestamp()+interval '120 seconds',b.manifest,
 b.reserved_cpu_millis,b.reserved_memory_bytes,b.reserved_scratch_bytes,b.vm_platform_id,b.vm_vcpu_count,b.cpu_config_digest,
 b.deployment_id,d.bundle_digest,platform.rootfs_digest,spec.seed,e.org_id,r.id,`+DiskRootColumns+preparationSourceJoinsSQL+`
 JOIN deployments d ON (d.environment_id,d.id)=(b.environment_id,b.deployment_id)
 JOIN image_specs spec ON (spec.environment_id,spec.id)=(b.environment_id,b.image_spec_id)
 JOIN vm_platforms platform ON platform.id=b.vm_platform_id
 JOIN images image ON (image.environment_id,image.id)=(b.environment_id,b.image_id)
 JOIN disk_roots r ON (r.environment_id,r.id)=(image.environment_id,image.root_id)
 WHERE b.environment_id=$4 AND b.id=$5 AND `+eligiblePreparationSourceSQL, host.HostID, host.GroupID, host.Epoch, key.EnvironmentID, key.BuildID).
			Scan(append([]any{&s.PolicyRevision, &s.ExpiresAt, &manifest, &s.Shape.CPUMillis, &s.Shape.MemoryBytes, &s.Shape.ScratchBytes, &s.Shape.VMPlatformID, &s.Shape.VCPUCount, &s.Shape.CPUConfigDigest, &s.DeploymentID, &s.BundleDigest, &s.RootfsDigest, &seed, &org, &result.disk.rootID}, DiskRootFields(&result.disk.source.Root)...)...)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(manifest, &s.Manifest); err != nil {
			return err
		}
		if err = s.Manifest.Validate(); err != nil {
			return err
		}
		if s.Manifest.EnvironmentID != key.EnvironmentID || s.Manifest.BuildID != key.BuildID {
			return ErrConflict
		}
		var definition definition.ComputerSeedManifest
		if err = json.Unmarshal(seed, &definition); err != nil {
			return err
		}
		s.ImageConfig = definition.Config
		result.disk.source.Scope, err = computerkey.EncryptionScope(org.String(), key.EnvironmentID.String())
		if err != nil {
			return err
		}
		result.disk.source.BaseVersion, err = result.disk.source.Root.Digest()
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT k.id,k.wrapping_key_id,k.wrapped_key,k.available FROM disk_encryption_keys k
 WHERE k.environment_id=$1 AND k.id IN (SELECT key_id FROM disk_object_keys WHERE environment_id=$1 AND digest=$2)
 ORDER BY k.id FOR KEY SHARE`, key.EnvironmentID, result.disk.source.Root.Pack.Digest)
		if err != nil {
			return err
		}
		result.disk.envelopes, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (computerKeyEnvelope, error) {
			var k computerKeyEnvelope
			var available bool
			err := row.Scan(&k.id, &k.envelope.WrappingKeyID, &k.envelope.Ciphertext, &available)
			if err == nil && (!available || len(k.envelope.Ciphertext) == 0) {
				err = ErrDenied
			}
			return k, err
		})
		if err != nil {
			return err
		}
		for _, k := range result.disk.envelopes {
			if k.id.String() == result.disk.source.Root.Page.KeyID {
				return nil
			}
		}
		return ErrDenied
	})
	return result, hideMissing(err)
}

// PreparationStart reads generation metadata before Program artifact lookup.
// PreparationSource repeats authorization after that lookup and key unwrap.
func (b *KeyBroker) PreparationStart(ctx context.Context, host workergroup.HostPrincipal, key PreparationKey) (BootSeedStart, error) {
	pin, err := b.pinPreparationSource(ctx, host, key)
	if err != nil {
		return BootSeedStart{}, err
	}
	return BootSeedStart{DeploymentID: pin.source.DeploymentID, BundleDigest: pin.source.BundleDigest, RootfsDigest: pin.source.RootfsDigest, ImageConfig: pin.source.ImageConfig}, nil
}
