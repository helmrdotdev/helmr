package computer

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// ReadNetworkPolicy resolves the Computer's immutable creation definition. The
// requesting Agent or a newer publication cannot replace this authority.
func ReadNetworkPolicy(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, identity LeaseIdentity) (networkpolicy.Policy, error) {
	if !identity.Valid() {
		return networkpolicy.Policy{}, ErrInvalidInput
	}
	var result networkpolicy.Policy
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := shortLockTimeout(ctx, tx); err != nil {
			return err
		}
		if err := LockHost(ctx, tx, host); err != nil {
			return err
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT d.network_policy
 FROM computers c JOIN template_definitions d
 ON (d.environment_id,d.template_id,d.deployment_id)=(c.environment_id,c.origin_template_id,c.origin_deployment_id)
 WHERE c.environment_id=$1 AND c.id=$2 AND c.deleted_at IS NULL AND c.integrity_fault_at IS NULL
 FOR NO KEY UPDATE OF c`, identity.EnvironmentID, identity.ComputerID).Scan(&raw); err != nil {
			return err
		}
		if err := checkComputerDiskAuthority(ctx, tx, host, identity); err != nil {
			return err
		}
		var err error
		result, err = networkpolicy.DecodeStored(raw)
		return err
	})
	if err != nil {
		return networkpolicy.Policy{}, notReadyOnLock(err)
	}
	return result, nil
}
