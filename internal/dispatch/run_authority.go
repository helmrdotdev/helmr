package dispatch

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const mebibyte = int64(1024 * 1024)

type runPlacementAuthority struct {
	entrypointKind            string
	actorID                   pgtype.UUID
	ownerActorID              pgtype.UUID
	ownerActorRunID           pgtype.UUID
	runID                     pgtype.UUID
	orgID                     pgtype.UUID
	projectID                 pgtype.UUID
	environmentID             pgtype.UUID
	deploymentID              pgtype.UUID
	workspaceDefinitionID     pgtype.UUID
	workspaceID               pgtype.UUID
	baseWorkspaceVersionID    pgtype.UUID
	restoreCheckpointID       pgtype.UUID
	restoreCheckpointVersion  pgtype.UUID
	resumeRunWaitID           pgtype.UUID
	resumeChildRunID          pgtype.UUID
	resumeConditionStatus     string
	resumeRequestVersion      int64
	restoreWorkerGroupID      pgtype.UUID
	restoreRuntimeIdentityID  string
	restoreVMVCPUCount        int32
	restoreCPUConfigDigest    string
	restoreSubstrateID        pgtype.UUID
	restoreSubstrateFormat    string
	restoreSubstrateContract  string
	restoreMountGeneration    pgtype.Int8
	sameWorkspaceResume       bool
	resumeOwnership           pgtype.Int8
	resumeParentWriter        pgtype.Int8
	resumeChildWriter         pgtype.Int8
	sameWorkspaceChildWaitID  pgtype.UUID
	sameWorkspaceOwnership    pgtype.Int8
	sameWorkspaceParentWriter pgtype.Int8
	sameWorkspaceChildWriter  pgtype.Int8
	attemptNumber             int32
	revision                  int64
	regionID                  string
	queueName                 string
	concurrencyKey            pgtype.Text
	queueLimit                pgtype.Int8
	ownershipGeneration       int64
	writerGeneration          int64
	traceID                   pgtype.Text
	rootSpanID                string
	resources                 runResources
	architecture              string
}

type runResources struct {
	cpuMillis               int64
	memoryBytes             int64
	guestEphemeralDiskBytes int64
	executionSlots          int32
}

func discoverRunQueueScope(
	ctx context.Context,
	tx pgx.Tx,
	candidate ReadyRunCandidate,
) (pgtype.UUID, string, pgtype.Text, error) {
	var environmentID pgtype.UUID
	var queueName string
	var concurrencyKey pgtype.Text
	err := tx.QueryRow(ctx, `
SELECT environment_id, queue_name, concurrency_key
  FROM runs
 WHERE org_id = $1
   AND id = $2
   AND revision = $3`,
		candidate.OrgID,
		candidate.RunID,
		candidate.ExpectedRunRevision,
	).Scan(&environmentID, &queueName, &concurrencyKey)
	if err != nil {
		return pgtype.UUID{}, "", pgtype.Text{}, err
	}
	return environmentID, queueName, concurrencyKey, nil
}

