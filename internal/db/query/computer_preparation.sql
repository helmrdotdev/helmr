-- All mutations require Computer then exact Instance locks in the caller's transaction.
-- name: ChargeComputerPreparation :one
UPDATE computers c SET preparation_attempt_count=c.preparation_attempt_count+1,
 preparation_instance_id=i.id,next_preparation_at=NULL,revision=c.revision+1,updated_at=clock_timestamp()
FROM computer_instances i
WHERE c.id=sqlc.arg(computer_id) AND i.id=sqlc.arg(instance_id) AND i.computer_id=c.id
 AND i.environment_id=c.environment_id AND i.writer_generation=c.writer_generation
 AND i.observed_state='allocated' AND i.desired_state='ready' AND i.reclaimed_at IS NULL
 AND i.writer_expires_at>clock_timestamp()
 AND c.status='active' AND c.desired_state='active' AND c.deleted_at IS NULL
 AND c.preparation_failure IS NULL AND c.recovery_failure IS NULL
 AND c.preparation_attempt_count<8
 AND (c.preparation_attempt_count=0 OR c.next_preparation_at<=clock_timestamp())
 AND (c.recovery_id IS NULL OR c.recovery_completed_at IS NOT NULL OR c.recovery_disk_version_id=i.source_disk_version_id)
 AND NOT EXISTS(SELECT 1 FROM computer_instances prior WHERE prior.computer_id=c.id AND prior.id<>i.id AND prior.reclaimed_at IS NULL)
RETURNING c.*;

-- name: ListFailedComputerPreparations :many
SELECT c.environment_id,c.id AS computer_id,c.preparation_instance_id AS instance_id
FROM computers c JOIN computer_instances i ON i.id=c.preparation_instance_id AND i.computer_id=c.id
WHERE c.preparation_attempt_count>0 AND c.next_preparation_at IS NULL AND c.preparation_failure IS NULL
 AND i.writer_generation=c.writer_generation
 AND (i.desired_state='closed' OR i.observed_state IN ('failed','lost','closed') OR i.reclaimed_at IS NOT NULL)
ORDER BY c.updated_at,c.id LIMIT sqlc.arg(row_limit);

-- name: SettleComputerPreparationFailure :one
UPDATE computers c SET
 next_preparation_at=CASE WHEN c.preparation_attempt_count<8 THEN clock_timestamp()+LEAST(60,power(2,c.preparation_attempt_count-1))*interval '1 second' END,
 preparation_failure=CASE WHEN c.preparation_attempt_count=8 THEN '{"code":"computer_preparation_exhausted","message":"Computer preparation limit reached","details":{}}'::jsonb END,
 desired_state=CASE WHEN c.preparation_attempt_count=8 AND c.status NOT IN ('deleting','deleted') AND c.desired_state<>'deleted' THEN 'stopped' ELSE c.desired_state END,
 revision=c.revision+1,updated_at=clock_timestamp()
FROM computer_instances i
WHERE c.environment_id=sqlc.arg(environment_id) AND c.id=sqlc.arg(computer_id)
 AND i.id=sqlc.arg(instance_id) AND c.preparation_instance_id=i.id AND i.computer_id=c.id
 AND i.writer_generation=c.writer_generation
 AND c.preparation_attempt_count>0 AND c.next_preparation_at IS NULL AND c.preparation_failure IS NULL
 AND (i.desired_state='closed' OR i.observed_state IN ('failed','lost','closed') OR i.reclaimed_at IS NOT NULL)
RETURNING c.*;

-- name: CompleteComputerPreparation :one
UPDATE computers c SET preparation_attempt_count=0,preparation_instance_id=NULL,next_preparation_at=NULL,
 recovery_completed_at=CASE WHEN c.recovery_id IS NOT NULL AND c.recovery_completed_at IS NULL THEN clock_timestamp() ELSE c.recovery_completed_at END,
 revision=c.revision+1,updated_at=clock_timestamp()
FROM computer_instances i
WHERE c.environment_id=sqlc.arg(environment_id) AND c.id=sqlc.arg(computer_id)
 AND i.id=sqlc.arg(instance_id) AND i.computer_id=c.id AND c.preparation_instance_id=i.id
 AND i.writer_generation=c.writer_generation AND i.desired_version=sqlc.arg(desired_version)
 AND i.observed_desired_version=i.desired_version AND i.observed_state='ready'
 AND i.desired_state='ready' AND i.admission_state='open' AND i.reclaimed_at IS NULL
 AND i.terminal_at IS NULL AND i.writer_expires_at>clock_timestamp()
 AND c.status='active' AND c.desired_state='active' AND c.deleted_at IS NULL
 AND c.preparation_failure IS NULL AND c.next_preparation_at IS NULL AND c.recovery_failure IS NULL
 AND (c.recovery_id IS NULL OR c.recovery_completed_at IS NOT NULL OR c.recovery_disk_version_id=i.retained_source_disk_version_id)
 AND (i.source_checkpoint_id IS NULL OR EXISTS(SELECT 1 FROM computer_checkpoints cp
      WHERE cp.id=i.source_checkpoint_id AND cp.resume_computer_instance_id=i.id AND cp.resume_committed_at IS NOT NULL))
RETURNING c.*;

-- The preparation failure owner invalidates its source checkpoint in the same
-- transaction as exhaustion, before logical members are settled separately.
-- name: InvalidateExhaustedComputerCheckpoint :exec
UPDATE computer_checkpoints cp SET status='invalid',invalidated_at=clock_timestamp(),
 invalidation_reason_code='computer_preparation_exhausted'
FROM computers c,computer_instances i
WHERE c.environment_id=sqlc.arg(environment_id) AND c.id=sqlc.arg(computer_id)
 AND c.preparation_failure IS NOT NULL AND c.preparation_instance_id=i.id
 AND i.computer_id=c.id AND i.writer_generation=c.writer_generation
 AND cp.id=i.source_checkpoint_id AND cp.computer_id=c.id AND cp.environment_id=c.environment_id
 AND cp.status='ready'
 AND (cp.resume_computer_instance_id IS NULL OR cp.resume_computer_instance_id=i.id)
 AND (i.desired_state='closed' OR i.observed_state IN ('failed','lost','closed') OR i.reclaimed_at IS NOT NULL);

-- Post-lock check for an allocated Instance, repeated after object writes and
-- before commit. The caller (a computer owner fence) must already
-- hold the admission-mode Worker Group, Pool and Host fence (which pins active
-- supply, the epoch, a present observation and no Run or VM pause) and the
-- Computer and Instance locks.
-- name: GetComputerPreparationDeadlinesValid :one
SELECT (i.preparation_expires_at>clock_timestamp() AND i.writer_expires_at>clock_timestamp()
 AND i.desired_state='ready' AND i.desired_version=sqlc.arg(desired_version) AND i.reclaimed_at IS NULL
 AND i.writer_generation=sqlc.arg(writer_generation) AND i.observed_state='allocated'
 AND w.observed_at>=clock_timestamp()-sqlc.arg(worker_freshness_seconds)::bigint*interval '1 second')::boolean AS valid
 FROM computer_instances i JOIN worker_hosts w ON w.id=i.worker_host_id WHERE i.id=sqlc.arg(id);
