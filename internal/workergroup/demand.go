package workergroup

import (
	"context"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
)

// HasQueuedDemand is an advisory idle-drain guard for the Group's region.
// Placement still revalidates resources, lifecycle and authority transactionally.
func HasQueuedDemand(ctx context.Context, database db.DBTX, workerGroupID uuid.UUID) (bool, error) {
	var present bool
	err := database.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM computer_preparations preparation
 JOIN environments e ON e.id=preparation.environment_id AND e.retired_at IS NULL
 JOIN projects project ON project.id=e.project_id
 JOIN worker_groups g ON g.region_id=project.default_region_id AND g.id=$1
 WHERE preparation.status='queued' AND preparation.worker_host_id IS NULL AND preparation.deadline_at>clock_timestamp()
 ) OR EXISTS(
 SELECT 1 FROM sessions s
 JOIN environments e ON e.id=s.environment_id AND e.retired_at IS NULL
 JOIN projects project ON project.id=e.project_id
 JOIN worker_groups g ON g.region_id=project.default_region_id AND g.id=$1
 JOIN deployments d ON (d.environment_id,d.id)=(s.environment_id,s.deployment_id) AND d.execution_revoked_at IS NULL
 JOIN computers c ON (c.environment_id,c.id)=(s.environment_id,s.computer_id)
 WHERE s.status IN ('open','closing') AND c.deleted_at IS NULL AND c.integrity_fault_at IS NULL AND c.preparation_failed_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM computer_secret_revocations revoked WHERE revoked.environment_id=c.environment_id AND revoked.computer_id=c.id)
 AND NOT EXISTS(WITH RECURSIVE ancestors AS (
   SELECT s.id,s.parent_session_id
   UNION ALL SELECT parent.id,parent.parent_session_id FROM sessions parent JOIN ancestors a ON parent.id=a.parent_session_id WHERE parent.environment_id=s.environment_id)
   SELECT 1 FROM session_holds hold JOIN ancestors a ON a.id=hold.session_id
   WHERE hold.environment_id=s.environment_id AND hold.released_at IS NULL AND (hold.session_id=s.id OR hold.scope='subtree'))
 AND ((EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.status='queued')
     AND (NOT EXISTS(SELECT 1 FROM session_processes p WHERE p.environment_id=s.environment_id AND p.session_id=s.id
       AND (p.fenced_at IS NULL OR p.epoch=9223372036854775807))
       OR (EXISTS(SELECT 1 FROM computer_checkpoints cp WHERE cp.environment_id=c.environment_id AND cp.computer_id=c.id AND cp.status IN ('ready','restoring'))
         AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id
           AND (l.fenced_at IS NULL OR l.epoch=9223372036854775807)))))
   OR (c.initial_root_id IS NOT NULL AND c.image_id IS NOT NULL AND c.recovery_save_id IS NULL
     AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=c.environment_id AND l.computer_id=c.id
       AND (l.fenced_at IS NULL OR l.initialized_at IS NOT NULL OR l.epoch=9223372036854775807))))
 ) OR EXISTS(
 SELECT 1 FROM computer_commands cmd JOIN computers c ON (c.environment_id,c.id)=(cmd.environment_id,cmd.computer_id)
 JOIN environments e ON e.id=c.environment_id AND e.retired_at IS NULL
 JOIN projects p ON p.id=e.project_id JOIN worker_groups g ON g.region_id=p.default_region_id AND g.id=$1
 WHERE cmd.status='pending' AND c.deleted_at IS NULL AND c.integrity_fault_at IS NULL AND c.preparation_failed_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM computer_secret_revocations r WHERE r.environment_id=c.environment_id AND r.computer_id=c.id)
 )`, workerGroupID).Scan(&present)
	if err != nil {
		return false, fmt.Errorf("read queued execution demand: %w", err)
	}
	return present, nil
}
