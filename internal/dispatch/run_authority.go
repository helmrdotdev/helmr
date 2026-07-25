package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const mebibyte = int64(1024 * 1024)

type runPlacementAuthority struct {
	entrypointKind           string
	actorID                  pgtype.UUID
	runID                    pgtype.UUID
	orgID                    pgtype.UUID
	projectID                pgtype.UUID
	environmentID            pgtype.UUID
	deploymentID             pgtype.UUID
	workspaceDefinitionID    pgtype.UUID
	workspaceID              pgtype.UUID
	baseVersionID            pgtype.UUID
	restoreCheckpointID      pgtype.UUID
	resumeRunWaitID          pgtype.UUID
	resumeRequestVersion     int64
	restoreRuntimeID         pgtype.UUID
	restoreRuntimeIdentityID string
	restoreSubstrateID       pgtype.UUID
	restoreSubstrateFormat   string
	restoreSubstrateBuilder  string
	restoreSubstrateLayout   string
	attemptNumber            int32
	stateVersion             int64
	regionID                 string
	queueName                string
	concurrencyKey           pgtype.Text
	queueLimit               pgtype.Int8
	ownershipGeneration      int64
	writerGeneration         int64
	traceID                  pgtype.Text
	rootSpanID               string
	resources                runResources
	networkPolicy            []byte
	architecture             string
}

