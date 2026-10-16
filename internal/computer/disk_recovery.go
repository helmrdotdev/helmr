package computer

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// DiskRecoveryEligible is the durable disk authority shared by loss settlement
// and reconstruction admission. The caller holds the Computer lock, which also
// serializes payload retirement. Physical fencing, queued demand, checkpoint
// barriers and available worker capacity are separate admission requirements.
func DiskRecoveryEligible(ctx context.Context, tx pgx.Tx, env, computerID uuid.UUID) (bool, error) {
	var eligible bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM computers c LEFT JOIN computer_saves s ON (s.environment_id,s.id)=(c.environment_id,c.recovery_save_id)
 WHERE c.environment_id=$1 AND c.id=$2 AND c.deleted_at IS NULL AND c.integrity_fault_at IS NULL AND c.preparation_failed_at IS NULL AND c.image_id IS NOT NULL
 AND ((c.recovery_save_id IS NULL AND c.initial_root_id IS NOT NULL) OR (s.status='published' AND s.root_id IS NOT NULL AND s.payload_retired_at IS NULL))
 AND NOT EXISTS(SELECT 1 FROM (`+db.RevokedComputerSecretsSQL+`) revoked WHERE revoked.environment_id=c.environment_id AND revoked.computer_id=c.id))`, env, computerID).Scan(&eligible)
	return eligible, err
}
