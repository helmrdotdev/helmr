package computer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/bootseed"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type BootSeedPublisher struct {
	database db.TxBeginner
	objects  cas.Reader
}

func NewBootSeedPublisher(database db.TxBeginner, objects cas.Reader) (*BootSeedPublisher, error) {
	if database == nil || objects == nil {
		return nil, errors.New("boot seed publication requires database and immutable storage")
	}
	return &BootSeedPublisher{database: database, objects: objects}, nil
}

// Publish requires fresh independent clone qualification and storage-confirmed
// encrypted objects. The same owning executor may reconcile a committed result
// after a lost reply without generating or qualifying another seed.
func (p *BootSeedPublisher) Publish(ctx context.Context, host workergroup.HostPrincipal, ref BootSeedExecutor, m bootseed.Manifest, qualification bootseed.Qualification) error {
	if !ref.Valid() || m.EnvironmentID != ref.EnvironmentID || m.BuildID != ref.BuildID {
		return ErrInvalidInput
	}
	if err := m.Validate(); err != nil {
		return ErrInvalidInput
	}
	if err := qualification.Validate(); err != nil {
		return ErrInvalidInput
	}
	content, err := m.Content()
	if err != nil || content.Program == nil {
		return ErrInvalidInput
	}
	raw, err := json.Marshal(m)
	if err != nil || len(raw) > 1<<20 {
		return ErrInvalidInput
	}
	evidence, err := json.Marshal(qualification)
	if err != nil {
		return err
	}
	check := func(tx pgx.Tx) (bool, error) {
		if err := LockHost(ctx, tx, host); err != nil {
			return false, err
		}
		build, err := LockBootSeedBuild(ctx, tx, ref.EnvironmentID, ref.BuildID)
		if err != nil {
			return false, err
		}
		if err := CheckBootSeedExecutor(ctx, tx, host, ref, false); err != nil {
			return false, err
		}
		if build.Status == "succeeded" {
			var stored []byte
			var proof string
			if err := tx.QueryRow(ctx, `SELECT manifest,qualification_evidence FROM boot_seed_builds WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, ref.BuildID).Scan(&stored, &proof); err != nil {
				return false, err
			}
			if !bytes.Equal(stored, raw) || proof != string(evidence) {
				return false, ErrConflict
			}
			return true, nil
		}
		if err := CheckBootSeedExecutor(ctx, tx, host, ref, true); err != nil {
			return false, err
		}
		var root disk.VersionRoot
		var shape AllocationShape
		if err := tx.QueryRow(ctx, `SELECT b.reserved_cpu_millis,b.reserved_memory_bytes,b.reserved_scratch_bytes,b.vm_platform_id,b.vm_vcpu_count,b.cpu_config_digest,`+DiskRootColumns+`
 FROM boot_seed_builds b JOIN images i ON (i.environment_id,i.id)=(b.environment_id,b.image_id)
 JOIN disk_roots r ON (r.environment_id,r.id)=(i.environment_id,i.root_id) WHERE b.environment_id=$1 AND b.id=$2`, ref.EnvironmentID, ref.BuildID).
			Scan(append([]any{&shape.CPUMillis, &shape.MemoryBytes, &shape.ScratchBytes, &shape.VMPlatformID, &shape.VCPUCount, &shape.CPUConfigDigest}, DiskRootFields(&root)...)...); err != nil {
			return false, err
		}
		rootDigest, err := root.Digest()
		if err != nil {
			return false, err
		}
		if m.MemoryBytes != shape.MemoryBytes || m.ScratchBytes != shape.ScratchBytes || content.RuntimeID != shape.VMPlatformID ||
			content.CPUConfig != shape.CPUConfigDigest || content.VCPUs != shape.VCPUCount ||
			content.ComputerRoot != rootDigest || content.ComputerBytes != root.LogicalBytes {
			return false, ErrConflict
		}
		// Program objects are deployment-owned; Runtime bytes are separately
		// platform-owned. Selection compares both against the admitted cohort.
		for _, object := range []cas.Descriptor{cas.Descriptor(content.Program.Artifact)} {
			var retained bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM deployment_objects WHERE environment_id=$1 AND deployment_id=$2 AND digest=$3 AND size_bytes=$4 AND media_type=$5)`,
				ref.EnvironmentID, build.DeploymentID, object.Digest, object.SizeBytes, object.MediaType).Scan(&retained); err != nil {
				return false, err
			}
			if !retained {
				return false, ErrConflict
			}
		}
		return false, nil
	}
	var done bool
	if err := db.RunTx(ctx, p.database, func(tx pgx.Tx) error {
		var err error
		done, err = check(tx)
		return err
	}); err != nil || done {
		return hideMissing(err)
	}
	for _, object := range m.Objects() {
		actual, err := p.objects.Stat(ctx, object.Digest)
		if err != nil {
			return err
		}
		if err := cas.RequireExact(actual, object.Descriptor); err != nil {
			return err
		}
	}
	return hideMissing(db.RunTx(ctx, p.database, func(tx pgx.Tx) error {
		done, err := check(tx)
		if err != nil || done {
			return err
		}
		var org uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, ref.EnvironmentID).Scan(&org); err != nil {
			return err
		}
		for _, object := range m.Objects() {
			if _, err := db.New(tx).UpsertCasObject(ctx, db.UpsertCasObjectParams{OrgID: pgvalue.UUID(org), Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType}); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO boot_seed_objects(environment_id,build_id,role,digest,size_bytes,media_type) VALUES($1,$2,$3,$4,$5,$6)`,
				ref.EnvironmentID, ref.BuildID, object.Role, object.Digest, object.SizeBytes, object.MediaType); err != nil {
				return err
			}
		}
		if err := CheckBootSeedExecutor(ctx, tx, host, ref, true); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE boot_seed_builds SET status='succeeded',manifest=$3,qualification_evidence=$4 WHERE environment_id=$1 AND id=$2`,
			ref.EnvironmentID, ref.BuildID, raw, string(evidence)); err != nil {
			return err
		}
		return notifyBootSeedChanged(ctx, tx, ref.EnvironmentID, ref.BuildID)
	}))
}
