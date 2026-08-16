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
	resumeRunWaitID           pgtype.UUID
	resumeRequestVersion      int64
	restoreRuntimeID          pgtype.UUID
	restoreWorkerGroupID      string
	restoreRuntimeIdentityID  string
	restoreVMVCPUCount        int32
	restoreCPUConfigDigest    string
	restoreSubstrateID        pgtype.UUID
	restoreSubstrateFormat    string
	restoreSubstrateContract  string
	sameWorkspaceResume       bool
	handoffResumeSucceeded    bool
	resumeHandoffRuntimeID    pgtype.UUID
	resumeHandoffMountID      pgtype.UUID
	resumeHandoffMountGen     pgtype.Int8
	resumeHandoffOwnership    pgtype.Int8
	resumeHandoffParentWriter pgtype.Int8
	resumeHandoffChildWriter  pgtype.Int8
	handoffChildWaitID        pgtype.UUID
	handoffRuntimeID          pgtype.UUID
	handoffWorkspaceMountID   pgtype.UUID
	handoffMountGeneration    pgtype.Int8
	handoffAdmissionMountGen  pgtype.Int8
	handoffOwnership          pgtype.Int8
	handoffParentWriter       pgtype.Int8
	handoffChildWriter        pgtype.Int8
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

func (a runPlacementAuthority) retainedHandoffRuntimeID() (pgtype.UUID, bool) {
	if a.sameWorkspaceResume {
		if !a.handoffResumeSucceeded || !a.resumeHandoffRuntimeID.Valid {
			return pgtype.UUID{}, false
		}
		if a.handoffChildWaitID.Valid &&
			a.handoffRuntimeID != a.resumeHandoffRuntimeID {
			return pgtype.UUID{}, false
		}
		return a.resumeHandoffRuntimeID, true
	}
	if a.handoffChildWaitID.Valid {
		return a.handoffRuntimeID, a.handoffRuntimeID.Valid
	}
	return pgtype.UUID{}, false
}