func lockRunPlacementAuthority(
	ctx context.Context,
	tx pgx.Tx,
	candidate ReadyRunCandidate,
	allowInitialization bool,
) (runPlacementAuthority, error) {
	var authority runPlacementAuthority
	var entrypointDefinitionID pgtype.UUID
	var actorStartInputSequence pgtype.Int8
	var actorStartInputHighWatermark pgtype.Int8
	var actorRunGeneration int64
	var actorCommittedInputSequence int64
	var actorNextInputSequence int64
	var err error
	if err := tx.QueryRow(ctx, `
SELECT entrypoint_kind, session_id
  FROM runs
 WHERE org_id = $1
   AND id = $2
   AND revision = $3`,
		candidate.OrgID,
		candidate.RunID,
		candidate.ExpectedRunRevision,
	).Scan(&authority.entrypointKind, &authority.actorID); err != nil {
		return runPlacementAuthority{}, err
	}
	authority.ownerActorID = authority.actorID
	authority.ownerActorRunID = candidate.RunID
	if authority.entrypointKind == "task" && !authority.actorID.Valid {
		authority.ownerActorID, authority.ownerActorRunID, err =
			discoverSameWorkspaceOwnerActor(ctx, tx, candidate.RunID)
		if err != nil {
			return runPlacementAuthority{}, err
		}
	}
	if authority.entrypointKind == "actor" {
		if !authority.actorID.Valid {
			return runPlacementAuthority{}, pgx.ErrNoRows
		}
	} else if authority.entrypointKind != "task" || authority.actorID.Valid {
		return runPlacementAuthority{}, pgx.ErrNoRows
	}
	if authority.ownerActorID.Valid {
		err := tx.QueryRow(ctx, `
SELECT run_generation, committed_input_sequence, next_input_sequence
  FROM sessions
 WHERE id = $1
   AND current_run_id = $2
   AND status IN ('open', 'closing')
 FOR UPDATE`, authority.ownerActorID, authority.ownerActorRunID).Scan(
			&actorRunGeneration,
			&actorCommittedInputSequence,
			&actorNextInputSequence,
		)
		if err != nil {
			return runPlacementAuthority{}, err
		}
		if actorRunGeneration <= 0 {
			return runPlacementAuthority{}, pgx.ErrNoRows
		}
	}
	sameWorkspaceWaitID, sameWorkspaceRootRunID, err := lockSameWorkspaceAncestors(
		ctx,
		tx,
		candidate.RunID,
		authority.ownerActorID,
		authority.ownerActorRunID,
	)
	if err != nil {
		return runPlacementAuthority{}, fmt.Errorf("lock Run placement same-Workspace ancestors: %w", err)
	}
	err = tx.QueryRow(ctx, `
SELECT runs.id,
       runs.org_id,
       runs.project_id,
       runs.environment_id,
       runs.deployment_id,
       runs.deployment_definition_id,
       runs.workspace_id,
       runs.current_attempt_number,
       runs.revision,
       runs.queue_name,
       runs.concurrency_key,
       runs.queue_concurrency_limit,
       runs.trace_id,
       runs.root_span_id,
	       child_wait.id,
	       child_wait.ownership_generation,
	       child_wait.parent_writer_generation,
	       child_wait.child_writer_generation,
	       restore_wait.id,
	       restore_wait.child_run_id,
	       coalesce(restore_wait.condition_status, ''),
	       coalesce(restore_wait.resume_request_version, 0),
	       restore_wait.suspend_checkpoint_id,
	       restore_checkpoint.private_workspace_version_id,
	       coalesce(
	           restore_wait.resume_workspace_version_id,
	           restore_checkpoint.private_workspace_version_id
	       ),
	       restore_wait.ownership_generation,
	       restore_wait.parent_writer_generation,
	       restore_wait.child_writer_generation,
	       runs.session_input_start_sequence,
	       runs.session_input_high_watermark
	  FROM runs
	  LEFT JOIN LATERAL (
	       SELECT edge.id,
	              edge.ownership_generation,
	              edge.parent_writer_generation,
	              edge.child_writer_generation
         FROM run_waits AS edge
         JOIN runs AS parent
           ON parent.environment_id = edge.environment_id
          AND parent.id = edge.run_id
          AND parent.workspace_id = edge.workspace_id
          AND parent.status = 'waiting'
          AND parent.current_attempt_number = edge.attempt_number
          AND parent.current_run_lease_id IS NULL
         JOIN run_checkpoints AS checkpoint
           ON checkpoint.id = edge.suspend_checkpoint_id
	          AND checkpoint.run_id = edge.run_id
          AND checkpoint.attempt_number = edge.attempt_number
          AND checkpoint.run_wait_id = edge.id
          AND checkpoint.workspace_id = edge.workspace_id
          AND checkpoint.status = 'ready'
          AND (checkpoint.expires_at IS NULL
               OR checkpoint.expires_at > transaction_timestamp())
         JOIN computer_versions AS base
           ON base.workspace_id = edge.workspace_id
          AND base.id = edge.base_workspace_version_id
          AND base.status = 'private'
         LEFT JOIN LATERAL (
	              SELECT child_workspace_lease.id
	                FROM run_leases AS child_lease
	                JOIN workspace_leases AS child_workspace_lease
	                  ON child_workspace_lease.owner_run_lease_id = child_lease.id
	                 AND child_workspace_lease.workspace_id = child_lease.workspace_id
				 AND (
				     (child_lease.status = 'checkpointed' AND child_lease.attempt_number = runs.current_attempt_number)
				     OR (
				         child_lease.status = 'expired'
				         AND child_workspace_lease.status = 'expired'
				         AND child_lease.terminal_reason_code IN ('lease_expired', 'worker_lost', 'runtime_failed')
				         AND child_workspace_lease.terminal_reason_code = child_lease.terminal_reason_code
				         AND EXISTS (
				             SELECT 1 FROM run_waits AS resume_edge
				             JOIN run_checkpoints AS resume_checkpoint
				               ON resume_checkpoint.id = resume_edge.suspend_checkpoint_id
				              AND resume_checkpoint.run_id = resume_edge.run_id
				              AND resume_checkpoint.attempt_number = resume_edge.attempt_number
				              AND resume_checkpoint.run_wait_id = resume_edge.id
				              AND resume_checkpoint.workspace_id = resume_edge.workspace_id
				              AND resume_checkpoint.status = 'ready'
				              AND (resume_checkpoint.expires_at IS NULL OR resume_checkpoint.expires_at > transaction_timestamp())
				              AND resume_checkpoint.source_run_lease_id = resume_edge.prior_run_lease_id
				             JOIN runtime_instances AS restored_runtime
				               ON restored_runtime.id = child_lease.runtime_instance_id
				              AND restored_runtime.workspace_id = child_lease.workspace_id
				              AND restored_runtime.runtime_identity_id = child_lease.runtime_identity_id
				              AND restored_runtime.restore_checkpoint_id = resume_checkpoint.id
				              AND child_workspace_lease.runtime_instance_id = restored_runtime.id
				            WHERE resume_edge.run_id = runs.id
				              AND resume_edge.attempt_number = child_lease.attempt_number
				              AND child_lease.attempt_number = runs.current_attempt_number
				              AND resume_edge.workspace_id = runs.workspace_id
				              AND resume_edge.suspension_status = 'resume_pending'
				              AND resume_edge.current_run_lease_id IS NULL
				              AND resume_edge.checkpoint_request_version > 0
				              AND resume_edge.checkpoint_ack_version = resume_edge.checkpoint_request_version
				              AND resume_edge.resume_request_version > resume_edge.resume_ack_version
				              AND child_workspace_lease.base_workspace_version_id =
				                  COALESCE(resume_edge.resume_workspace_version_id, resume_checkpoint.private_workspace_version_id)
				         )
				     )
				     OR (child_workspace_lease.base_workspace_version_id = edge.base_workspace_version_id
				         AND runs.base_workspace_version_id = edge.base_workspace_version_id
				         AND child_lease.attempt_number = runs.current_attempt_number)
				     OR EXISTS (
				         SELECT 1
				           FROM computer_versions AS retry_version
				           JOIN computer_version_roots AS retry_root ON retry_root.version_id = retry_version.id AND retry_root.computer_id = retry_version.workspace_id AND retry_root.environment_id = retry_version.environment_id
				           JOIN run_finalization_objects AS retry_capture
				             ON retry_capture.run_lease_id = child_lease.id
				            AND retry_capture.operation_id = child_lease.finalization_operation_id
				            AND retry_capture.lease_status = 'failed'
				            AND retry_capture.root = retry_root.locator
				          WHERE retry_version.id = runs.base_workspace_version_id
				            AND retry_version.workspace_id = runs.workspace_id
				            AND retry_version.status = 'private'
				            AND retry_version.source_workspace_lease_id = child_workspace_lease.id
				            AND retry_version.parent_version_id = child_workspace_lease.base_workspace_version_id
				            AND retry_version.ownership_generation = child_workspace_lease.ownership_generation
				            AND retry_version.writer_generation = child_workspace_lease.writer_generation
				            AND child_lease.attempt_number = runs.current_attempt_number - 1
				            AND child_lease.status = 'failed'
				            AND child_lease.terminal_at IS NOT NULL
				            AND child_lease.terminal_request_fingerprint IS NOT NULL
				            AND child_lease.finalization_kind = 'capture'
				     )
				     OR EXISTS (
				         SELECT 1
				           FROM run_waits AS resume_edge
				          WHERE resume_edge.run_id = runs.id
				            AND resume_edge.attempt_number = child_lease.attempt_number
				            AND child_lease.attempt_number = runs.current_attempt_number
				            AND resume_edge.workspace_id = runs.workspace_id
				            AND resume_edge.suspension_status = 'resume_pending'
				            AND resume_edge.ownership_generation = edge.ownership_generation
				            AND resume_edge.resume_writer_generation IS NULL
				            AND resume_edge.resume_workspace_version_id =
				                child_workspace_lease.base_workspace_version_id
				     )
				 )
                 AND child_workspace_lease.ownership_generation =
                     edge.ownership_generation
                 AND child_workspace_lease.writer_generation =
                     edge.child_writer_generation
                 AND child_workspace_lease.status IN ('released', 'fenced', 'expired')
               WHERE child_lease.run_id = runs.id
                 AND child_lease.workspace_id = runs.workspace_id
                 AND (
                     child_lease.status IN ('failed', 'expired', 'lost', 'rejected')
                     OR (
                         (child_lease.status = 'checkpointed' AND child_lease.attempt_number = runs.current_attempt_number)
                         AND EXISTS (
                             SELECT 1
                              FROM run_waits AS resume_edge
                              JOIN run_checkpoints AS resume_checkpoint
                                ON resume_checkpoint.id = resume_edge.suspend_checkpoint_id
                               AND resume_checkpoint.run_id = resume_edge.run_id
                               AND resume_checkpoint.attempt_number = resume_edge.attempt_number
                               AND resume_checkpoint.run_wait_id = resume_edge.id
                               AND resume_checkpoint.workspace_id = resume_edge.workspace_id
                               AND resume_checkpoint.status = 'ready'
                               AND (resume_checkpoint.expires_at IS NULL OR resume_checkpoint.expires_at > transaction_timestamp())
                               AND resume_checkpoint.source_run_lease_id = child_lease.id
                               AND resume_checkpoint.source_workspace_lease_id = child_workspace_lease.id
                               AND resume_checkpoint.base_workspace_version_id = child_workspace_lease.base_workspace_version_id
                              WHERE resume_edge.run_id = runs.id
                                AND resume_edge.attempt_number = child_lease.attempt_number
                                AND child_lease.attempt_number = runs.current_attempt_number
                                AND resume_edge.workspace_id = runs.workspace_id
                                AND resume_edge.suspension_status = 'resume_pending'
                                AND resume_edge.prior_run_lease_id = child_lease.id
                                AND resume_edge.checkpoint_request_version > 0
                                AND resume_edge.checkpoint_ack_version = resume_edge.checkpoint_request_version
                                AND resume_edge.resume_writer_generation IS NULL
                         )
                     )
                 )
               ORDER BY child_lease.lease_sequence DESC
               LIMIT 1
         ) AS prior_child ON edge.child_writer_generation IS NOT NULL
        WHERE edge.child_run_id = runs.id
          AND edge.kind = 'child'
          AND runs.parent_run_id = edge.run_id
          AND runs.environment_id = edge.environment_id
          AND runs.parent_owns_lifecycle IS TRUE
          AND edge.workspace_id = runs.workspace_id
          AND edge.condition_status = 'pending'
          AND edge.suspension_status = 'parked'
          AND EXISTS (
              SELECT 1 FROM run_attempts origin
               WHERE origin.run_id = runs.id AND origin.number <= runs.current_attempt_number
                 AND origin.workspace_id = edge.workspace_id
                 AND origin.base_workspace_version_id = edge.base_workspace_version_id
          )
	          AND edge.ownership_generation IS NOT NULL
	          AND edge.parent_writer_generation IS NOT NULL
	          AND (edge.child_writer_generation IS NULL
	               OR prior_child.id IS NOT NULL)
	  ) AS child_wait ON true
	  LEFT JOIN LATERAL (
	       SELECT run_waits.id,
	              run_waits.child_run_id,
	              run_waits.condition_status,
	              run_waits.resume_request_version,
	              run_waits.suspend_checkpoint_id,
	              run_waits.attempt_number,
	              run_waits.workspace_id,
	              run_waits.resume_workspace_version_id,
	              run_waits.ownership_generation,
	              run_waits.parent_writer_generation,
	              run_waits.child_writer_generation
	         FROM run_waits
	        WHERE run_waits.run_id = runs.id
	          AND run_waits.suspension_status = 'resume_pending'
	  ) AS restore_wait ON true
	  LEFT JOIN run_checkpoints AS restore_checkpoint
	    ON restore_checkpoint.id = restore_wait.suspend_checkpoint_id
   AND restore_checkpoint.run_id = runs.id
   AND restore_checkpoint.attempt_number = restore_wait.attempt_number
   AND restore_checkpoint.run_wait_id = restore_wait.id
   AND restore_checkpoint.workspace_id = restore_wait.workspace_id
   AND restore_checkpoint.status = 'ready'
   AND (restore_checkpoint.expires_at IS NULL
        OR restore_checkpoint.expires_at > transaction_timestamp())
	  LEFT JOIN computer_versions AS restore_version
	    ON restore_version.workspace_id = restore_checkpoint.workspace_id
	   AND restore_version.id = coalesce(
	       restore_wait.resume_workspace_version_id,
	       restore_checkpoint.private_workspace_version_id
	   )
   AND restore_version.status = 'private'
 WHERE runs.org_id = $1
   AND runs.id = $2
   AND runs.revision = $3
   AND runs.entrypoint_kind = $4
   AND (($4 = 'task' AND runs.session_id IS NULL
         AND runs.cause_kind IN ('api', 'manual', 'schedule', 'child'))
        OR ($4 = 'actor' AND runs.session_id = $5
            AND runs.active_elapsed_ms < runs.max_active_duration_ms
            AND runs.cause_kind IN ('actor_start', 'continuation')
            AND runs.parent_run_id IS NULL))
   AND runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
   AND (runs.next_runtime_preparation_at IS NULL
        OR runs.next_runtime_preparation_at <= transaction_timestamp())
	   AND child_wait.id IS NOT DISTINCT FROM $6::uuid
	   AND (
	       ($4 IN ('task', 'actor') AND child_wait.id IS NULL AND
           restore_wait.id IS NULL
           AND NOT EXISTS (
               SELECT 1
                 FROM run_waits
                WHERE run_waits.run_id = runs.id
                  AND run_waits.suspension_status IN (
                      'hot', 'checkpointing', 'parked', 'resume_pending', 'resuming'
                  )
           ))
	       OR ($4 = 'task' AND child_wait.id IS NOT NULL)
       OR (
           restore_wait.id IS NOT NULL
           AND restore_checkpoint.id IS NOT NULL
           AND restore_version.id IS NOT NULL
           AND ($4 = 'task' OR restore_checkpoint.actor_speculative_input_sequence IS NOT NULL)
       )
   )
   AND (
       runs.first_lease_at IS NOT NULL
       OR runs.queued_expires_at IS NULL
       OR runs.queued_expires_at > transaction_timestamp()
   )
 FOR UPDATE OF runs`,
		candidate.OrgID,
		candidate.RunID,
		candidate.ExpectedRunRevision,
		authority.entrypointKind,
		authority.actorID,
		sameWorkspaceWaitID,
	).Scan(
		&authority.runID,
		&authority.orgID,
		&authority.projectID,
		&authority.environmentID,
		&authority.deploymentID,
		&entrypointDefinitionID,
		&authority.workspaceID,
		&authority.attemptNumber,
		&authority.revision,
		&authority.queueName,
		&authority.concurrencyKey,
		&authority.queueLimit,
		&authority.traceID,
		&authority.rootSpanID,
		&authority.sameWorkspaceChildWaitID,
		&authority.sameWorkspaceOwnership,
		&authority.sameWorkspaceParentWriter,
		&authority.sameWorkspaceChildWriter,
		&authority.resumeRunWaitID,
		&authority.resumeChildRunID,
		&authority.resumeConditionStatus,
		&authority.resumeRequestVersion,
		&authority.restoreCheckpointID,
		&authority.restoreCheckpointVersion,
		&authority.baseWorkspaceVersionID,
		&authority.resumeOwnership,
		&authority.resumeParentWriter,
		&authority.resumeChildWriter,
		&actorStartInputSequence,
		&actorStartInputHighWatermark,
	)
	if err != nil {
		return runPlacementAuthority{}, fmt.Errorf("lock Run placement Run authority: %w", err)
	}
	authority.sameWorkspaceResume = authority.resumeOwnership.Valid
	if authority.sameWorkspaceResume &&
		(!authority.resumeParentWriter.Valid ||
			!authority.baseWorkspaceVersionID.Valid) {
		return runPlacementAuthority{}, fmt.Errorf("lock Run placement same-Workspace resume shape: %w", pgx.ErrNoRows)
	}
	var manifestVersion int32
	var manifest []byte
	workspaceOwnerPredicate := "computers.owner_run_id = $5 AND computers.owner_session_id IS NULL"
	workspaceOwnerID := authority.runID
	if authority.sameWorkspaceChildWaitID.Valid {
		if authority.ownerActorID.Valid {
			workspaceOwnerPredicate = "computers.owner_session_id = $5 AND computers.owner_run_id IS NULL"
			workspaceOwnerID = authority.ownerActorID
		} else {
			workspaceOwnerID = sameWorkspaceRootRunID
		}
	} else if authority.entrypointKind == "actor" {
		workspaceOwnerPredicate = "computers.owner_session_id = $5 AND computers.owner_run_id IS NULL"
		workspaceOwnerID = authority.actorID
	}
	err = tx.QueryRow(ctx, fmt.Sprintf(`
SELECT computers.deployment_definition_id,
       computers.region_id,
       computers.ownership_generation,
       computers.writer_generation,
       workspace_definitions.manifest_version,
       workspace_definitions.manifest
  FROM computers
  JOIN environments AS workspace_environment
    ON workspace_environment.id = computers.environment_id
  JOIN deployment_definitions AS workspace_definitions
    ON workspace_definitions.environment_id = computers.environment_id
   AND workspace_definitions.id = computers.deployment_definition_id
   AND workspace_definitions.kind = 'sandbox'
   AND workspace_definitions.declared_id = computers.sandbox_declared_id
 WHERE workspace_environment.org_id = $1
   AND workspace_environment.project_id = $2
   AND computers.environment_id = $3
   AND computers.id = $4
   AND computers.status = 'active'
   AND computers.desired_state = 'active'
   AND computers.dirty_state = 'clean'
   AND %s
 FOR UPDATE OF computers`, workspaceOwnerPredicate),
		authority.orgID,
		authority.projectID,
		authority.environmentID,
		authority.workspaceID,
		workspaceOwnerID,
	).Scan(
		&authority.workspaceDefinitionID,
		&authority.regionID,
		&authority.ownershipGeneration,
		&authority.writerGeneration,
		&manifestVersion,
		&manifest,
	)
	if err != nil {
		return runPlacementAuthority{}, fmt.Errorf("lock Run placement Workspace authority: %w", err)
	}
	if authority.sameWorkspaceChildWaitID.Valid {
		if !authority.sameWorkspaceOwnership.Valid ||
			authority.sameWorkspaceOwnership.Int64 != authority.ownershipGeneration ||
			!authority.sameWorkspaceParentWriter.Valid {
			return runPlacementAuthority{}, fmt.Errorf("lock Run placement same-Workspace child shape: %w", pgx.ErrNoRows)
		}
		if authority.sameWorkspaceResume {
			if !authority.sameWorkspaceChildWriter.Valid ||
				!authority.resumeParentWriter.Valid ||
				(authority.sameWorkspaceChildWriter.Int64 != authority.resumeParentWriter.Int64 &&
					authority.sameWorkspaceChildWriter.Int64 != authority.writerGeneration) {
				return runPlacementAuthority{}, fmt.Errorf("lock Run placement nested same-Workspace lineage: %w", pgx.ErrNoRows)
			}
		} else {
			expectedWriter := authority.sameWorkspaceParentWriter.Int64
			if authority.sameWorkspaceChildWriter.Valid {
				expectedWriter = authority.sameWorkspaceChildWriter.Int64
			}
			if expectedWriter != authority.writerGeneration {
				return runPlacementAuthority{}, fmt.Errorf("lock Run placement child writer: %w", pgx.ErrNoRows)
			}
		}
	}
	terminalChildExcluded, childOwnsCurrentWriter := false, false
	if authority.sameWorkspaceResume {
		predecessorWriter := authority.resumeChildWriter.Int64
		if !authority.resumeChildWriter.Valid {
			if (authority.resumeConditionStatus != "failed" && authority.resumeConditionStatus != "cancelled") ||
				authority.baseWorkspaceVersionID != authority.restoreCheckpointVersion {
				return runPlacementAuthority{}, fmt.Errorf("unadmitted child handback receipt: %w", pgx.ErrNoRows)
			}
			valid, err := db.New(tx).SameWorkspaceChildHasNoExecution(ctx, db.SameWorkspaceChildHasNoExecutionParams{
				ChildRunID: authority.resumeChildRunID, ParentRunID: authority.runID,
				WorkspaceID: authority.workspaceID, BaseWorkspaceVersionID: authority.restoreCheckpointVersion,
			})
			if err != nil {
				return runPlacementAuthority{}, fmt.Errorf("validate unadmitted child handback: %w", err)
			}
			if !valid {
				return runPlacementAuthority{}, fmt.Errorf("validate unadmitted child handback: %w", pgx.ErrNoRows)
			}
			predecessorWriter = authority.resumeParentWriter.Int64
		}
		if authority.resumeOwnership.Int64 != authority.ownershipGeneration ||
			predecessorWriter > authority.writerGeneration {
			return runPlacementAuthority{}, fmt.Errorf("lock Run placement same-Workspace resume generation: %w", pgx.ErrNoRows)
		}
		if authority.resumeChildWriter.Valid &&
			(authority.resumeConditionStatus == "failed" || authority.resumeConditionStatus == "cancelled") {
			if authority.baseWorkspaceVersionID != authority.restoreCheckpointVersion {
				return runPlacementAuthority{}, fmt.Errorf("terminal child original-base handback: %w", pgx.ErrNoRows)
			}
			terminalChildExcluded, childOwnsCurrentWriter, err = sameWorkspaceTerminalChildExcluded(ctx, tx, authority)
			if err != nil {
				return runPlacementAuthority{}, fmt.Errorf("validate terminal child exclusion: %w", err)
			}
			if !terminalChildExcluded {
				return runPlacementAuthority{}, fmt.Errorf("validate terminal child exclusion: %w", pgx.ErrNoRows)
			}
		}
		if predecessorWriter != authority.writerGeneration && !childOwnsCurrentWriter {
			var fenced pgtype.Bool
			err = tx.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
      FROM workspace_leases
      JOIN run_leases
        ON run_leases.id = workspace_leases.owner_run_lease_id
       AND run_leases.run_id = $5
       AND run_leases.attempt_number = $6
       AND run_leases.workspace_id = $1
       AND run_leases.status = 'expired'
       AND run_leases.terminal_reason_code IN (
           'lease_expired',
           'worker_lost',
           'runtime_failed'
       )
     WHERE workspace_leases.workspace_id = $1
       AND workspace_leases.ownership_generation = $2
       AND workspace_leases.writer_generation = $3
       AND workspace_leases.base_workspace_version_id = $4
       AND workspace_leases.status = 'expired'
)`,
				authority.workspaceID,
				authority.ownershipGeneration,
				authority.writerGeneration,
				authority.baseWorkspaceVersionID,
				authority.runID,
				authority.attemptNumber,
			).Scan(&fenced)
			if err != nil || !fenced.Valid || !fenced.Bool {
				return runPlacementAuthority{}, fmt.Errorf("lock Run placement same-Workspace resume fence: %w", pgx.ErrNoRows)
			}
		}
	}

	var attemptBaseWorkspaceVersionID pgtype.UUID
	var attemptSessionInputStartSequence pgtype.Int8
	err = tx.QueryRow(ctx, `
SELECT run_attempts.base_workspace_version_id,
       run_attempts.session_input_start_sequence
  FROM run_attempts
  JOIN computer_versions
    ON computer_versions.workspace_id = run_attempts.workspace_id
   AND computer_versions.id = run_attempts.base_workspace_version_id
   AND (($5::boolean AND computer_versions.status = 'private')
        OR (NOT $5::boolean AND computer_versions.status = 'committed')
        OR ($6::boolean AND NOT $5::boolean
            AND computer_versions.status = 'initializing'
            AND computer_versions.parent_version_id IS NULL))
 WHERE run_attempts.run_id = $1
   AND run_attempts.number = $2
   AND run_attempts.entrypoint_kind = $4
   AND run_attempts.workspace_id = $3
 FOR UPDATE OF run_attempts`,
		authority.runID,
		authority.attemptNumber,
		authority.workspaceID,
		authority.entrypointKind,
		authority.sameWorkspaceChildWaitID.Valid,
		allowInitialization && !authority.restoreCheckpointID.Valid,
	).Scan(&attemptBaseWorkspaceVersionID, &attemptSessionInputStartSequence)
	if err != nil {
		return runPlacementAuthority{}, fmt.Errorf("lock Run placement Attempt authority: %w", err)
	}
	if !authority.baseWorkspaceVersionID.Valid {
		authority.baseWorkspaceVersionID = attemptBaseWorkspaceVersionID
	}
	if authority.entrypointKind == "actor" &&
		(!actorStartInputSequence.Valid || !attemptSessionInputStartSequence.Valid ||
			attemptSessionInputStartSequence.Int64 != actorStartInputSequence.Int64 ||
			!actorStartInputHighWatermark.Valid ||
			actorStartInputSequence.Int64 > actorStartInputHighWatermark.Int64 ||
			actorCommittedInputSequence < actorStartInputSequence.Int64 ||
			actorCommittedInputSequence >= actorNextInputSequence) {
		return runPlacementAuthority{}, fmt.Errorf("lock Run placement Actor input authority: %w", pgx.ErrNoRows)
	}
	if authority.restoreCheckpointID.Valid {
		if authority.entrypointKind == "actor" {
			validLineage, err := db.New(tx).ActorCheckpointLineageIsValid(ctx, db.ActorCheckpointLineageIsValidParams{
				RunID: authority.runID, AttemptNumber: authority.attemptNumber, WorkspaceID: authority.workspaceID,
				CheckpointID:        authority.restoreCheckpointID,
				OwnershipGeneration: authority.ownershipGeneration,
			})
			if err != nil {
				return runPlacementAuthority{}, fmt.Errorf("validate Actor checkpoint lineage: %w", err)
			}
			if !validLineage {
				return runPlacementAuthority{}, fmt.Errorf("validate Actor checkpoint lineage: %w", pgx.ErrNoRows)
			}
		}
		var restoreSourceMountGeneration pgtype.Int8
		err = tx.QueryRow(ctx, `
SELECT source_lease.worker_group_id,
       source_runtime.runtime_identity_id,
       source_runtime.vm_vcpu_count,
	       source_runtime.cpu_config_digest,
	       source_runtime.runtime_substrate_id,
	       runtime_substrates.substrate_format,
	       runtime_substrates.substrate_contract,
	       source_workspace_lease.mount_fencing_generation
	  FROM run_waits
	  JOIN run_checkpoints
	    ON run_checkpoints.id = run_waits.suspend_checkpoint_id
	   AND run_checkpoints.run_id = run_waits.run_id
   AND run_checkpoints.attempt_number = run_waits.attempt_number
   AND run_checkpoints.run_wait_id = run_waits.id
   AND run_checkpoints.workspace_id = run_waits.workspace_id
	   AND run_checkpoints.status = 'ready'
	   AND run_checkpoints.private_workspace_version_id = $13
   AND (($10 = 'task' AND run_checkpoints.actor_speculative_input_sequence IS NULL)
        OR ($10 = 'actor'
            AND run_checkpoints.actor_speculative_input_sequence BETWEEN $11 AND $12))
   AND (run_checkpoints.expires_at IS NULL
        OR run_checkpoints.expires_at > transaction_timestamp())
  JOIN computer_versions
    ON computer_versions.workspace_id = run_checkpoints.workspace_id
   AND computer_versions.id = run_checkpoints.private_workspace_version_id
   AND computer_versions.status = 'private'
  JOIN run_leases AS source_lease
    ON source_lease.id = run_checkpoints.source_run_lease_id
   AND source_lease.run_id = run_checkpoints.run_id
   AND source_lease.attempt_number = run_checkpoints.attempt_number
   AND source_lease.workspace_id = run_checkpoints.workspace_id
   AND source_lease.status = 'checkpointed'
	  JOIN workspace_leases AS source_workspace_lease
    ON source_workspace_lease.id = run_checkpoints.source_workspace_lease_id
   AND source_workspace_lease.workspace_id = run_checkpoints.workspace_id
   AND source_workspace_lease.owner_run_lease_id = source_lease.id
	   AND source_workspace_lease.base_workspace_version_id = run_checkpoints.base_workspace_version_id
   AND source_workspace_lease.status IN ('released', 'fenced')
   AND computer_versions.parent_version_id = run_checkpoints.base_workspace_version_id
   AND computer_versions.source_workspace_lease_id = source_workspace_lease.id
   AND computer_versions.ownership_generation = source_workspace_lease.ownership_generation
   AND computer_versions.writer_generation = source_workspace_lease.writer_generation
   AND source_workspace_lease.ownership_generation = $14
   AND source_workspace_lease.owner_process_id IS NULL
   AND ($10 <> 'actor' OR (
       -- A child handback retains the parent's checkpoint and names the exact
       -- child output, or the original private base after failure.
       (run_waits.resume_workspace_version_id IS NULL OR (
           run_waits.kind = 'child'
           AND run_waits.base_workspace_version_id = run_checkpoints.private_workspace_version_id
           AND run_waits.ownership_generation = $14
           AND run_waits.parent_writer_generation = source_workspace_lease.writer_generation
           AND (run_waits.child_writer_generation IS NULL
                OR run_waits.parent_writer_generation < run_waits.child_writer_generation)
           AND run_waits.resume_writer_generation IS NULL
           AND ((run_waits.condition_status = 'completed' AND EXISTS (
               SELECT 1
                 FROM computer_versions AS child_version
                 JOIN workspace_leases AS child_source
                   ON child_source.id = child_version.source_workspace_lease_id
                  AND child_source.workspace_id = child_version.workspace_id
                  AND child_source.base_workspace_version_id = child_version.parent_version_id
                  AND child_source.ownership_generation = child_version.ownership_generation
                  AND child_source.writer_generation = child_version.writer_generation
                  AND child_source.status IN ('released', 'fenced')
                  AND child_source.owner_process_id IS NULL
                 JOIN run_leases AS child_lease
                   ON child_lease.id = child_source.owner_run_lease_id
                  AND child_lease.run_id = run_waits.child_run_id
                  AND child_lease.workspace_id = child_version.workspace_id
                  AND child_lease.status = 'completed'
                 JOIN runtime_instances AS child_runtime
                   ON child_runtime.id = child_lease.runtime_instance_id
                  AND child_runtime.workspace_id = child_version.workspace_id
                  AND child_runtime.runtime_identity_id = child_lease.runtime_identity_id
                  AND child_runtime.reclaimed_at IS NOT NULL AND child_runtime.reclaim_evidence->>'method' IN ('session_closed', 'host_reconciled', 'provider_absent')
                 JOIN runs AS child
                   ON child.id = child_lease.run_id
                  AND child.parent_run_id = run_waits.run_id
                  AND child.parent_owns_lifecycle
                  AND child.entrypoint_kind = 'task'
                  AND EXISTS (
                      SELECT 1 FROM run_attempts origin
                       WHERE origin.run_id = child.id AND origin.number <= child.current_attempt_number
                         AND origin.workspace_id = child.workspace_id
                         AND origin.base_workspace_version_id = run_checkpoints.private_workspace_version_id
                  )
                  AND child.current_attempt_number = child_lease.attempt_number
                  AND child.current_run_lease_id IS NULL
                  AND child.status = 'succeeded'
                  AND EXISTS (SELECT 1 FROM run_attempts terminal_attempt
                      WHERE terminal_attempt.run_id = child.id
                        AND terminal_attempt.number = child.current_attempt_number
                        AND terminal_attempt.workspace_id = child.workspace_id
                        AND terminal_attempt.base_workspace_version_id = child.base_workspace_version_id
                        AND terminal_attempt.terminal_at IS NOT NULL
                        AND terminal_attempt.terminal_outcome = 'succeeded')
                WHERE child_version.id = run_waits.resume_workspace_version_id
                  AND child_version.workspace_id = run_checkpoints.workspace_id
                  AND child_version.status = 'private'
                  AND child_version.ownership_generation = $14
                  AND child_version.writer_generation = run_waits.child_writer_generation
           )) OR (
               run_waits.condition_status IN ('failed', 'cancelled')
               AND run_waits.resume_workspace_version_id = run_waits.base_workspace_version_id
               AND (run_waits.child_writer_generation IS NULL OR $17::boolean)
           ))
       ))
       -- A recovered restore owns the current fence; its checkpoint source is immutable.
       AND (source_workspace_lease.writer_generation = $15
            OR $16::boolean
            OR (run_waits.resume_workspace_version_id IS NOT NULL
                AND run_waits.child_writer_generation = $15)
            OR (
           source_workspace_lease.writer_generation < $15 AND EXISTS (
               SELECT 1
                 FROM workspace_leases AS restored_workspace_lease
                 JOIN run_leases AS restored_lease
                   ON restored_lease.id = restored_workspace_lease.owner_run_lease_id
                  AND restored_lease.run_id = run_checkpoints.run_id
                  AND restored_lease.attempt_number = run_checkpoints.attempt_number
                  AND restored_lease.workspace_id = run_checkpoints.workspace_id
                  AND restored_lease.status = 'expired'
                  AND restored_lease.terminal_reason_code IN ('lease_expired', 'worker_lost', 'runtime_failed')
                 JOIN runtime_instances AS restored_runtime
                   ON restored_runtime.id = restored_lease.runtime_instance_id
                  AND restored_runtime.workspace_id = run_checkpoints.workspace_id
                  AND restored_runtime.restore_checkpoint_id = run_checkpoints.id
                WHERE restored_workspace_lease.workspace_id = run_checkpoints.workspace_id
                  AND restored_workspace_lease.ownership_generation = $14
                  AND restored_workspace_lease.writer_generation = $15
                  AND restored_workspace_lease.base_workspace_version_id = $5
                  AND restored_workspace_lease.owner_process_id IS NULL
                  AND restored_workspace_lease.status = 'expired'
                  AND restored_workspace_lease.terminal_reason_code = restored_lease.terminal_reason_code
           )
       ))
   ))
  JOIN runtime_instances AS source_runtime
    ON source_runtime.id = source_lease.runtime_instance_id
   AND source_runtime.workspace_id = run_checkpoints.workspace_id
   AND source_runtime.runtime_identity_id = source_lease.runtime_identity_id
   AND source_runtime.deployment_definition_id = $8
   AND source_runtime.program_deployment_id = $9
   AND source_runtime.reserved_cpu_millis = source_lease.requested_cpu_millis
   AND source_runtime.reserved_memory_bytes = source_lease.requested_memory_bytes
   AND source_runtime.reserved_guest_ephemeral_disk_bytes = source_lease.requested_guest_ephemeral_disk_bytes
   AND source_runtime.reserved_execution_slots = source_lease.requested_execution_slots
  JOIN runtime_substrates
    ON runtime_substrates.id = source_runtime.runtime_substrate_id
   AND runtime_substrates.org_id = source_runtime.org_id
   AND runtime_substrates.project_id = source_runtime.project_id
   AND runtime_substrates.environment_id = source_runtime.environment_id
   AND runtime_substrates.deployment_definition_id = source_runtime.deployment_definition_id
 WHERE source_runtime.reclaimed_at IS NOT NULL
   AND source_runtime.reclaim_evidence->>'method' IN ('session_closed', 'host_reconciled', 'provider_absent')
   AND run_waits.id = $1
   AND run_waits.run_id = $2
   AND run_waits.attempt_number = $3
   AND run_waits.workspace_id = $4
	   AND run_waits.suspension_status = 'resume_pending'
	   AND run_waits.resume_request_version = $6
	   AND run_checkpoints.id = $7
	   AND (run_waits.resume_workspace_version_id IS NULL
	        OR run_waits.resume_workspace_version_id = $5)
	 FOR UPDATE OF run_waits, run_checkpoints, computer_versions`,
			authority.resumeRunWaitID,
			authority.runID,
			authority.attemptNumber,
			authority.workspaceID,
			authority.baseWorkspaceVersionID,
			authority.resumeRequestVersion,
			authority.restoreCheckpointID,
			authority.workspaceDefinitionID,
			authority.deploymentID,
			authority.entrypointKind,
			actorCommittedInputSequence,
			actorNextInputSequence-1,
			authority.restoreCheckpointVersion,
			authority.ownershipGeneration, authority.writerGeneration, childOwnsCurrentWriter, terminalChildExcluded,
		).Scan(
			&authority.restoreWorkerGroupID,
			&authority.restoreRuntimeIdentityID,
			&authority.restoreVMVCPUCount,
			&authority.restoreCPUConfigDigest,
			&authority.restoreSubstrateID,
			&authority.restoreSubstrateFormat,
			&authority.restoreSubstrateContract,
			&restoreSourceMountGeneration,
		)
		if err != nil {
			return runPlacementAuthority{}, fmt.Errorf("lock Run placement restore authority: %w", err)
		}
		if !restoreSourceMountGeneration.Valid ||
			restoreSourceMountGeneration.Int64 <= 0 ||
			restoreSourceMountGeneration.Int64 > math.MaxInt64-2 {
			return runPlacementAuthority{}, fmt.Errorf("lock Run placement restore Mount generation: %w", pgx.ErrNoRows)
		}
		authority.restoreMountGeneration = pgtype.Int8{
			Int64: restoreSourceMountGeneration.Int64 + 1,
			Valid: true,
		}
	}

	var deploymentID pgtype.UUID
	err = tx.QueryRow(ctx, `
SELECT deployments.id
  FROM deployments
  JOIN deployment_definitions AS entrypoint_definitions
    ON entrypoint_definitions.environment_id = deployments.environment_id
   AND entrypoint_definitions.deployment_id = deployments.id
   AND entrypoint_definitions.id = $3
   AND entrypoint_definitions.kind = $4
 WHERE deployments.environment_id = $1
   AND deployments.id = $2
   AND deployments.program_artifact_id IS NOT NULL
   AND deployments.runtime_artifact_digest IS NOT NULL
   AND deployments.program_index_digest IS NOT NULL`,
		authority.environmentID,
		authority.deploymentID,
		entrypointDefinitionID,
		authority.entrypointKind,
	).Scan(&deploymentID)
	if err != nil {
		return runPlacementAuthority{}, fmt.Errorf("lock Run placement Deployment authority: %w", err)
	}
	workspaceManifest, err := deployment.ParseSandboxManifest(manifestVersion, manifest)
	if err != nil {
		return runPlacementAuthority{}, fmt.Errorf("parse workspace manifest: %w", err)
	}
	resources, err := normalizeRunResources(workspaceManifest.Resources)
	if err != nil {
		return runPlacementAuthority{}, err
	}
	authority.resources = resources
	authority.architecture = runtimeArchitecture
	return authority, nil
}

// The parent Run and Workspace are locked by placement. A failed child returns
// the parent's original checkpoint, so its logical terminal attempt and physical
// exclusion are independent of which child attempt last owned a writer.
// Scope runtime/mount exclusion to that graph: grant rechecks this after the
// parent's own restore runtime and mount have already been prepared.
// Reclaimed runtimes carry an accepted cleanup receipt, including host/provider
// exclusion whose observed state can remain failed or lost.
func sameWorkspaceTerminalChildExcluded(
	ctx context.Context,
	tx pgx.Tx,
	authority runPlacementAuthority,
) (excluded, ownsCurrentWriter bool, err error) {
	err = tx.QueryRow(ctx, `
WITH RECURSIVE owned(id) AS (
    SELECT child.id
      FROM runs child
      JOIN runs parent ON parent.id = child.parent_run_id
       AND parent.org_id = child.org_id AND parent.project_id = child.project_id
       AND parent.environment_id = child.environment_id
       AND parent.workspace_id = child.workspace_id
     WHERE parent.id = $1 AND child.id = $2 AND child.workspace_id = $3
       AND child.parent_owns_lifecycle AND child.entrypoint_kind = 'task'
       -- A matching Attempt binds the logical child to this parent handoff;
       -- subsequent retries may advance its execution base.
       AND EXISTS (
           SELECT 1 FROM run_attempts origin
            WHERE origin.run_id = child.id AND origin.number <= child.current_attempt_number
              AND origin.workspace_id = $3 AND origin.base_workspace_version_id = $6
       )
       AND (($7 = 'cancelled' AND child.status = 'cancelled')
            OR ($7 = 'failed' AND child.status IN ('failed', 'expired', 'system_failed')))
    UNION
    SELECT child.id
      FROM owned JOIN runs parent ON parent.id = owned.id
      JOIN runs child ON child.parent_run_id = parent.id
       AND child.org_id = parent.org_id AND child.project_id = parent.project_id
       AND child.environment_id = parent.environment_id AND child.workspace_id = $3
       AND child.parent_owns_lifecycle AND child.entrypoint_kind = 'task'
), owned_runtimes AS (
    SELECT lease.runtime_instance_id AS id FROM run_leases lease JOIN owned ON owned.id = lease.run_id
    UNION
    SELECT runtime.id FROM runtime_instances runtime JOIN owned ON owned.id = runtime.reserved_run_id
)
SELECT EXISTS (
    SELECT 1 FROM owned
    JOIN runs child ON child.id = owned.id
    JOIN run_leases lease ON lease.run_id = child.id
     AND lease.workspace_id = $3
     AND lease.status IN ('completed', 'failed', 'cancelled', 'expired', 'lost', 'rejected', 'checkpointed')
     AND lease.terminal_at IS NOT NULL
    JOIN workspace_leases source ON source.owner_run_lease_id = lease.id
     AND source.workspace_id = $3 AND source.ownership_generation = $4
     AND source.writer_generation = $5 AND source.owner_process_id IS NULL
     AND source.status IN ('released', 'fenced', 'expired')
    JOIN runtime_instances runtime ON runtime.id = lease.runtime_instance_id
     AND runtime.id = source.runtime_instance_id AND runtime.workspace_id = $3
     AND runtime.runtime_identity_id = lease.runtime_identity_id
     AND runtime.reclaimed_at IS NOT NULL
),
EXISTS (SELECT 1 FROM owned WHERE id = $2)
AND NOT EXISTS (
    SELECT 1 FROM owned JOIN runs child ON child.id = owned.id
     WHERE child.current_run_lease_id IS NOT NULL
        OR child.status NOT IN ('succeeded', 'failed', 'cancelled', 'expired', 'system_failed')
        OR NOT EXISTS (
            SELECT 1 FROM run_attempts attempt
             WHERE attempt.run_id = child.id AND attempt.number = child.current_attempt_number
               AND attempt.workspace_id = $3 AND attempt.base_workspace_version_id = child.base_workspace_version_id
               AND attempt.terminal_at IS NOT NULL
               AND ((child.status = 'succeeded' AND attempt.terminal_outcome = 'succeeded')
                    OR (child.status = 'cancelled' AND attempt.terminal_outcome = 'cancelled')
                    OR (child.status IN ('failed', 'expired', 'system_failed') AND attempt.terminal_outcome = 'failed'))
        )
)
AND NOT EXISTS (
    SELECT 1 FROM owned_runtimes JOIN runtime_instances runtime ON runtime.id = owned_runtimes.id
     WHERE runtime.reclaimed_at IS NULL
        OR EXISTS (SELECT 1 FROM workspace_mounts mount WHERE mount.runtime_instance_id = runtime.id
            AND mount.status IN ('mounting', 'mounted', 'unmounting'))
)
AND NOT EXISTS (SELECT 1 FROM workspace_leases WHERE workspace_id = $3 AND status IN ('active', 'releasing'))
AND NOT EXISTS (SELECT 1 FROM workspace_processes WHERE workspace_id = $3
    AND status IN ('pending', 'starting', 'running', 'exit_requested'))`,
		authority.runID, authority.resumeChildRunID, authority.workspaceID,
		authority.ownershipGeneration, authority.writerGeneration,
		authority.restoreCheckpointVersion, authority.resumeConditionStatus,
	).Scan(&ownsCurrentWriter, &excluded)
	return excluded, ownsCurrentWriter, err
}

func discoverSameWorkspaceOwnerActor(
	ctx context.Context,
	tx pgx.Tx,
	childRunID pgtype.UUID,
) (pgtype.UUID, pgtype.UUID, error) {
	rows, err := tx.Query(ctx, `
WITH RECURSIVE ancestors AS (
    SELECT parent.id,
           parent.environment_id,
           parent.workspace_id,
           parent.parent_run_id,
           parent.parent_owns_lifecycle,
           parent.session_id
      FROM run_waits AS edge
      JOIN runs AS parent
        ON parent.environment_id = edge.environment_id
       AND parent.id = edge.run_id
       AND parent.workspace_id = edge.workspace_id
       AND parent.status = 'waiting'
       AND parent.current_run_lease_id IS NULL
     WHERE edge.child_run_id = $1
       AND edge.kind = 'child'
       AND EXISTS (
           SELECT 1 FROM runs AS owned_child
            WHERE owned_child.id = edge.child_run_id
              AND owned_child.parent_run_id = edge.run_id
              AND owned_child.environment_id = edge.environment_id
              AND owned_child.parent_owns_lifecycle IS TRUE
       )
       AND edge.condition_status = 'pending'
       AND edge.suspension_status = 'parked'
       AND edge.ownership_generation IS NOT NULL
       AND edge.parent_writer_generation IS NOT NULL
    UNION
    SELECT parent.id,
           parent.environment_id,
           parent.workspace_id,
           parent.parent_run_id,
           parent.parent_owns_lifecycle,
           parent.session_id
      FROM ancestors AS child
      JOIN runs AS parent
        ON parent.environment_id = child.environment_id
       AND parent.id = child.parent_run_id
       AND parent.workspace_id = child.workspace_id
      JOIN run_waits AS edge
        ON edge.environment_id = child.environment_id
       AND edge.run_id = parent.id
       AND edge.workspace_id = child.workspace_id
       AND edge.child_run_id = child.id
       AND edge.kind = 'child'
       AND edge.condition_status = 'pending'
       AND edge.suspension_status = 'parked'
     WHERE child.parent_owns_lifecycle IS TRUE
)
SELECT session_id, id
  FROM ancestors
 WHERE session_id IS NOT NULL`,
		childRunID,
	)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	defer rows.Close()
	var actorID, actorRunID pgtype.UUID
	for rows.Next() {
		if actorID.Valid {
			return pgtype.UUID{}, pgtype.UUID{}, pgx.ErrNoRows
		}
		if err := rows.Scan(&actorID, &actorRunID); err != nil {
			return pgtype.UUID{}, pgtype.UUID{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	return actorID, actorRunID, nil
}

func lockSameWorkspaceAncestors(
	ctx context.Context,
	tx pgx.Tx,
	childRunID pgtype.UUID,
	ownerActorID pgtype.UUID,
	ownerActorRunID pgtype.UUID,
) (pgtype.UUID, pgtype.UUID, error) {
	rows, err := tx.Query(ctx, `
WITH RECURSIVE ancestors AS (
    SELECT edge.id AS wait_id,
           parent.id,
           parent.environment_id,
           parent.workspace_id,
           parent.parent_run_id,
           parent.parent_owns_lifecycle,
           parent.session_id,
           0 AS depth
      FROM run_waits AS edge
      JOIN runs AS parent
        ON parent.environment_id = edge.environment_id
       AND parent.id = edge.run_id
       AND parent.workspace_id = edge.workspace_id
       AND parent.status = 'waiting'
       AND parent.current_run_lease_id IS NULL
     WHERE edge.child_run_id = $1
       AND edge.kind = 'child'
       AND EXISTS (
           SELECT 1 FROM runs AS owned_child
            WHERE owned_child.id = edge.child_run_id
              AND owned_child.parent_run_id = edge.run_id
              AND owned_child.environment_id = edge.environment_id
              AND owned_child.parent_owns_lifecycle IS TRUE
       )
       AND edge.condition_status = 'pending'
       AND edge.suspension_status = 'parked'
       AND edge.ownership_generation IS NOT NULL
       AND edge.parent_writer_generation IS NOT NULL
    UNION ALL
    SELECT child.wait_id,
           parent.id,
           parent.environment_id,
           parent.workspace_id,
           parent.parent_run_id,
           parent.parent_owns_lifecycle,
           parent.session_id,
           child.depth + 1
      FROM ancestors AS child
      JOIN runs AS parent
        ON parent.environment_id = child.environment_id
       AND parent.id = child.parent_run_id
       AND parent.workspace_id = child.workspace_id
      JOIN run_waits AS edge
        ON edge.environment_id = child.environment_id
       AND edge.run_id = parent.id
       AND edge.workspace_id = child.workspace_id
       AND edge.child_run_id = child.id
       AND edge.kind = 'child'
       AND edge.condition_status = 'pending'
       AND edge.suspension_status = 'parked'
     WHERE child.parent_owns_lifecycle IS TRUE
)
SELECT ancestors.wait_id,
       locked_parent.id,
       locked_parent.session_id
  FROM ancestors
  JOIN runs AS locked_parent
    ON locked_parent.id = ancestors.id
 ORDER BY ancestors.depth DESC
 FOR UPDATE OF locked_parent`,
		childRunID,
	)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	defer rows.Close()
	var waitID pgtype.UUID
	var rootRunID pgtype.UUID
	found := false
	actorFound := false
	for rows.Next() {
		var candidateWaitID pgtype.UUID
		var parentID pgtype.UUID
		var parentActorID pgtype.UUID
		if err := rows.Scan(&candidateWaitID, &parentID, &parentActorID); err != nil {
			return pgtype.UUID{}, pgtype.UUID{}, err
		}
		if !found {
			waitID = candidateWaitID
			rootRunID = parentID
			found = true
		} else if waitID != candidateWaitID {
			return pgtype.UUID{}, pgtype.UUID{}, pgx.ErrNoRows
		}
		if parentActorID.Valid {
			if actorFound ||
				!ownerActorID.Valid ||
				parentActorID != ownerActorID ||
				parentID != ownerActorRunID {
				return pgtype.UUID{}, pgtype.UUID{}, pgx.ErrNoRows
			}
			actorFound = true
		}
	}
	if err := rows.Err(); err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	if !found {
		return pgtype.UUID{}, pgtype.UUID{}, nil
	}
	if actorFound != ownerActorID.Valid {
		return pgtype.UUID{}, pgtype.UUID{}, pgx.ErrNoRows
	}
	return waitID, rootRunID, nil
}

func lockRunSecrets(
	ctx context.Context,
	tx pgx.Tx,
	candidate ReadyRunCandidate,
) error {
	secretRows, err := tx.Query(ctx, `
SELECT secrets.status = 'active'
       AND secret_resolutions.id IS NOT NULL
       AND secret_resolutions.revocation_generation = secrets.revocation_generation
  FROM runs
  JOIN workspace_secrets ON workspace_secrets.workspace_id = runs.workspace_id
  JOIN secrets ON secrets.id = workspace_secrets.secret_id
  LEFT JOIN secret_resolutions
    ON secret_resolutions.workspace_id = workspace_secrets.workspace_id
   AND secret_resolutions.run_id = runs.id
   AND secret_resolutions.attempt_number = runs.current_attempt_number
   AND secret_resolutions.placement_kind = workspace_secrets.placement_kind
   AND secret_resolutions.placement_target = workspace_secrets.placement_target
   AND secret_resolutions.secret_id = workspace_secrets.secret_id
 WHERE runs.org_id = $1
   AND runs.id = $2
   AND runs.revision = $3
   AND runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
 ORDER BY secrets.id, workspace_secrets.placement_kind, workspace_secrets.placement_target
 FOR UPDATE OF secrets`,
		candidate.OrgID,
		candidate.RunID,
		candidate.ExpectedRunRevision,
	)
	if err != nil {
		return err
	}
	for secretRows.Next() {
		var valid bool
		if err := secretRows.Scan(&valid); err != nil {
			secretRows.Close()
			return err
		}
		if !valid {
			secretRows.Close()
			return errors.New("run secret resolution is revoked or incomplete")
		}
	}
	if err := secretRows.Err(); err != nil {
		secretRows.Close()
		return err
	}
	secretRows.Close()
	return nil
}

func normalizeRunResources(
	resources deployment.ResourcesManifest,
) (runResources, error) {
	if resources.MilliCPU <= 0 ||
		resources.MemoryMiB <= 0 ||
		resources.MemoryMiB > math.MaxInt64/mebibyte {
		return runResources{}, errors.New("workspace resources are outside the run placement domain")
	}
	return runResources{
		cpuMillis:               resources.MilliCPU,
		memoryBytes:             resources.MemoryMiB * mebibyte,
		guestEphemeralDiskBytes: compute.WorkspaceGuestEphemeralDiskMiB * mebibyte,
		executionSlots:          1,
	}, nil
}

func lockRunQueueScope(
	ctx context.Context,
	tx pgx.Tx,
	candidate ReadyRunCandidate,
) (pgtype.UUID, string, pgtype.Text, error) {
	environmentID, queueName, concurrencyKey, err := discoverRunQueueScope(
		ctx,
		tx,
		candidate,
	)
	if err != nil {
		return pgtype.UUID{}, "", pgtype.Text{}, err
	}
	key, err := queueScopeLockKey(environmentID, queueName, concurrencyKey)
	if err != nil {
		return pgtype.UUID{}, "", pgtype.Text{}, err
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", key); err != nil {
		return pgtype.UUID{}, "", pgtype.Text{}, fmt.Errorf("lock run queue scope: %w", err)
	}
	return environmentID, queueName, concurrencyKey, nil
}

// checkRunPreparationDeadlines is a final preparation check, after provider and
// Runtime work that may have waited. The caller holds Run/ancestor Run locks
// and its restore checkpoint lock. Parent checkpoint lifecycle changes also
// require the parent Run; an expiry reaper cannot invalidate this authority for us.
func checkRunPreparationDeadlines(ctx context.Context, tx pgx.Tx, authority runPlacementAuthority) error {
	var id pgtype.UUID
	err := tx.QueryRow(ctx, `
SELECT runs.id FROM runs
 WHERE runs.id = $1 AND runs.current_attempt_number = $2
   AND (runs.first_lease_at IS NOT NULL OR runs.queued_expires_at IS NULL
        OR runs.queued_expires_at > clock_timestamp())
   AND ($3::uuid IS NULL OR EXISTS (
       SELECT 1 FROM run_checkpoints
        WHERE run_checkpoints.id = $3
          AND run_checkpoints.run_id = runs.id
          AND run_checkpoints.attempt_number = runs.current_attempt_number
          AND run_checkpoints.workspace_id = runs.workspace_id
          AND run_checkpoints.status = 'ready'
          AND (run_checkpoints.expires_at IS NULL
               OR run_checkpoints.expires_at > clock_timestamp())
   ))
   AND ($4::uuid IS NULL OR EXISTS (
       SELECT 1 FROM run_waits AS parent_wait
       JOIN run_checkpoints AS parent_checkpoint
         ON parent_checkpoint.id = parent_wait.suspend_checkpoint_id
        AND parent_checkpoint.run_id = parent_wait.run_id
        AND parent_checkpoint.attempt_number = parent_wait.attempt_number
        AND parent_checkpoint.workspace_id = parent_wait.workspace_id
        AND parent_checkpoint.status = 'ready'
        WHERE parent_wait.id = $4
          AND parent_wait.child_run_id = runs.id
          AND parent_wait.workspace_id = runs.workspace_id
          AND (parent_checkpoint.expires_at IS NULL
               OR parent_checkpoint.expires_at > clock_timestamp())
   ))
`, authority.runID, authority.attemptNumber, authority.restoreCheckpointID, authority.sameWorkspaceChildWaitID).Scan(&id)
	if err != nil {
		return fmt.Errorf("recheck Run preparation deadlines: %w", classifyRunCandidateError(err))
	}
	return nil
}
