-- Admission holds the Computer, instance and Run locks. Physical capacity and
-- writer ownership belong to the instance; this grants only a logical scope.
-- name: InsertAssignedRunLease :one
WITH admitted AS (
 UPDATE computer_instances i SET membership_revision=membership_revision+1,updated_at=clock_timestamp()
 FROM runs r
 WHERE i.id=sqlc.arg(computer_instance_id) AND i.environment_id=sqlc.arg(environment_id)
 AND i.computer_id=sqlc.arg(computer_id) AND i.writer_generation=sqlc.arg(writer_generation)
 AND i.worker_host_id=sqlc.arg(worker_host_id) AND i.worker_epoch=sqlc.arg(worker_epoch)
 AND i.worker_group_id=sqlc.arg(worker_group_id)
 AND i.desired_state='ready' AND i.observed_state='ready' AND i.mount_state='mounted'
 AND i.observed_desired_version=i.desired_version
 AND i.reclaimed_at IS NULL AND i.writer_expires_at>clock_timestamp()
 AND r.id=sqlc.arg(run_id) AND r.environment_id=i.environment_id AND r.computer_id=i.computer_id
 AND r.current_attempt_number=sqlc.arg(attempt_number) AND r.current_run_lease_id IS NULL
 AND r.deployment_id=i.program_deployment_id
 AND NOT EXISTS(SELECT 1 FROM run_leases l WHERE l.run_id=r.id AND l.process_reconciled_at IS NULL)
 AND ((i.admission_state='open' AND r.status='queued'
       AND NOT EXISTS(SELECT 1 FROM run_waits w WHERE w.run_id=r.id AND w.attempt_number=r.current_attempt_number
         AND w.suspension_status IN ('hot','checkpointing','parked','resume_pending','resuming'))) OR
      (i.admission_state='restoring' AND r.status IN ('queued','waiting') AND EXISTS(
        SELECT 1 FROM computer_checkpoints c JOIN computer_checkpoint_runs m ON m.checkpoint_id=c.id
        JOIN run_waits w ON w.id=m.run_wait_id AND w.suspend_checkpoint_id=c.id
        WHERE c.id=i.source_checkpoint_id AND c.resume_computer_instance_id=i.id
        AND c.resume_committed_at IS NOT NULL AND m.run_id=r.id AND m.attempt_number=r.current_attempt_number
        AND w.suspension_status IN ('parked','resume_pending'))))
 RETURNING i.*,r.id AS admitted_run_id,r.current_attempt_number AS admitted_attempt_number,r.deployment_id
)
INSERT INTO run_leases(id,org_id,project_id,environment_id,run_id,computer_id,region_id,
 lease_sequence,attempt_number,worker_group_id,worker_host_id,worker_epoch,computer_instance_id,
 writer_generation,deployment_id,requested_cpu_millis,requested_memory_bytes,
 requested_guest_ephemeral_disk_bytes,requested_execution_slots,trace_id,span_id,parent_span_id,
 traceparent,start_deadline_at,expires_at)
SELECT sqlc.arg(id),org_id,project_id,environment_id,admitted_run_id,computer_id,region_id,
 sqlc.arg(lease_sequence),admitted_attempt_number,worker_group_id,worker_host_id,worker_epoch,id,
 writer_generation,deployment_id,sqlc.arg(requested_cpu_millis),sqlc.arg(requested_memory_bytes),
 sqlc.arg(requested_guest_ephemeral_disk_bytes),sqlc.arg(requested_execution_slots),sqlc.narg(trace_id),
 sqlc.narg(span_id),sqlc.narg(parent_span_id),sqlc.narg(traceparent),sqlc.arg(start_deadline_at),sqlc.arg(expires_at)
FROM admitted RETURNING *;

-- Recheck decision-time deadlines after grant writes. The surrounding transaction
-- rolls back the grant and membership change if this compare-and-set fails.
-- name: SetRunCurrentLease :one
UPDATE runs r SET status=CASE WHEN i.admission_state='restoring' THEN 'waiting' ELSE r.status END,
 current_run_lease_id=l.id,first_lease_at=COALESCE(r.first_lease_at,clock_timestamp()),
 instance_preparation_count=0,next_instance_preparation_at=NULL,revision=r.revision+1,updated_at=clock_timestamp()
FROM run_leases l,computer_instances i
WHERE r.id=sqlc.arg(run_id) AND r.environment_id=sqlc.arg(environment_id)
 AND r.revision=sqlc.arg(expected_revision) AND r.current_attempt_number=sqlc.arg(attempt_number)
 AND r.current_run_lease_id IS NULL AND r.status IN ('queued','waiting')
 AND (r.first_lease_at IS NOT NULL OR r.queued_expires_at IS NULL OR r.queued_expires_at>clock_timestamp())
 AND l.id=sqlc.arg(run_lease_id) AND l.run_id=r.id AND l.attempt_number=r.current_attempt_number
 AND l.status='assigned' AND l.start_deadline_at>clock_timestamp() AND l.expires_at>clock_timestamp()
 AND i.id=l.computer_instance_id AND i.writer_generation=l.writer_generation
 AND i.writer_expires_at>clock_timestamp() AND i.reclaimed_at IS NULL
 AND (i.admission_state='open' OR (i.admission_state='restoring' AND EXISTS(
      SELECT 1 FROM computer_checkpoints c WHERE c.id=i.source_checkpoint_id
      AND c.resume_computer_instance_id=i.id AND c.resume_committed_at IS NOT NULL)))
RETURNING r.*;
