package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/helmrdotdev/helmr/internal/compute"
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
	baseVersionID             pgtype.UUID
	restoreCheckpointID       pgtype.UUID
	restoreCheckpointVersion  pgtype.UUID
	resumeRunWaitID           pgtype.UUID
	resumeRequestVersion      int64
	restoreWorkerGroupID      string
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
	stateVersion              int64
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
   AND state_version = $3`,
		candidate.OrgID,
		candidate.RunID,
		candidate.ExpectedRunStateVersion,
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
   AND state_version = $3`,
		candidate.OrgID,
		candidate.RunID,
		candidate.ExpectedRunStateVersion,
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
   AND state IN ('open', 'closing')
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
       runs.state_version,
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
          AND parent.current_run_lease_id IS NULL
         JOIN run_checkpoints AS checkpoint
           ON checkpoint.id = edge.suspend_checkpoint_id
	          AND checkpoint.run_id = edge.run_id
          AND checkpoint.attempt_number = edge.attempt_number
          AND checkpoint.run_wait_id = edge.id
          AND checkpoint.workspace_id = edge.workspace_id
          AND checkpoint.state = 'ready'
          AND (checkpoint.expires_at IS NULL
               OR checkpoint.expires_at > transaction_timestamp())
         JOIN workspace_versions AS base
           ON base.workspace_id = edge.workspace_id
          AND base.id = edge.base_workspace_version_id
          AND base.state = 'private'
         LEFT JOIN LATERAL (
	              SELECT child_workspace_lease.id
	                FROM run_leases AS child_lease
	                JOIN workspace_leases AS child_workspace_lease
	                  ON child_workspace_lease.owner_run_lease_id = child_lease.id
	                 AND child_workspace_lease.workspace_id = child_lease.workspace_id
				 AND (
				     child_workspace_lease.base_version_id = edge.base_workspace_version_id
				     OR EXISTS (
				         SELECT 1
				           FROM run_waits AS resume_edge
				          WHERE resume_edge.run_id = runs.id
				            AND resume_edge.workspace_id = runs.workspace_id
				            AND resume_edge.suspension_state = 'resume_pending'
				            AND resume_edge.ownership_generation = edge.ownership_generation
				            AND resume_edge.resume_writer_generation IS NULL
				            AND resume_edge.resume_workspace_version_id =
				                child_workspace_lease.base_version_id
				     )
				 )
                 AND child_workspace_lease.ownership_generation =
                     edge.ownership_generation
                 AND child_workspace_lease.writer_generation =
                     edge.child_writer_generation
                 AND child_workspace_lease.state IN ('released', 'fenced', 'expired', 'lost')
               WHERE child_lease.run_id = runs.id
                 AND child_lease.workspace_id = runs.workspace_id
                 AND (
                     child_lease.state IN ('failed', 'expired', 'lost', 'rejected')
                     OR (
                         child_lease.state = 'checkpointed'
                         AND EXISTS (
                             SELECT 1
                              FROM run_waits AS resume_edge
                              WHERE resume_edge.run_id = runs.id
                                AND resume_edge.attempt_number = child_lease.attempt_number
                                AND resume_edge.workspace_id = runs.workspace_id
                                AND resume_edge.suspension_state = 'resume_pending'
                                AND resume_edge.prior_run_lease_id = child_lease.id
                                AND resume_edge.ownership_generation = edge.ownership_generation
                                AND resume_edge.parent_writer_generation =
                                    child_workspace_lease.writer_generation
                                AND resume_edge.resume_writer_generation IS NULL
                         )
                     )
                 )
               ORDER BY child_lease.lease_sequence DESC
               LIMIT 1
         ) AS prior_child ON edge.child_writer_generation IS NOT NULL
        WHERE edge.child_run_id = runs.id
          AND edge.child_parent_owned IS TRUE
          AND edge.workspace_id = runs.workspace_id
          AND edge.condition_state = 'pending'
          AND edge.suspension_state = 'parked'
          AND edge.base_workspace_version_id =
              runs.base_workspace_version_id
	          AND edge.ownership_generation IS NOT NULL
	          AND edge.parent_writer_generation IS NOT NULL
	          AND (edge.child_writer_generation IS NULL
	               OR prior_child.id IS NOT NULL)
	  ) AS child_wait ON true
	  LEFT JOIN LATERAL (
	       SELECT run_waits.id,
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
	          AND run_waits.suspension_state = 'resume_pending'
	  ) AS restore_wait ON true
	  LEFT JOIN run_checkpoints AS restore_checkpoint
	    ON restore_checkpoint.id = restore_wait.suspend_checkpoint_id
   AND restore_checkpoint.run_id = runs.id
   AND restore_checkpoint.attempt_number = restore_wait.attempt_number
   AND restore_checkpoint.run_wait_id = restore_wait.id
   AND restore_checkpoint.workspace_id = restore_wait.workspace_id
   AND restore_checkpoint.state = 'ready'
   AND (restore_checkpoint.expires_at IS NULL
        OR restore_checkpoint.expires_at > transaction_timestamp())
	  LEFT JOIN workspace_versions AS restore_version
	    ON restore_version.workspace_id = restore_checkpoint.workspace_id
	   AND restore_version.id = coalesce(
	       restore_wait.resume_workspace_version_id,
	       restore_checkpoint.private_workspace_version_id
	   )
   AND restore_version.state = 'private'
 WHERE runs.org_id = $1
   AND runs.id = $2
   AND runs.state_version = $3
   AND runs.entrypoint_kind = $4
   AND (($4 = 'task' AND runs.session_id IS NULL
         AND runs.cause_kind IN ('api', 'manual', 'schedule', 'child'))
        OR ($4 = 'actor' AND runs.session_id = $5
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
                  AND run_waits.suspension_state IN (
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
		candidate.ExpectedRunStateVersion,
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
		&authority.stateVersion,
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
		&authority.resumeRequestVersion,
		&authority.restoreCheckpointID,
		&authority.restoreCheckpointVersion,
		&authority.baseVersionID,
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
			!authority.resumeChildWriter.Valid ||
			!authority.baseVersionID.Valid) {
		return runPlacementAuthority{}, fmt.Errorf("lock Run placement same-Workspace resume shape: %w", pgx.ErrNoRows)
	}
	var manifest []byte
	workspaceOwnerPredicate := "workspaces.owner_run_id = $5 AND workspaces.owner_session_id IS NULL"
	workspaceOwnerID := authority.runID
	if authority.sameWorkspaceChildWaitID.Valid {
		if authority.ownerActorID.Valid {
			workspaceOwnerPredicate = "workspaces.owner_session_id = $5 AND workspaces.owner_run_id IS NULL"
			workspaceOwnerID = authority.ownerActorID
		} else {
			workspaceOwnerID = sameWorkspaceRootRunID
		}
	} else if authority.entrypointKind == "actor" {
		workspaceOwnerPredicate = "workspaces.owner_session_id = $5 AND workspaces.owner_run_id IS NULL"
		workspaceOwnerID = authority.actorID
	}
	err = tx.QueryRow(ctx, fmt.Sprintf(`
SELECT workspaces.deployment_definition_id,
       workspaces.region_id,
       workspaces.ownership_generation,
       workspaces.writer_generation,
       workspace_definitions.manifest
  FROM workspaces
  JOIN environments AS workspace_environment
    ON workspace_environment.id = workspaces.environment_id
  JOIN deployment_definitions AS workspace_definitions
    ON workspace_definitions.environment_id = workspaces.environment_id
   AND workspace_definitions.id = workspaces.deployment_definition_id
   AND workspace_definitions.kind = 'sandbox'
   AND workspace_definitions.declared_id = workspaces.sandbox_declared_id
 WHERE workspace_environment.org_id = $1
   AND workspace_environment.project_id = $2
   AND workspaces.environment_id = $3
   AND workspaces.id = $4
   AND workspaces.state = 'active'
   AND workspaces.desired_state = 'active'
   AND workspaces.dirty_state = 'clean'
   AND %s
 FOR UPDATE OF workspaces`, workspaceOwnerPredicate),
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
	if authority.sameWorkspaceResume {
		if authority.resumeOwnership.Int64 != authority.ownershipGeneration ||
			authority.resumeChildWriter.Int64 > authority.writerGeneration {
			return runPlacementAuthority{}, fmt.Errorf("lock Run placement same-Workspace resume generation: %w", pgx.ErrNoRows)
		}
		if authority.resumeChildWriter.Int64 != authority.writerGeneration {
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
       AND run_leases.state = 'expired'
       AND run_leases.terminal_reason_code IN (
           'lease_expired',
           'worker_lost',
           'runtime_failed'
       )
     WHERE workspace_leases.workspace_id = $1
       AND workspace_leases.ownership_generation = $2
       AND workspace_leases.writer_generation = $3
       AND workspace_leases.base_version_id = $4
       AND workspace_leases.state = 'expired'
)`,
				authority.workspaceID,
				authority.ownershipGeneration,
				authority.writerGeneration,
				authority.baseVersionID,
				authority.runID,
				authority.attemptNumber,
			).Scan(&fenced)
			if err != nil || !fenced.Valid || !fenced.Bool {
				return runPlacementAuthority{}, fmt.Errorf("lock Run placement same-Workspace resume fence: %w", pgx.ErrNoRows)
			}
		}
	}

	var attemptBaseVersionID pgtype.UUID
	var attemptSessionInputStartSequence pgtype.Int8
	err = tx.QueryRow(ctx, `
SELECT run_attempts.base_workspace_version_id,
       run_attempts.session_input_start_sequence
  FROM run_attempts
  JOIN workspace_versions
    ON workspace_versions.workspace_id = run_attempts.workspace_id
   AND workspace_versions.id = run_attempts.base_workspace_version_id
   AND (($5::boolean AND workspace_versions.state = 'private')
        OR (NOT $5::boolean AND workspace_versions.state = 'committed'))
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
	).Scan(&attemptBaseVersionID, &attemptSessionInputStartSequence)
	if err != nil {
		return runPlacementAuthority{}, fmt.Errorf("lock Run placement Attempt authority: %w", err)
	}
	if !authority.baseVersionID.Valid {
		authority.baseVersionID = attemptBaseVersionID
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
	   AND run_checkpoints.state = 'ready'
	   AND run_checkpoints.base_workspace_version_id = $8
	   AND run_checkpoints.private_workspace_version_id = $14
   AND (($11 = 'task' AND run_checkpoints.actor_speculative_input_sequence IS NULL)
        OR ($11 = 'actor'
            AND run_checkpoints.actor_speculative_input_sequence BETWEEN $12 AND $13))
   AND (run_checkpoints.expires_at IS NULL
        OR run_checkpoints.expires_at > transaction_timestamp())
  JOIN workspace_versions
    ON workspace_versions.workspace_id = run_checkpoints.workspace_id
   AND workspace_versions.id = run_checkpoints.private_workspace_version_id
   AND workspace_versions.state = 'private'
  JOIN run_leases AS source_lease
    ON source_lease.id = run_checkpoints.source_run_lease_id
   AND source_lease.run_id = run_checkpoints.run_id
   AND source_lease.attempt_number = run_checkpoints.attempt_number
   AND source_lease.workspace_id = run_checkpoints.workspace_id
   AND source_lease.state = 'checkpointed'
	  JOIN workspace_leases AS source_workspace_lease
    ON source_workspace_lease.id = run_checkpoints.source_workspace_lease_id
   AND source_workspace_lease.workspace_id = run_checkpoints.workspace_id
   AND source_workspace_lease.owner_run_lease_id = source_lease.id
	   AND source_workspace_lease.base_version_id = run_checkpoints.base_workspace_version_id
   AND source_workspace_lease.state IN ('released', 'fenced')
  JOIN runtime_instances AS source_runtime
    ON source_runtime.id = source_lease.runtime_instance_id
   AND source_runtime.workspace_id = run_checkpoints.workspace_id
   AND source_runtime.runtime_identity_id = source_lease.runtime_identity_id
   AND source_runtime.deployment_definition_id = $9
   AND source_runtime.program_deployment_id = $10
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
 WHERE run_waits.id = $1
   AND run_waits.run_id = $2
   AND run_waits.attempt_number = $3
   AND run_waits.workspace_id = $4
	   AND run_waits.suspension_state = 'resume_pending'
	   AND run_waits.resume_request_version = $6
	   AND run_checkpoints.id = $7
	   AND (run_waits.resume_workspace_version_id IS NULL
	        OR run_waits.resume_workspace_version_id = $5)
	 FOR UPDATE OF run_waits, run_checkpoints, workspace_versions`,
			authority.resumeRunWaitID,
			authority.runID,
			authority.attemptNumber,
			authority.workspaceID,
			authority.baseVersionID,
			authority.resumeRequestVersion,
			authority.restoreCheckpointID,
			attemptBaseVersionID,
			authority.workspaceDefinitionID,
			authority.deploymentID,
			authority.entrypointKind,
			actorCommittedInputSequence,
			actorNextInputSequence-1,
			authority.restoreCheckpointVersion,
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
	var workspaceManifest deployment.SandboxManifest
	decoder := json.NewDecoder(bytes.NewReader(manifest))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&workspaceManifest); err != nil {
		return runPlacementAuthority{}, fmt.Errorf("decode workspace manifest: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return runPlacementAuthority{}, fmt.Errorf("decode workspace manifest: %w", err)
	}
	resources, err := normalizeRunResources(workspaceManifest.Resources)
	if err != nil {
		return runPlacementAuthority{}, err
	}
	authority.resources = resources
	authority.architecture = runtimeArchitecture
	return authority, nil
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
       AND edge.child_parent_owned IS TRUE
       AND edge.condition_state = 'pending'
       AND edge.suspension_state = 'parked'
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
       AND edge.child_parent_owned IS TRUE
       AND edge.condition_state = 'pending'
       AND edge.suspension_state = 'parked'
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
       AND edge.child_parent_owned IS TRUE
       AND edge.condition_state = 'pending'
       AND edge.suspension_state = 'parked'
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
       AND edge.child_parent_owned IS TRUE
       AND edge.condition_state = 'pending'
       AND edge.suspension_state = 'parked'
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
SELECT secrets.state = 'active'
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
   AND runs.state_version = $3
   AND runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
 ORDER BY secrets.id, workspace_secrets.placement_kind, workspace_secrets.placement_target
 FOR UPDATE OF secrets`,
		candidate.OrgID,
		candidate.RunID,
		candidate.ExpectedRunStateVersion,
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

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("contains trailing JSON value")
}
