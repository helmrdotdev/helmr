package agent

import (
	"context"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// Call only after removing a known Computer graph/checkpoint owner in this
// transaction. This is not an expiry policy for arbitrary tenant uploads.
func releaseComputerCASMembership(ctx context.Context, tx pgx.Tx, org uuid.UUID, digest string) error {
	var locked string
	// Collectors of different owners of one digest serialize before taking a fresh
	// statement snapshot. NO KEY UPDATE still allows ordinary availability FKs.
	if err := tx.QueryRow(ctx, `SELECT digest FROM cas_blobs WHERE digest=$1 FOR NO KEY UPDATE`, digest).Scan(&locked); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `DELETE FROM cas_objects c WHERE c.org_id=$1 AND c.digest=$2
 AND NOT EXISTS(SELECT 1 FROM computer_objects o WHERE o.org_id=c.org_id AND o.digest=c.digest)
 AND NOT EXISTS(SELECT 1 FROM deployment_objects d WHERE d.org_id=c.org_id AND d.digest=c.digest)
 AND NOT EXISTS(SELECT 1 FROM computer_checkpoint_objects o JOIN environments e ON e.id=o.environment_id WHERE e.org_id=c.org_id AND o.digest=c.digest)`, org, digest)
	return err
}