func (a runPlacementAuthority) usesRetainedHandoff(runtimeID pgtype.UUID) bool {
	retainedID, ok := a.retainedHandoffRuntimeID()
	return ok && retainedID == runtimeID
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
	handoffHintID, err := lockSameWorkspaceHandoffAncestors(
		ctx,
		tx,
		candidate.RunID,
		authority.ownerActorID,
		authority.ownerActorRunID,
	)
	if err != nil {
		return runPlacementAuthority{}, fmt.Errorf("lock Run placement handoff ancestors: %w", err)
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
       child_handoff.id,
       child_handoff.handoff_runtime_instance_id,
       child_handoff.handoff_workspace_mount_id,
       child_handoff.handoff_mount_generation,
       child_handoff.admission_mount_generation,
       child_handoff.ownership_generation,
       child_handoff.parent_writer_generation,
       child_handoff.child_writer_generation,
	       restore_wait.id,
	       coalesce(restore_wait.resume_request_version, 0),
	       restore_wait.checkpoint_id,
	       restore_checkpoint.private_workspace_version_id,
	       restore_wait.handoff_runtime_instance_id,
	       restore_wait.handoff_workspace_mount_id,
	       restore_wait.handoff_mount_generation,
	       restore_wait.ownership_generation,
	       restore_wait.parent_writer_generation,
	       restore_wait.child_writer_generation,
	       coalesce(restore_wait.condition_state = 'completed', false),
	       runs.session_input_start_sequence,
       runs.session_input_high_watermark
  FROM runs
  LEFT JOIN LATERAL (
       SELECT handoff.id,
              handoff.handoff_runtime_instance_id,
              handoff.handoff_workspace_mount_id,
              handoff.handoff_mount_generation,
              coalesce(
                  prior_child.mount_fencing_generation,
                  handoff.handoff_mount_generation
              ) AS admission_mount_generation,
              handoff.ownership_generation,
              handoff.parent_writer_generation,
              handoff.child_writer_generation
         FROM run_waits AS handoff
         JOIN runs AS parent
           ON parent.environment_id = handoff.environment_id
          AND parent.id = handoff.run_id
          AND parent.workspace_id = handoff.workspace_id
          AND parent.status = 'waiting'
          AND parent.current_run_lease_id IS NULL
         JOIN run_checkpoints AS checkpoint
           ON checkpoint.id = handoff.suspend_checkpoint_id
          AND checkpoint.kind = 'suspend'
          AND checkpoint.run_id = handoff.run_id
          AND checkpoint.attempt_number = handoff.attempt_number
          AND checkpoint.run_wait_id = handoff.id
          AND checkpoint.workspace_id = handoff.workspace_id
          AND checkpoint.state = 'ready'
          AND (checkpoint.expires_at IS NULL
               OR checkpoint.expires_at > transaction_timestamp())
         JOIN workspace_versions AS base
           ON base.workspace_id = handoff.workspace_id
          AND base.id = handoff.base_workspace_version_id
          AND base.state = 'private'
         LEFT JOIN LATERAL (
              SELECT child_workspace_lease.mount_fencing_generation
                FROM run_leases AS child_lease
                JOIN workspace_leases AS child_workspace_lease
                  ON child_workspace_lease.owner_run_lease_id = child_lease.id
                 AND child_workspace_lease.workspace_id = child_lease.workspace_id
                 AND child_workspace_lease.runtime_instance_id =
                     handoff.handoff_runtime_instance_id
                 AND child_workspace_lease.workspace_mount_id =
                     handoff.handoff_workspace_mount_id
                 AND child_workspace_lease.base_version_id =
                     handoff.base_workspace_version_id
                 AND child_workspace_lease.ownership_generation =
                     handoff.ownership_generation
                 AND child_workspace_lease.writer_generation =
                     handoff.child_writer_generation
                 AND child_workspace_lease.state IN ('released', 'fenced')
               WHERE child_lease.run_id = runs.id
                 AND child_lease.workspace_id = runs.workspace_id
                 AND child_lease.state IN ('failed', 'expired', 'lost', 'rejected')
               ORDER BY child_lease.lease_sequence DESC
               LIMIT 1
         ) AS prior_child ON handoff.child_writer_generation IS NOT NULL
        WHERE handoff.child_run_id = runs.id
          AND handoff.child_parent_owned IS TRUE
          AND handoff.workspace_id = runs.workspace_id
          AND handoff.condition_state = 'pending'
          AND handoff.suspension_state = 'parked'
          AND handoff.base_workspace_version_id =
              runs.base_workspace_version_id
          AND handoff.handoff_runtime_instance_id IS NOT NULL
          AND handoff.handoff_workspace_mount_id IS NOT NULL
          AND handoff.handoff_mount_generation IS NOT NULL
          AND handoff.ownership_generation IS NOT NULL
          AND handoff.parent_writer_generation IS NOT NULL
          AND (handoff.child_writer_generation IS NULL
               OR prior_child.mount_fencing_generation IS NOT NULL)
  ) AS child_handoff ON true
  LEFT JOIN LATERAL (
	       SELECT run_waits.id,
	              run_waits.resume_request_version,
	              CASE
	                  WHEN run_waits.handoff_runtime_instance_id IS NOT NULL
	                   AND run_waits.condition_state = 'completed'
	                  THEN run_waits.handoff_resume_checkpoint_id
	                  ELSE run_waits.suspend_checkpoint_id
	              END AS checkpoint_id,
	              run_waits.attempt_number,
	              run_waits.workspace_id,
	              run_waits.handoff_runtime_instance_id,
	              run_waits.handoff_workspace_mount_id,
	              run_waits.handoff_mount_generation,
	              run_waits.ownership_generation,
	              run_waits.parent_writer_generation,
	              run_waits.child_writer_generation,
	              run_waits.condition_state
	         FROM run_waits
	        WHERE run_waits.run_id = runs.id
	          AND run_waits.suspension_state = 'resume_pending'
	          AND (
	              (run_waits.handoff_runtime_instance_id IS NULL
	               AND run_waits.handoff_workspace_mount_id IS NULL
	               AND run_waits.handoff_resume_checkpoint_id IS NULL)
	              OR
	              (run_waits.handoff_runtime_instance_id IS NOT NULL
	               AND run_waits.handoff_workspace_mount_id IS NOT NULL
	               AND run_waits.child_run_id IS NOT NULL
	               AND run_waits.child_writer_generation IS NOT NULL
	               AND run_waits.resume_writer_generation IS NULL
	               AND run_waits.resume_workspace_version_id IS NOT NULL
	               AND run_waits.condition_state IN ('completed', 'failed', 'cancelled')
	               AND (
	                   (run_waits.condition_state = 'completed'
	                    AND run_waits.handoff_resume_checkpoint_id IS NOT NULL)
	                   OR
	                   (run_waits.condition_state IN ('failed', 'cancelled')
	                    AND run_waits.handoff_resume_checkpoint_id IS NULL)
	               ))
	          )
	  ) AS restore_wait ON true
	  LEFT JOIN run_checkpoints AS restore_checkpoint
	    ON restore_checkpoint.id = restore_wait.checkpoint_id
	   AND restore_checkpoint.kind = CASE
	       WHEN restore_wait.handoff_runtime_instance_id IS NOT NULL
	        AND restore_wait.condition_state = 'completed'
	       THEN 'handoff_resume'::run_checkpoint_kind
	       ELSE 'suspend'::run_checkpoint_kind
	   END
   AND restore_checkpoint.run_id = runs.id
   AND restore_checkpoint.attempt_number = restore_wait.attempt_number
   AND restore_checkpoint.run_wait_id = restore_wait.id
   AND restore_checkpoint.workspace_id = restore_wait.workspace_id
   AND restore_checkpoint.state = 'ready'
   AND (restore_checkpoint.expires_at IS NULL
        OR restore_checkpoint.expires_at > transaction_timestamp())
  LEFT JOIN workspace_versions AS restore_version
    ON restore_version.workspace_id = restore_checkpoint.workspace_id
   AND restore_version.id = restore_checkpoint.private_workspace_version_id
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
   AND child_handoff.id IS NOT DISTINCT FROM $6::uuid
   AND (
       ($4 IN ('task', 'actor') AND child_handoff.id IS NULL AND
           restore_wait.id IS NULL
           AND NOT EXISTS (
               SELECT 1
                 FROM run_waits
                WHERE run_waits.run_id = runs.id
                  AND run_waits.suspension_state IN (
                      'hot', 'checkpointing', 'parked', 'resume_pending', 'resuming'
                  )
           ))
       OR ($4 = 'task' AND child_handoff.id IS NOT NULL)
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
		handoffHintID,
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
		&authority.handoffChildWaitID,
		&authority.handoffRuntimeID,
		&authority.handoffWorkspaceMountID,
		&authority.handoffMountGeneration,
		&authority.handoffAdmissionMountGen,
		&authority.handoffOwnership,
		&authority.handoffParentWriter,
		&authority.handoffChildWriter,
		&authority.resumeRunWaitID,
		&authority.resumeRequestVersion,
		&authority.restoreCheckpointID,
		&authority.baseVersionID,
		&authority.resumeHandoffRuntimeID,
		&authority.resumeHandoffMountID,
		&authority.resumeHandoffMountGen,
		&authority.resumeHandoffOwnership,
		&authority.resumeHandoffParentWriter,
		&authority.resumeHandoffChildWriter,
		&authority.handoffResumeSucceeded,
		&actorStartInputSequence,
		&actorStartInputHighWatermark,
	)
	if err != nil {
		return runPlacementAuthority{}, fmt.Errorf("lock Run placement Run authority: %w", err)
	}
	authority.sameWorkspaceResume = authority.resumeHandoffRuntimeID.Valid
	if authority.sameWorkspaceResume &&
		(!authority.resumeHandoffMountID.Valid ||
			!authority.resumeHandoffMountGen.Valid ||
			!authority.resumeHandoffOwnership.Valid ||
			!authority.resumeHandoffParentWriter.Valid ||
			!authority.resumeHandoffChildWriter.Valid) {
		return runPlacementAuthority{}, fmt.Errorf("lock Run placement resume handoff shape: %w", pgx.ErrNoRows)
	}
	// A nested failed handoff cannot migrate its enclosing same-kernel edge.
	// Its completion transaction unwinds that edge before placement can see it.
	if authority.sameWorkspaceResume && authority.handoffChildWaitID.Valid &&
		!authority.handoffResumeSucceeded {
		return runPlacementAuthority{}, fmt.Errorf("lock Run placement nested handoff outcome: %w", pgx.ErrNoRows)
	}
	var manifest []byte
	workspaceOwnerPredicate := "workspaces.owner_run_id = $5 AND workspaces.owner_session_id IS NULL"
	workspaceOwnerID := authority.runID
	if authority.handoffChildWaitID.Valid {
		workspaceOwnerPredicate = `(
			(workspaces.owner_run_id IS NOT NULL
			 OR workspaces.owner_session_id IS NOT NULL)
			AND $5::uuid IS NULL
		)`
		workspaceOwnerID = pgtype.UUID{}
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
	if authority.handoffChildWaitID.Valid && !authority.sameWorkspaceResume {
		if !authority.handoffMountGeneration.Valid ||
			!authority.handoffAdmissionMountGen.Valid ||
			!authority.handoffOwnership.Valid ||
			authority.handoffOwnership.Int64 != authority.ownershipGeneration ||
			!authority.handoffParentWriter.Valid {
			return runPlacementAuthority{}, fmt.Errorf("lock Run placement child handoff shape: %w", pgx.ErrNoRows)
		}
		expectedWriter := authority.handoffParentWriter.Int64
		if authority.handoffChildWriter.Valid {
			expectedWriter = authority.handoffChildWriter.Int64
		}
		if expectedWriter != authority.writerGeneration {
			return runPlacementAuthority{}, fmt.Errorf("lock Run placement child writer: %w", pgx.ErrNoRows)
		}
	}
	if authority.sameWorkspaceResume {
		if authority.resumeHandoffOwnership.Int64 != authority.ownershipGeneration ||
			authority.resumeHandoffChildWriter.Int64 > authority.writerGeneration {
			return runPlacementAuthority{}, fmt.Errorf("lock Run placement resume handoff generation: %w", pgx.ErrNoRows)
		}
		if authority.handoffResumeSucceeded &&
			authority.resumeHandoffChildWriter.Int64 != authority.writerGeneration {
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
				return runPlacementAuthority{}, fmt.Errorf("lock Run placement resume handoff fence: %w", pgx.ErrNoRows)
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
		authority.handoffChildWaitID.Valid,
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
		err = tx.QueryRow(ctx, `
SELECT source_runtime.id,
       source_lease.worker_group_id,
       source_runtime.runtime_identity_id,
       source_runtime.vm_vcpu_count,
       source_runtime.cpu_config_digest,
       source_runtime.runtime_substrate_id,
	       runtime_substrates.substrate_format,
	       runtime_substrates.substrate_contract
	  FROM run_waits
	  JOIN run_checkpoints
	    ON run_checkpoints.id = CASE
	        WHEN $14::boolean AND run_waits.condition_state = 'completed'
	        THEN run_waits.handoff_resume_checkpoint_id
	        ELSE run_waits.suspend_checkpoint_id
	    END
	   AND run_checkpoints.kind = CASE
	        WHEN $14::boolean AND run_waits.condition_state = 'completed'
	        THEN 'handoff_resume'::run_checkpoint_kind
	        ELSE 'suspend'::run_checkpoint_kind
	    END
   AND run_checkpoints.run_id = run_waits.run_id
   AND run_checkpoints.attempt_number = run_waits.attempt_number
   AND run_checkpoints.run_wait_id = run_waits.id
   AND run_checkpoints.workspace_id = run_waits.workspace_id
   AND run_checkpoints.state = 'ready'
   AND run_checkpoints.base_workspace_version_id = $8
   AND run_checkpoints.private_workspace_version_id = $5
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
	   AND (
	       (NOT $14::boolean
	        AND run_waits.handoff_runtime_instance_id IS NULL
	        AND run_waits.handoff_workspace_mount_id IS NULL
	        AND run_waits.handoff_resume_checkpoint_id IS NULL)
	       OR
	       ($14::boolean
	        AND run_waits.handoff_runtime_instance_id IS NOT NULL
	        AND run_waits.handoff_workspace_mount_id IS NOT NULL
	        AND run_waits.resume_workspace_version_id = $5
	        AND run_waits.child_writer_generation IS NOT NULL
	        AND run_waits.resume_writer_generation IS NULL
	        AND (
	            (run_waits.condition_state = 'completed'
	             AND run_checkpoints.kind = 'handoff_resume'
	             AND run_waits.handoff_resume_checkpoint_id = run_checkpoints.id)
	            OR
	            (run_waits.condition_state IN ('failed', 'cancelled')
	             AND run_checkpoints.kind = 'suspend'
	             AND run_waits.handoff_resume_checkpoint_id IS NULL)
	        ))
	   )
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
			authority.sameWorkspaceResume,
		).Scan(
			&authority.restoreRuntimeID,
			&authority.restoreWorkerGroupID,
			&authority.restoreRuntimeIdentityID,
			&authority.restoreVMVCPUCount,
			&authority.restoreCPUConfigDigest,
			&authority.restoreSubstrateID,
			&authority.restoreSubstrateFormat,
			&authority.restoreSubstrateContract,
		)
		if err != nil {
			return runPlacementAuthority{}, fmt.Errorf("lock Run placement restore authority: %w", err)
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
      FROM run_waits AS handoff
      JOIN runs AS parent
        ON parent.environment_id = handoff.environment_id
       AND parent.id = handoff.run_id
       AND parent.workspace_id = handoff.workspace_id
       AND parent.status = 'waiting'
       AND parent.current_run_lease_id IS NULL
     WHERE handoff.child_run_id = $1
       AND handoff.child_parent_owned IS TRUE
       AND handoff.condition_state = 'pending'
       AND handoff.suspension_state = 'parked'
       AND handoff.handoff_runtime_instance_id IS NOT NULL
       AND handoff.handoff_workspace_mount_id IS NOT NULL
       AND handoff.handoff_mount_generation IS NOT NULL
       AND handoff.ownership_generation IS NOT NULL
       AND handoff.parent_writer_generation IS NOT NULL
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

func lockSameWorkspaceHandoffAncestors(
	ctx context.Context,
	tx pgx.Tx,
	childRunID pgtype.UUID,
	ownerActorID pgtype.UUID,
	ownerActorRunID pgtype.UUID,
) (pgtype.UUID, error) {
	rows, err := tx.Query(ctx, `
WITH RECURSIVE ancestors AS (
    SELECT handoff.id AS wait_id,
           parent.id,
           parent.environment_id,
           parent.workspace_id,
           parent.parent_run_id,
           parent.parent_owns_lifecycle,
           parent.session_id,
           0 AS depth
      FROM run_waits AS handoff
      JOIN runs AS parent
        ON parent.environment_id = handoff.environment_id
       AND parent.id = handoff.run_id
       AND parent.workspace_id = handoff.workspace_id
       AND parent.status = 'waiting'
       AND parent.current_run_lease_id IS NULL
     WHERE handoff.child_run_id = $1
       AND handoff.child_parent_owned IS TRUE
       AND handoff.condition_state = 'pending'
       AND handoff.suspension_state = 'parked'
       AND handoff.handoff_runtime_instance_id IS NOT NULL
       AND handoff.handoff_workspace_mount_id IS NOT NULL
       AND handoff.handoff_mount_generation IS NOT NULL
       AND handoff.ownership_generation IS NOT NULL
       AND handoff.parent_writer_generation IS NOT NULL
    UNION
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
		return pgtype.UUID{}, err
	}
	defer rows.Close()
	var waitID pgtype.UUID
	found := false
	actorFound := false
	for rows.Next() {
		var candidateWaitID pgtype.UUID
		var parentID pgtype.UUID
		var parentActorID pgtype.UUID
		if err := rows.Scan(&candidateWaitID, &parentID, &parentActorID); err != nil {
			return pgtype.UUID{}, err
		}
		if !found {
			waitID = candidateWaitID
			found = true
		} else if waitID != candidateWaitID {
			return pgtype.UUID{}, pgx.ErrNoRows
		}
		if parentActorID.Valid {
			if actorFound ||
				!ownerActorID.Valid ||
				parentActorID != ownerActorID ||
				parentID != ownerActorRunID {
				return pgtype.UUID{}, pgx.ErrNoRows
			}
			actorFound = true
		}
	}
	if err := rows.Err(); err != nil {
		return pgtype.UUID{}, err
	}
	if !found {
		return pgtype.UUID{}, nil
	}
	if actorFound != ownerActorID.Valid {
		return pgtype.UUID{}, pgx.ErrNoRows
	}
	return waitID, nil
}

type sameWorkspaceHandoffEdge struct {
	waitID                 pgtype.UUID
	parentRunID            pgtype.UUID
	childRunID             pgtype.UUID
	parentAttempt          int32
	runtimeID              pgtype.UUID
	mountID                pgtype.UUID
	mountGeneration        int64
	ownershipGeneration    int64
	parentWriterGeneration int64
	childWriterGeneration  pgtype.Int8
	checkpointID           pgtype.UUID
	checkpointBaseID       pgtype.UUID
	checkpointPrivateID    pgtype.UUID
	sourceRunLeaseID       pgtype.UUID
	sourceWorkspaceLeaseID pgtype.UUID
	depth                  int
}

func discoverSameWorkspaceHandoffChain(
	ctx context.Context,
	tx pgx.Tx,
	authority runPlacementAuthority,
) ([]sameWorkspaceHandoffEdge, error) {
	rows, err := tx.Query(ctx, `
WITH RECURSIVE edges AS (
    SELECT handoff.id,
           handoff.run_id AS parent_run_id,
           handoff.child_run_id,
           0 AS depth
      FROM run_waits AS handoff
     WHERE handoff.id = $1
       AND handoff.child_run_id = $2
       AND handoff.workspace_id = $3
       AND handoff.child_parent_owned IS TRUE
       AND handoff.condition_state = 'pending'
       AND handoff.suspension_state = 'parked'
    UNION ALL
    SELECT outer_wait.id,
           outer_wait.run_id,
           outer_wait.child_run_id,
           inner_edge.depth + 1
      FROM edges AS inner_edge
      JOIN runs AS inner_parent
        ON inner_parent.id = inner_edge.parent_run_id
       AND inner_parent.workspace_id = $3
       AND inner_parent.parent_owns_lifecycle IS TRUE
      JOIN run_waits AS outer_wait
        ON outer_wait.run_id = inner_parent.parent_run_id
       AND outer_wait.child_run_id = inner_parent.id
       AND outer_wait.workspace_id = $3
       AND outer_wait.child_parent_owned IS TRUE
       AND outer_wait.condition_state = 'pending'
       AND outer_wait.suspension_state = 'parked'
)
SELECT handoff.id,
       handoff.run_id,
       handoff.child_run_id,
       handoff.attempt_number,
       handoff.handoff_runtime_instance_id,
       handoff.handoff_workspace_mount_id,
       handoff.handoff_mount_generation,
       handoff.ownership_generation,
       handoff.parent_writer_generation,
       handoff.child_writer_generation,
       checkpoint.id,
       checkpoint.base_workspace_version_id,
       checkpoint.private_workspace_version_id,
       checkpoint.source_run_lease_id,
       checkpoint.source_workspace_lease_id,
       edges.depth
  FROM edges
  JOIN run_waits AS handoff
    ON handoff.id = edges.id
  JOIN run_checkpoints AS checkpoint
    ON checkpoint.id = handoff.suspend_checkpoint_id
   AND checkpoint.kind = 'suspend'
   AND checkpoint.run_id = handoff.run_id
   AND checkpoint.attempt_number = handoff.attempt_number
   AND checkpoint.run_wait_id = handoff.id
   AND checkpoint.workspace_id = handoff.workspace_id
   AND checkpoint.state = 'ready'
   AND checkpoint.private_workspace_version_id =
       handoff.base_workspace_version_id
   AND (checkpoint.expires_at IS NULL
        OR checkpoint.expires_at > transaction_timestamp())
 ORDER BY edges.depth DESC`,
		authority.handoffChildWaitID,
		authority.runID,
		authority.workspaceID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var edges []sameWorkspaceHandoffEdge
	for rows.Next() {
		var edge sameWorkspaceHandoffEdge
		if err := rows.Scan(
			&edge.waitID,
			&edge.parentRunID,
			&edge.childRunID,
			&edge.parentAttempt,
			&edge.runtimeID,
			&edge.mountID,
			&edge.mountGeneration,
			&edge.ownershipGeneration,
			&edge.parentWriterGeneration,
			&edge.childWriterGeneration,
			&edge.checkpointID,
			&edge.checkpointBaseID,
			&edge.checkpointPrivateID,
			&edge.sourceRunLeaseID,
			&edge.sourceWorkspaceLeaseID,
			&edge.depth,
		); err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(edges) == 0 {
		return nil, pgx.ErrNoRows
	}
	return edges, nil
}

func lockSameWorkspaceHandoffChain(
	ctx context.Context,
	tx pgx.Tx,
	authority runPlacementAuthority,
	runtime runRuntime,
	mount runWorkspaceMount,
) error {
	edges, err := discoverSameWorkspaceHandoffChain(ctx, tx, authority)
	if err != nil {
		return err
	}

	// The source run lease and workspace lease rows are locked before any wait,
	// preserving the canonical physical-authority-before-wait order.
	for _, edge := range edges {
		var state string
		var runtimeID, workspaceID pgtype.UUID
		var attemptNumber int32
		err := tx.QueryRow(ctx, `
SELECT state, runtime_instance_id, workspace_id, attempt_number
  FROM run_leases
 WHERE id = $1
   AND run_id = $2
   AND attempt_number = $3
   AND workspace_id = $4
 FOR UPDATE`,
			edge.sourceRunLeaseID,
			edge.parentRunID,
			edge.parentAttempt,
			authority.workspaceID,
		).Scan(&state, &runtimeID, &workspaceID, &attemptNumber)
		if err != nil {
			return err
		}
		if state != "checkpointed" ||
			runtimeID != runtime.id ||
			workspaceID != authority.workspaceID ||
			attemptNumber != edge.parentAttempt ||
			!edge.sourceRunLeaseID.Valid {
			return pgx.ErrNoRows
		}
	}
	for _, edge := range edges {
		var state string
		var ownerRunLeaseID, runtimeID, workspaceID, mountID, baseID pgtype.UUID
		var ownership, writer, mountGeneration int64
		err := tx.QueryRow(ctx, `
SELECT state,
       owner_run_lease_id,
       runtime_instance_id,
       workspace_id,
       workspace_mount_id,
       base_version_id,
       ownership_generation,
       writer_generation,
       mount_fencing_generation
  FROM workspace_leases
 WHERE id = $1
   AND workspace_id = $2
   AND owner_run_lease_id = $3
 FOR UPDATE`,
			edge.sourceWorkspaceLeaseID,
			authority.workspaceID,
			edge.sourceRunLeaseID,
		).Scan(
			&state,
			&ownerRunLeaseID,
			&runtimeID,
			&workspaceID,
			&mountID,
			&baseID,
			&ownership,
			&writer,
			&mountGeneration,
		)
		if err != nil {
			return err
		}
		if (state != "released" && state != "fenced") ||
			ownerRunLeaseID != edge.sourceRunLeaseID ||
			runtimeID != runtime.id ||
			workspaceID != authority.workspaceID ||
			mountID != mount.id ||
			baseID != edge.checkpointBaseID ||
			ownership != edge.ownershipGeneration ||
			writer != edge.parentWriterGeneration ||
			mountGeneration != edge.mountGeneration {
			return pgx.ErrNoRows
		}
	}
	if authority.handoffChildWriter.Valid {
		var priorRunLeaseID, priorWorkspaceLeaseID pgtype.UUID
		err := tx.QueryRow(ctx, `
SELECT child_lease.id, child_workspace_lease.id
  FROM run_leases AS child_lease
  JOIN workspace_leases AS child_workspace_lease
    ON child_workspace_lease.owner_run_lease_id = child_lease.id
   AND child_workspace_lease.workspace_id = child_lease.workspace_id
   AND child_workspace_lease.runtime_instance_id = $3
   AND child_workspace_lease.workspace_mount_id = $4
   AND child_workspace_lease.base_version_id = $5
   AND child_workspace_lease.ownership_generation = $6
   AND child_workspace_lease.writer_generation = $7
   AND child_workspace_lease.mount_fencing_generation = $8
   AND child_workspace_lease.state IN ('released', 'fenced')
 WHERE child_lease.run_id = $1
   AND child_lease.workspace_id = $2
   AND child_lease.state IN ('failed', 'expired', 'lost', 'rejected')
 ORDER BY child_lease.lease_sequence DESC
 LIMIT 1
 FOR UPDATE OF child_lease, child_workspace_lease`,
			authority.runID,
			authority.workspaceID,
			runtime.id,
			mount.id,
			authority.baseVersionID,
			authority.ownershipGeneration,
			authority.handoffChildWriter.Int64,
			authority.handoffAdmissionMountGen.Int64,
		).Scan(&priorRunLeaseID, &priorWorkspaceLeaseID)
		if err != nil {
			return err
		}
	}

	var priorChildWriter pgtype.Int8
	for index, edge := range edges {
		var parentRunID, childRunID, runtimeID, mountID, priorRunLeaseID pgtype.UUID
		var mountGeneration, ownership, parentWriter int64
		var childWriter pgtype.Int8
		var ownerMatch bool
		err := tx.QueryRow(ctx, `
SELECT handoff.run_id,
       handoff.child_run_id,
       handoff.handoff_runtime_instance_id,
       handoff.handoff_workspace_mount_id,
       handoff.handoff_mount_generation,
       handoff.ownership_generation,
       handoff.parent_writer_generation,
       handoff.child_writer_generation,
       handoff.prior_run_lease_id,
       (
           (workspaces.owner_run_id = parent.id
            AND workspaces.owner_session_id IS NULL)
           OR
           (workspaces.owner_session_id = parent.session_id
            AND workspaces.owner_run_id IS NULL
            AND parent.session_id IS NOT NULL)
       )
  FROM run_waits AS handoff
  JOIN runs AS parent
    ON parent.id = handoff.run_id
   AND parent.workspace_id = handoff.workspace_id
   AND parent.status = 'waiting'
   AND parent.current_run_lease_id IS NULL
  JOIN workspaces
    ON workspaces.id = handoff.workspace_id
 WHERE handoff.id = $1
   AND handoff.workspace_id = $2
   AND handoff.child_parent_owned IS TRUE
   AND handoff.condition_state = 'pending'
   AND handoff.suspension_state = 'parked'
 FOR UPDATE OF handoff`,
			edge.waitID,
			authority.workspaceID,
		).Scan(
			&parentRunID,
			&childRunID,
			&runtimeID,
			&mountID,
			&mountGeneration,
			&ownership,
			&parentWriter,
			&childWriter,
			&priorRunLeaseID,
			&ownerMatch,
		)
		if err != nil {
			return err
		}
		if parentRunID != edge.parentRunID ||
			childRunID != edge.childRunID ||
			runtimeID != runtime.id ||
			mountID != mount.id ||
			mountGeneration != edge.mountGeneration ||
			ownership != authority.ownershipGeneration ||
			parentWriter != edge.parentWriterGeneration ||
			childWriter != edge.childWriterGeneration ||
			priorRunLeaseID != edge.sourceRunLeaseID {
			return pgx.ErrNoRows
		}
		if index == 0 {
			if !ownerMatch {
				return pgx.ErrNoRows
			}
		} else if !priorChildWriter.Valid ||
			priorChildWriter.Int64 != parentWriter {
			return pgx.ErrNoRows
		}
		if index == len(edges)-1 {
			if edge.waitID != authority.handoffChildWaitID ||
				edge.childRunID != authority.runID ||
				childWriter != authority.handoffChildWriter ||
				parentWriter != authority.handoffParentWriter.Int64 {
				return pgx.ErrNoRows
			}
		} else if !childWriter.Valid {
			return pgx.ErrNoRows
		}
		priorChildWriter = childWriter
	}

	for _, edge := range edges {
		var sourceRunLeaseID, sourceWorkspaceLeaseID, baseID, privateID pgtype.UUID
		err := tx.QueryRow(ctx, `
SELECT source_run_lease_id,
       source_workspace_lease_id,
       base_workspace_version_id,
       private_workspace_version_id
  FROM run_checkpoints
 WHERE id = $1
   AND kind = 'suspend'
   AND run_id = $2
   AND attempt_number = $3
   AND run_wait_id = $4
   AND workspace_id = $5
   AND state = 'ready'
   AND (expires_at IS NULL OR expires_at > transaction_timestamp())
 FOR UPDATE`,
			edge.checkpointID,
			edge.parentRunID,
			edge.parentAttempt,
			edge.waitID,
			authority.workspaceID,
		).Scan(
			&sourceRunLeaseID,
			&sourceWorkspaceLeaseID,
			&baseID,
			&privateID,
		)
		if err != nil {
			return err
		}
		if sourceRunLeaseID != edge.sourceRunLeaseID ||
			sourceWorkspaceLeaseID != edge.sourceWorkspaceLeaseID ||
			baseID != edge.checkpointBaseID ||
			privateID != edge.checkpointPrivateID {
			return pgx.ErrNoRows
		}
	}
	return nil
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
