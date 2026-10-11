package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type ComputerBoot struct {
	ImageConfig  oci.RuntimeConfig
	RootfsDigest string
}

// ReadComputerBoot reads only immutable Computer image and VM metadata. A cold
// command boot never needs a Session's executable bundle or setup permission.
func ReadComputerBoot(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, identity ComputerLeaseIdentity) (ComputerBoot, error) {
	if !identity.valid() {
		return ComputerBoot{}, ErrInvalidInput
	}
	var result ComputerBoot
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		var raw []byte
		var arch, contract string
		if err := tx.QueryRow(ctx, `SELECT spec.seed,platform.arch,platform.contract,platform.rootfs_digest
 FROM computer_leases l JOIN computers c ON (c.environment_id,c.id)=(l.environment_id,l.computer_id)
 JOIN computer_preparation_specs spec ON (spec.environment_id,spec.id)=(c.environment_id,c.preparation_spec_id)
 JOIN vm_platforms platform ON platform.id=l.vm_platform_id
 WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3 AND c.deleted_at IS NULL AND c.integrity_fault_at IS NULL
 FOR NO KEY UPDATE OF c`, identity.EnvironmentID, identity.ComputerID, identity.Epoch).Scan(&raw, &arch, &contract, &result.RootfsDigest); err != nil {
			return err
		}
		if err := checkComputerDiskAuthority(ctx, tx, host, identity); err != nil {
			return err
		}
		var seed definition.ComputerSeedManifest
		if err := json.Unmarshal(raw, &seed); err != nil {
			return err
		}
		if seed.Profile != definition.ComputerSeedProfile || seed.MediaType != definition.ComputerSeedMediaType || !sha256sum.ValidDigest(seed.ArtifactDigest) || arch != string(definition.ArchitectureX8664) || contract != vmplatform.Contract || !sha256sum.ValidDigest(result.RootfsDigest) {
			return errors.New("computer boot metadata violates its admitted image or platform contract")
		}
		result.ImageConfig = seed.Config
		return nil
	})
	if err != nil {
		return ComputerBoot{}, allocationError(err)
	}
	return result, nil
}
