package agent

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type PreparationStart struct {
	Spec         artifact.ComputerPreparationSpec
	DeploymentID uuid.UUID
	BundleDigest string
	RootfsDigest string
}

// ReadPreparationStart resolves executable inputs from the admitted immutable
// spec. A deployment is only a container for its verified bundle; no Session
// or caller-selected Program can replace the preparation's content identity.
func ReadPreparationStart(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, ref PreparationExecutor) (PreparationStart, error) {
	if !ref.valid() {
		return PreparationStart{}, ErrInvalidInput
	}
	var result PreparationStart
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		if err := lockPreparationSecretOwners(ctx, tx, ref); err != nil {
			return err
		}
		if err := lockPreparationExecutorOnHost(ctx, tx, host, ref, true); err != nil {
			return err
		}
		var raw []byte
		var arch, contract string
		var sealed bool
		if err := tx.QueryRow(ctx, `SELECT spec.spec,d.deployment_id,deployment.bundle_digest,platform.rootfs_digest,platform.arch,platform.contract,p.logical_bytes IS NOT NULL
 FROM computer_preparations p JOIN computer_preparation_specs spec ON (spec.environment_id,spec.id)=(p.environment_id,p.preparation_spec_id)
 JOIN computer_definitions d ON (d.environment_id,d.preparation_spec_id)=(spec.environment_id,spec.id)
 JOIN deployments deployment ON (deployment.environment_id,deployment.id)=(d.environment_id,d.deployment_id)
 JOIN vm_platforms platform ON platform.id=p.vm_platform_id
 WHERE p.environment_id=$1 AND p.id=$2 ORDER BY d.deployment_id,d.definition_key LIMIT 1`, ref.EnvironmentID, ref.PreparationID).Scan(&raw, &result.DeploymentID, &result.BundleDigest, &result.RootfsDigest, &arch, &contract, &sealed); err != nil {
			return err
		}
		if sealed {
			return ErrDenied
		}
		if arch != string(definition.ArchitectureX8664) || contract != vmplatform.Contract || !sha256sum.ValidDigest(result.RootfsDigest) || !sha256sum.ValidDigest(result.BundleDigest) {
			return errors.New("preparation executable or platform identity is invalid")
		}
		var err error
		result.Spec, err = artifact.ParseComputerPreparationSpec(raw)
		if err != nil {
			return err
		}
		return checkPreparationExecutor(ctx, tx, host, ref, true)
	})
	if err != nil {
		return PreparationStart{}, hideMissing(err)
	}
	return result, nil
}
