package computer

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type ImageBuildStart struct {
	Spec         artifact.ComputerPreparationSpec
	DeploymentID uuid.UUID
	BundleDigest string
	RootfsDigest string
}

// ReadImageBuildStart resolves executable inputs from the admitted immutable
// spec. A deployment is only a container for its verified bundle; no Session
// or caller-selected Program can replace the preparation's content identity.
func ReadImageBuildStart(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, ref ImageBuildExecutor) (ImageBuildStart, error) {
	if !ref.Valid() {
		return ImageBuildStart{}, ErrInvalidInput
	}
	var result ImageBuildStart
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := LockHost(ctx, tx, host); err != nil {
			return err
		}
		if err := lockImageBuildSecretOwners(ctx, tx, ref); err != nil {
			return err
		}
		if err := lockImageBuildExecutorOnHost(ctx, tx, host, ref, true); err != nil {
			return err
		}
		var raw []byte
		var sealed bool
		if err := tx.QueryRow(ctx, `SELECT spec.spec,d.deployment_id,deployment.bundle_digest,platform.rootfs_digest,p.logical_bytes IS NOT NULL
 FROM image_builds p JOIN image_specs spec ON (spec.environment_id,spec.id)=(p.environment_id,p.image_spec_id)
 JOIN template_definitions d ON (d.environment_id,d.image_spec_id)=(spec.environment_id,spec.id)
 JOIN deployments deployment ON (deployment.environment_id,deployment.id)=(d.environment_id,d.deployment_id)
 JOIN vm_platforms platform ON platform.id=p.vm_platform_id
 WHERE p.environment_id=$1 AND p.id=$2 ORDER BY d.deployment_id,d.template_id LIMIT 1`, ref.EnvironmentID, ref.ImageBuildID).Scan(&raw, &result.DeploymentID, &result.BundleDigest, &result.RootfsDigest, &sealed); err != nil {
			return err
		}
		if sealed {
			return ErrDenied
		}
		if !sha256sum.ValidDigest(result.RootfsDigest) || !sha256sum.ValidDigest(result.BundleDigest) {
			return errors.New("preparation executable or platform identity is invalid")
		}
		var err error
		result.Spec, err = artifact.ParseComputerPreparationSpec(raw)
		if err != nil {
			return err
		}
		return CheckImageBuildExecutor(ctx, tx, host, ref, true)
	})
	if err != nil {
		return ImageBuildStart{}, hideMissing(err)
	}
	return result, nil
}

// StartImageBuild records the first authenticated start after executable inputs
// have been verified, immediately before the transport returns them to the host.
// It rechecks authority after external I/O; replay preserves the first timestamp.
func StartImageBuild(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, ref ImageBuildExecutor) error {
	if !ref.Valid() {
		return ErrInvalidInput
	}
	return hideMissing(db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := LockHost(ctx, tx, host); err != nil {
			return err
		}
		if err := lockImageBuildSecretOwners(ctx, tx, ref); err != nil {
			return err
		}
		if err := lockImageBuildExecutorOnHost(ctx, tx, host, ref, true); err != nil {
			return err
		}
		var sealed bool
		if err := tx.QueryRow(ctx, `SELECT logical_bytes IS NOT NULL FROM image_builds WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, ref.ImageBuildID).Scan(&sealed); err != nil {
			return err
		}
		if sealed {
			return ErrDenied
		}
		if err := CheckImageBuildExecutor(ctx, tx, host, ref, true); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE image_builds SET started_at=COALESCE(started_at,clock_timestamp()) WHERE environment_id=$1 AND id=$2`, ref.EnvironmentID, ref.ImageBuildID)
		return err
	}))
}