type runResources struct {
	cpuMillis      int64
	memoryBytes    int64
	workloadDisk   int64
	scratchBytes   int64
	executionSlots int32
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
	if err := tx.QueryRow(ctx, `
SELECT entrypoint_kind, actor_id
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
	if authority.entrypointKind == "actor" {
		if !authority.actorID.Valid {
			return runPlacementAuthority{}, pgx.ErrNoRows
		}
		err := tx.QueryRow(ctx, `
SELECT run_generation, committed_input_sequence, next_input_sequence
  FROM actors
 WHERE id = $1
   AND current_run_id = $2
   AND state IN ('open', 'closing')
 FOR UPDATE`, authority.actorID, candidate.RunID).Scan(
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
	} else if authority.entrypointKind != "task" || authority.actorID.Valid {
		return runPlacementAuthority{}, pgx.ErrNoRows
	}
	err := tx.QueryRow(ctx, `
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
       restore_wait.id,
       coalesce(restore_wait.resume_request_version, 0),
       restore_wait.suspend_checkpoint_id,
       restore_checkpoint.private_workspace_version_id,
       runs.actor_start_input_sequence,
       runs.actor_start_input_high_watermark
  FROM runs
  LEFT JOIN LATERAL (
       SELECT run_waits.id,
              run_waits.resume_request_version,
              run_waits.suspend_checkpoint_id,
              run_waits.attempt_number,
              run_waits.workspace_id
         FROM run_waits
        WHERE run_waits.run_id = runs.id
          AND run_waits.suspension_state = 'resume_pending'
          AND run_waits.handoff_runtime_instance_id IS NULL
          AND run_waits.handoff_workspace_mount_id IS NULL
          AND run_waits.handoff_resume_checkpoint_id IS NULL
  ) AS restore_wait ON true
  LEFT JOIN run_checkpoints AS restore_checkpoint
    ON restore_checkpoint.id = restore_wait.suspend_checkpoint_id
   AND restore_checkpoint.kind = 'suspend'
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
   AND (($4 = 'task' AND runs.actor_id IS NULL
         AND runs.cause_kind IN ('api', 'manual', 'schedule', 'child')
         AND (runs.parent_run_id IS NULL OR runs.parent_owns_lifecycle IS FALSE))
        OR ($4 = 'actor' AND runs.actor_id = $5
            AND runs.cause_kind IN ('actor_start', 'continuation')
            AND runs.parent_run_id IS NULL))
   AND runs.status = 'queued'
   AND runs.current_run_lease_id IS NULL
   AND (
       ($4 = 'task' AND
           restore_wait.id IS NULL
           AND NOT EXISTS (
               SELECT 1
                 FROM run_waits
                WHERE run_waits.run_id = runs.id
                  AND run_waits.suspension_state IN (
                      'hot', 'checkpointing', 'parked', 'resume_pending', 'resuming'
                  )
           ))
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
		&authority.resumeRunWaitID,
		&authority.resumeRequestVersion,
		&authority.restoreCheckpointID,
		&authority.baseVersionID,
		&actorStartInputSequence,
		&actorStartInputHighWatermark,
	)
	if err != nil {
		return runPlacementAuthority{}, err
	}
	var manifest []byte
	var workspaceArchitecture pgtype.Text
	workspaceOwnerPredicate := "workspaces.owner_run_id = $5 AND workspaces.owner_actor_id IS NULL"
	workspaceOwnerID := authority.runID
	if authority.entrypointKind == "actor" {
		workspaceOwnerPredicate = "workspaces.owner_actor_id = $5 AND workspaces.owner_run_id IS NULL"
		workspaceOwnerID = authority.actorID
	}
	err = tx.QueryRow(ctx, fmt.Sprintf(`
SELECT workspaces.deployment_definition_id,
       workspaces.region_id,
       workspaces.ownership_generation,
       workspaces.writer_generation,
       workspace_definitions.manifest,
       workspace_definitions.workspace_architecture
  FROM workspaces
  JOIN deployment_definitions AS workspace_definitions
    ON workspace_definitions.environment_id = workspaces.environment_id
   AND workspace_definitions.id = workspaces.deployment_definition_id
   AND workspace_definitions.kind = 'workspace'
   AND workspace_definitions.declared_id = workspaces.workspace_declared_id
 WHERE workspaces.org_id = $1
   AND workspaces.project_id = $2
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
		&workspaceArchitecture,
	)
	if err != nil {
		return runPlacementAuthority{}, err
	}

	var attemptBaseVersionID pgtype.UUID
	var attemptActorStartInputSequence pgtype.Int8
	err = tx.QueryRow(ctx, `
SELECT run_attempts.base_workspace_version_id,
       run_attempts.actor_start_input_sequence
  FROM run_attempts
  JOIN workspace_versions
    ON workspace_versions.workspace_id = run_attempts.workspace_id
   AND workspace_versions.id = run_attempts.base_workspace_version_id
   AND workspace_versions.state = 'committed'
 WHERE run_attempts.run_id = $1
   AND run_attempts.number = $2
   AND run_attempts.entrypoint_kind = $4
   AND run_attempts.workspace_id = $3
 FOR UPDATE OF run_attempts`,
		authority.runID,
		authority.attemptNumber,
		authority.workspaceID,
		authority.entrypointKind,
	).Scan(&attemptBaseVersionID, &attemptActorStartInputSequence)
	if err != nil {
		return runPlacementAuthority{}, err
	}
	if !authority.baseVersionID.Valid {
		authority.baseVersionID = attemptBaseVersionID
	}
	if authority.entrypointKind == "actor" &&
		(!actorStartInputSequence.Valid || !attemptActorStartInputSequence.Valid ||
			attemptActorStartInputSequence.Int64 != actorStartInputSequence.Int64 ||
			!actorStartInputHighWatermark.Valid ||
			actorStartInputSequence.Int64 > actorStartInputHighWatermark.Int64 ||
			actorCommittedInputSequence < actorStartInputSequence.Int64 ||
			actorCommittedInputSequence >= actorNextInputSequence) {
		return runPlacementAuthority{}, pgx.ErrNoRows
	}
	if authority.restoreCheckpointID.Valid {
		err = tx.QueryRow(ctx, `
SELECT source_runtime.id,
       source_runtime.runtime_identity_id,
       source_runtime.runtime_substrate_id,
       runtime_substrates.substrate_format,
       runtime_substrates.builder_abi,
       runtime_substrates.layout_abi
  FROM run_waits
  JOIN run_checkpoints
    ON run_checkpoints.id = run_waits.suspend_checkpoint_id
   AND run_checkpoints.kind = 'suspend'
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
   AND source_runtime.reserved_workload_disk_bytes = source_lease.requested_workload_disk_bytes
   AND source_runtime.reserved_scratch_bytes = source_lease.requested_scratch_bytes
   AND source_runtime.reserved_execution_slots = source_lease.requested_execution_slots
  JOIN runtime_substrates
    ON runtime_substrates.id = source_runtime.runtime_substrate_id
   AND runtime_substrates.org_id = source_runtime.org_id
   AND runtime_substrates.project_id = source_runtime.project_id
   AND runtime_substrates.environment_id = source_runtime.environment_id
   AND runtime_substrates.deployment_definition_id = source_runtime.deployment_definition_id
   AND runtime_substrates.retired_at IS NULL
 WHERE run_waits.id = $1
   AND run_waits.run_id = $2
   AND run_waits.attempt_number = $3
   AND run_waits.workspace_id = $4
   AND run_waits.suspension_state = 'resume_pending'
   AND run_waits.resume_request_version = $6
   AND run_checkpoints.id = $7
   AND run_waits.handoff_runtime_instance_id IS NULL
   AND run_waits.handoff_workspace_mount_id IS NULL
   AND run_waits.handoff_resume_checkpoint_id IS NULL
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
		).Scan(
			&authority.restoreRuntimeID,
			&authority.restoreRuntimeIdentityID,
			&authority.restoreSubstrateID,
			&authority.restoreSubstrateFormat,
			&authority.restoreSubstrateBuilder,
			&authority.restoreSubstrateLayout,
		)
		if err != nil {
			return runPlacementAuthority{}, err
		}
	}

	var programArchitecture pgtype.Text
	err = tx.QueryRow(ctx, `
SELECT deployments.program_architecture
  FROM deployments
  JOIN deployment_definitions AS entrypoint_definitions
    ON entrypoint_definitions.environment_id = deployments.environment_id
   AND entrypoint_definitions.deployment_id = deployments.id
   AND entrypoint_definitions.id = $3
   AND entrypoint_definitions.kind = $4
 WHERE deployments.environment_id = $1
   AND deployments.id = $2
   AND deployments.status = 'deployed'
   AND deployments.program_artifact_id IS NOT NULL
   AND deployments.program_runtime_digest IS NOT NULL
   AND deployments.program_architecture IS NOT NULL`,
		authority.environmentID,
		authority.deploymentID,
		entrypointDefinitionID,
		authority.entrypointKind,
	).Scan(&programArchitecture)
	if err != nil {
		return runPlacementAuthority{}, err
	}
	if !workspaceArchitecture.Valid ||
		!programArchitecture.Valid ||
		workspaceArchitecture.String != programArchitecture.String {
		return runPlacementAuthority{}, errors.New(
			"Run Program and Workspace architectures do not match",
		)
	}
	var workspaceManifest deployment.WorkspaceManifest
	decoder := json.NewDecoder(bytes.NewReader(manifest))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&workspaceManifest); err != nil {
		return runPlacementAuthority{}, fmt.Errorf("decode Workspace manifest: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return runPlacementAuthority{}, fmt.Errorf("decode Workspace manifest: %w", err)
	}
	resources, err := normalizeRunResources(workspaceManifest.Resources)
	if err != nil {
		return runPlacementAuthority{}, err
	}
	network, err := json.Marshal(workspaceManifest.Network)
	if err != nil {
		return runPlacementAuthority{}, fmt.Errorf("encode Workspace network policy: %w", err)
	}
	network, err = jsoncanon.Transform(network)
	if err != nil {
		return runPlacementAuthority{}, fmt.Errorf("canonicalize Workspace network policy: %w", err)
	}
	authority.resources = resources
	authority.networkPolicy = network
	authority.architecture = workspaceArchitecture.String
	return authority, nil
}

func lockRunRestoreSecrets(
	ctx context.Context,
	tx pgx.Tx,
	candidate ReadyRunCandidate,
) error {
	secretRows, err := tx.Query(ctx, `
SELECT secrets.state = 'active'
       AND secret_resolutions.id IS NOT NULL
       AND secret_resolutions.revocation_generation = secrets.revocation_generation
  FROM runs
  JOIN run_waits
    ON run_waits.run_id = runs.id
   AND run_waits.suspension_state = 'resume_pending'
   AND run_waits.handoff_runtime_instance_id IS NULL
   AND run_waits.handoff_workspace_mount_id IS NULL
   AND run_waits.handoff_resume_checkpoint_id IS NULL
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
			return errors.New("Run restore Secret resolution is revoked or incomplete")
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
		resources.DiskMiB <= 0 ||
		resources.MemoryMiB > math.MaxInt64/mebibyte ||
		resources.DiskMiB > math.MaxInt64/mebibyte {
		return runResources{}, errors.New("Workspace resources are outside the Run placement domain")
	}
	return runResources{
		cpuMillis:      resources.MilliCPU,
		memoryBytes:    resources.MemoryMiB * mebibyte,
		workloadDisk:   resources.DiskMiB * mebibyte,
		scratchBytes:   0,
		executionSlots: 1,
	}, nil
}

func lockRunQueueScope(
	ctx context.Context,
	tx pgx.Tx,
	candidate ReadyRunCandidate,
) error {
	environmentID, queueName, concurrencyKey, err := discoverRunQueueScope(
		ctx,
		tx,
		candidate,
	)
	if err != nil {
		return err
	}
	key, err := queueScopeLockKey(environmentID, queueName, concurrencyKey)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", key); err != nil {
		return fmt.Errorf("lock Run queue scope: %w", err)
	}
	return nil
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
