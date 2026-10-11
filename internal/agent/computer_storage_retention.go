package agent

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
)

// The reservation belongs to the Computer, not to its shared initial image or
// historical identity. Release it after execution stops and all Computer-owned
// payload attachments retire. Independent CAS reclamation retries deletion of
// unreferenced bytes; other Computers and images still protect shared objects.
func releaseDeletedComputerStorage(ctx context.Context, database db.DBTX) error {
	_, err := database.Exec(ctx, `WITH released AS (
 SELECT c.environment_id,c.id FROM computers c
 WHERE c.deleted_at IS NOT NULL AND c.storage_reservation_bytes IS NOT NULL
 AND c.initial_root_id IS NULL AND c.recovery_save_id IS NULL
 AND NOT EXISTS(SELECT 1 FROM sessions s WHERE s.environment_id=c.environment_id AND s.computer_id=c.id AND s.status IN ('open','closing'))
 AND NOT EXISTS(SELECT 1 FROM session_processes p WHERE p.environment_id=c.environment_id AND p.computer_id=c.id AND p.fenced_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM computer_commands q WHERE q.environment_id=c.environment_id AND q.computer_id=c.id AND (q.terminal_at IS NULL OR (q.computer_lease_epoch IS NOT NULL AND q.process_reconciled_at IS NULL)))
 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id AND (l.fenced_at IS NULL OR l.retained_base_root_id IS NOT NULL))
 AND NOT EXISTS(SELECT 1 FROM computer_saves s WHERE s.environment_id=c.environment_id AND s.computer_id=c.id AND (s.status IN ('requested','captured') OR s.root_id IS NOT NULL
   OR EXISTS(SELECT 1 FROM computer_object_pins p WHERE p.environment_id=s.environment_id AND p.save_id=s.id)))
 AND NOT EXISTS(SELECT 1 FROM computer_checkpoints k WHERE k.environment_id=c.environment_id AND k.computer_id=c.id AND ((k.status NOT IN ('cancelled','lost') AND k.controls_reconciled_at IS NULL)
   OR EXISTS(SELECT 1 FROM computer_checkpoint_objects o WHERE o.environment_id=k.environment_id AND o.checkpoint_id=k.id)))
 ORDER BY c.environment_id,c.id LIMIT 100 FOR NO KEY UPDATE OF c SKIP LOCKED
 ) UPDATE computers c SET storage_reservation_bytes=NULL,updated_at=clock_timestamp()
 FROM released r WHERE c.environment_id=r.environment_id AND c.id=r.id`)
	return err
}
