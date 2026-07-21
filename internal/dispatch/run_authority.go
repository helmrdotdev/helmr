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

type freshRunAuthority struct {
	runID                 pgtype.UUID
	orgID                 pgtype.UUID
	projectID             pgtype.UUID
	environmentID         pgtype.UUID
	deploymentID          pgtype.UUID
	workspaceDefinitionID pgtype.UUID
	workspaceID           pgtype.UUID
	baseVersionID         pgtype.UUID
	attemptNumber         int32
	stateVersion          int64
	regionID              string
	queueName             string
	concurrencyKey        pgtype.Text
	queueLimit            pgtype.Int8
	ownershipGeneration   int64
	writerGeneration      int64
	traceID               pgtype.Text
	rootSpanID            string
	resources             runResources
	networkPolicy         []byte
	architecture          string
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

func lockFreshRunAuthority(
	ctx context.Context,
	tx pgx.Tx,
	candidate ReadyRunCandidate,
) (freshRunAuthority, error) {
	var authority freshRunAuthority
	var entrypointDefinitionID pgtype.UUID
	err := tx.QueryRow(ctx, `
SELECT id,
       org_id,
       project_id,
       environment_id,
       deployment_id,
       deployment_definition_id,
       workspace_id,
       current_attempt_number,
       state_version,
       queue_name,
       concurrency_key,
       queue_concurrency_limit,
       trace_id,
       root_span_id
  FROM runs
 WHERE org_id = $1
   AND id = $2
   AND state_version = $3
   AND entrypoint_kind = 'task'
   AND cause_kind IN ('api', 'manual', 'schedule')
   AND status = 'queued'
   AND current_run_lease_id IS NULL
   AND NOT EXISTS (
       SELECT 1
         FROM run_waits
        WHERE run_waits.run_id = runs.id
          AND run_waits.suspension_state IN (
              'hot', 'checkpointing', 'parked', 'resume_pending', 'resuming'
          )
   )
   AND (
       first_lease_at IS NOT NULL
       OR queued_expires_at IS NULL
       OR queued_expires_at > transaction_timestamp()
   )
 FOR UPDATE`,
		candidate.OrgID,
		candidate.RunID,
		candidate.ExpectedRunStateVersion,
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
	)
	if err != nil {
		return freshRunAuthority{}, err
	}

	var manifest []byte
	var workspaceArchitecture pgtype.Text
	err = tx.QueryRow(ctx, `
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
   AND workspaces.owner_run_id = $5
   AND workspaces.owner_actor_id IS NULL
 FOR UPDATE OF workspaces`,
		authority.orgID,
		authority.projectID,
		authority.environmentID,
		authority.workspaceID,
		authority.runID,
	).Scan(
		&authority.workspaceDefinitionID,
		&authority.regionID,
		&authority.ownershipGeneration,
		&authority.writerGeneration,
		&manifest,
		&workspaceArchitecture,
	)
	if err != nil {
		return freshRunAuthority{}, err
	}

	err = tx.QueryRow(ctx, `
SELECT run_attempts.base_workspace_version_id
  FROM run_attempts
  JOIN workspace_versions
    ON workspace_versions.workspace_id = run_attempts.workspace_id
   AND workspace_versions.id = run_attempts.base_workspace_version_id
   AND workspace_versions.state = 'committed'
 WHERE run_attempts.run_id = $1
   AND run_attempts.number = $2
   AND run_attempts.entrypoint_kind = 'task'
   AND run_attempts.workspace_id = $3
 FOR UPDATE OF run_attempts`,
		authority.runID,
		authority.attemptNumber,
		authority.workspaceID,
	).Scan(&authority.baseVersionID)
	if err != nil {
		return freshRunAuthority{}, err
	}

	var programArchitecture pgtype.Text
	err = tx.QueryRow(ctx, `
SELECT deployments.program_architecture
  FROM deployments
  JOIN deployment_definitions AS entrypoint_definitions
    ON entrypoint_definitions.environment_id = deployments.environment_id
   AND entrypoint_definitions.deployment_id = deployments.id
   AND entrypoint_definitions.id = $3
   AND entrypoint_definitions.kind = 'task'
 WHERE deployments.environment_id = $1
   AND deployments.id = $2
   AND deployments.status = 'deployed'
   AND deployments.program_code_artifact_id IS NOT NULL
   AND deployments.program_dependency_artifact_id IS NOT NULL
   AND deployments.program_runtime_digest IS NOT NULL
   AND deployments.program_architecture IS NOT NULL`,
		authority.environmentID,
		authority.deploymentID,
		entrypointDefinitionID,
	).Scan(&programArchitecture)
	if err != nil {
		return freshRunAuthority{}, err
	}
	if !workspaceArchitecture.Valid ||
		!programArchitecture.Valid ||
		workspaceArchitecture.String != programArchitecture.String {
		return freshRunAuthority{}, errors.New(
			"Run Program and Workspace architectures do not match",
		)
	}
	var workspaceManifest deployment.WorkspaceManifest
	decoder := json.NewDecoder(bytes.NewReader(manifest))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&workspaceManifest); err != nil {
		return freshRunAuthority{}, fmt.Errorf("decode Workspace manifest: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return freshRunAuthority{}, fmt.Errorf("decode Workspace manifest: %w", err)
	}
	resources, err := normalizeRunResources(workspaceManifest.Resources)
	if err != nil {
		return freshRunAuthority{}, err
	}
	network, err := json.Marshal(workspaceManifest.Network)
	if err != nil {
		return freshRunAuthority{}, fmt.Errorf("encode Workspace network policy: %w", err)
	}
	network, err = jsoncanon.Transform(network)
	if err != nil {
		return freshRunAuthority{}, fmt.Errorf("canonicalize Workspace network policy: %w", err)
	}
	authority.resources = resources
	authority.networkPolicy = network
	authority.architecture = workspaceArchitecture.String
	return authority, nil
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
