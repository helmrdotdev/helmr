package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const workspaceExecLeaseTTL = 30 * time.Minute

type workspaceExecPermanentError struct {
	code string
	err  error
}

func (e workspaceExecPermanentError) Error() string {
	if e.err == nil {
		return e.code
	}
	return e.err.Error()
}

func (e workspaceExecPermanentError) Unwrap() error {
	return e.err
}

type ReadyWorkspaceExecCandidate struct {
	OrgID                pgtype.UUID
	ProcessID            pgtype.UUID
	ExpectedStateVersion int64
}

type WorkspaceExecPlacement struct {
	WorkspaceMountID  pgtype.UUID
	WorkerInstanceID  pgtype.UUID
	WorkerEpoch       int64
	RuntimeInstanceID pgtype.UUID
	ProcessBound      bool
}

type workspaceExecAuthority struct {
	processID             pgtype.UUID
	processStateVersion   int64
	orgID                 pgtype.UUID
	projectID             pgtype.UUID
	environmentID         pgtype.UUID
	workspaceID           pgtype.UUID
	workspaceDefinitionID pgtype.UUID
	baseVersionID         pgtype.UUID
	regionID              string
	ownershipGeneration   int64
	writerGeneration      int64
	resources             runResources
	networkPolicy         []byte
	architecture          string
}

func (a workspaceExecAuthority) runAuthority() runPlacementAuthority {
	return runPlacementAuthority{
		orgID:                 a.orgID,
		projectID:             a.projectID,
		environmentID:         a.environmentID,
		workspaceDefinitionID: a.workspaceDefinitionID,
		workspaceID:           a.workspaceID,
		baseVersionID:         a.baseVersionID,
		regionID:              a.regionID,
		resources:             a.resources,
		networkPolicy:         a.networkPolicy,
		architecture:          a.architecture,
	}
}

func (d *Authority) PlaceWorkspaceExec(
	ctx context.Context,
	candidate ReadyWorkspaceExecCandidate,
	observationFreshAfter pgtype.Timestamptz,
) (WorkspaceExecPlacement, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return WorkspaceExecPlacement{}, fmt.Errorf("begin Workspace exec placement: %w", err)
	}
	defer rollback(ctx, tx)

	if err := lockWorkspaceExecSecrets(ctx, tx, candidate); err != nil {
		return WorkspaceExecPlacement{}, d.finishRejectedWorkspaceExec(ctx, tx, candidate, err)
	}
	authority, err := lockWorkspaceExecAuthority(ctx, tx, candidate)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = workspaceExecPermanentError{
				code: "workspace_exec_authority_changed",
				err:  errors.New("Workspace exec authority changed after admission"),
			}
		}
		return WorkspaceExecPlacement{}, d.finishRejectedWorkspaceExec(ctx, tx, candidate, err)
	}
	runtime, err := discoverRunRuntime(ctx, tx, authority.workspaceID)
	if errors.Is(err, pgx.ErrNoRows) {
		placement, err := d.createWorkspaceExecRuntime(ctx, tx, authority, observationFreshAfter)
		if err != nil {
			return WorkspaceExecPlacement{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return WorkspaceExecPlacement{}, fmt.Errorf("commit Workspace exec runtime reservation: %w", err)
		}
		return placement, nil
	}
	if err != nil {
		return WorkspaceExecPlacement{}, fmt.Errorf("discover Workspace exec runtime: %w", err)
	}
	if err := lockWorkerFence(ctx, tx, workerFence{
		GroupID:               runtime.groupID,
		RegionID:              authority.regionID,
		WorkerInstanceID:      runtime.workerID,
		WorkerEpoch:           runtime.workerEpoch,
		WorkerProtocolVersion: runtime.protocolVersion,
		ObservationFreshAfter: observationFreshAfter,
		Role:                  "run",
		RunArchitecture:       authority.architecture,
	}); err != nil {
		return WorkspaceExecPlacement{}, ErrCapacityUnavailable
	}
	runtime, err = lockRunRuntime(ctx, tx, runtime)
	if err != nil {
		return WorkspaceExecPlacement{}, err
	}
	if runtime.reservedProcessID == authority.processID && !runtime.reservationActive {
		closed, err := db.New(tx).CloseExpiredWorkspaceExecReservation(
			ctx,
			db.CloseExpiredWorkspaceExecReservationParams{
				RuntimeInstanceID: runtime.id,
				WorkspaceID:       authority.workspaceID,
				ProcessID:         authority.processID,
			},
		)
		if err != nil {
			return WorkspaceExecPlacement{}, fmt.Errorf("close expired Workspace exec reservation: %w", err)
		}
		if closed != 1 {
			return WorkspaceExecPlacement{}, ErrCapacityUnavailable
		}
		if err := tx.Commit(ctx); err != nil {
			return WorkspaceExecPlacement{}, fmt.Errorf("commit expired Workspace exec reservation close: %w", err)
		}
		return WorkspaceExecPlacement{
			WorkerInstanceID:  runtime.workerID,
			WorkerEpoch:       runtime.workerEpoch,
			RuntimeInstanceID: runtime.id,
		}, nil
	}
	if err := validateWorkspaceExecRuntime(authority, runtime); err != nil {
		return WorkspaceExecPlacement{}, ErrCapacityUnavailable
	}
	if !runtime.reservedProcessID.Valid {
		if runtime.observedState != db.RuntimeObservedStateReady ||
			runtime.networkSlotState != db.WorkerNetworkSlotStateBound {
			return WorkspaceExecPlacement{
				WorkerInstanceID:  runtime.workerID,
				WorkerEpoch:       runtime.workerEpoch,
				RuntimeInstanceID: runtime.id,
			}, nil
		}
		reservation, err := db.New(tx).ReserveReadyRuntimeForWorkspaceExec(
			ctx,
			db.ReserveReadyRuntimeForWorkspaceExecParams{
				ProcessID:              candidate.ProcessID,
				BaseWorkspaceVersionID: authority.baseVersionID,
				ReservationExpiresAt:   pgvalue.Timestamptz(time.Now().Add(d.runPolicy.ReservationTTL)),
				ID:                     runtime.id,
				WorkspaceID:            authority.workspaceID,
				DeploymentDefinitionID: authority.workspaceDefinitionID,
			},
		)
		if err != nil {
			if isConstraintConflict(err) || errors.Is(err, pgx.ErrNoRows) {
				return WorkspaceExecPlacement{}, ErrCapacityUnavailable
			}
			return WorkspaceExecPlacement{}, fmt.Errorf("reserve ready Workspace exec runtime: %w", err)
		}
		runtime.reservedProcessID = reservation.ReservedProcessID
		runtime.reservedVersionID = reservation.ReservedWorkspaceVersionID
		runtime.reservationExpiresAt = reservation.ReservationExpiresAt
		runtime.reservationActive = true
	}

	mount, err := getWorkspaceExecMount(ctx, tx, authority, runtime)
	if errors.Is(err, pgx.ErrNoRows) {
		if runtime.observedState != db.RuntimeObservedStateReady ||
			runtime.networkSlotState != db.WorkerNetworkSlotStateBound {
			return WorkspaceExecPlacement{
				WorkerInstanceID:  runtime.workerID,
				WorkerEpoch:       runtime.workerEpoch,
				RuntimeInstanceID: runtime.id,
			}, nil
		}
		requested, err := db.New(tx).EnsureProcessWorkspaceMountRequested(
			ctx,
			db.EnsureProcessWorkspaceMountRequestedParams{
				ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
				Request:            []byte(`{"kind":"workspace_exec"}`),
				OrgID:              authority.orgID,
				WorkspaceID:        authority.workspaceID,
				RuntimeInstanceID:  runtime.id,
				ProcessID:          authority.processID,
				WorkspaceVersionID: authority.baseVersionID,
			},
		)
		if err != nil {
			return WorkspaceExecPlacement{}, fmt.Errorf("request Workspace exec mount: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return WorkspaceExecPlacement{}, fmt.Errorf("commit Workspace exec mount request: %w", err)
		}
		return WorkspaceExecPlacement{
			WorkspaceMountID:  requested.ID,
			WorkerInstanceID:  requested.WorkerInstanceID,
			WorkerEpoch:       requested.WorkerEpoch,
			RuntimeInstanceID: requested.RuntimeInstanceID,
		}, nil
	}
	if err != nil {
		return WorkspaceExecPlacement{}, fmt.Errorf("read Workspace exec mount: %w", err)
	}
	placement := WorkspaceExecPlacement{
		WorkspaceMountID:  mount.id,
		WorkerInstanceID:  mount.workerID,
		WorkerEpoch:       mount.epoch,
		RuntimeInstanceID: mount.runtimeID,
	}
	if mount.state != db.WorkspaceMountStateMounted {
		if err := tx.Commit(ctx); err != nil {
			return WorkspaceExecPlacement{}, fmt.Errorf("commit Workspace exec placement observation: %w", err)
		}
		return placement, nil
	}
	if err := d.grantWorkspaceExec(ctx, tx, authority, runtime, mount); err != nil {
		return WorkspaceExecPlacement{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return WorkspaceExecPlacement{}, fmt.Errorf("commit Workspace exec grant: %w", err)
	}
	placement.ProcessBound = true
	return placement, nil
}

func lockWorkspaceExecSecrets(
	ctx context.Context,
	tx pgx.Tx,
	candidate ReadyWorkspaceExecCandidate,
) error {
	rows, err := tx.Query(ctx, `
SELECT secrets.state = 'active'
       AND secret_resolutions.id IS NOT NULL
       AND secret_resolutions.revocation_generation = secrets.revocation_generation
  FROM workspace_processes
  JOIN workspace_secrets
    ON workspace_secrets.workspace_id = workspace_processes.workspace_id
  JOIN secrets
    ON secrets.id = workspace_secrets.secret_id
  LEFT JOIN secret_resolutions
    ON secret_resolutions.workspace_id = workspace_secrets.workspace_id
   AND secret_resolutions.process_id = workspace_processes.id
   AND secret_resolutions.placement_kind = workspace_secrets.placement_kind
   AND secret_resolutions.placement_target = workspace_secrets.placement_target
   AND secret_resolutions.secret_id = workspace_secrets.secret_id
 WHERE workspace_processes.org_id = $1
   AND workspace_processes.id = $2
   AND workspace_processes.state_version = $3
   AND workspace_processes.state = 'pending'
 ORDER BY secrets.id, workspace_secrets.placement_kind, workspace_secrets.placement_target
 FOR UPDATE OF secrets`,
		candidate.OrgID,
		candidate.ProcessID,
		candidate.ExpectedStateVersion,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var valid bool
		if err := rows.Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return workspaceExecPermanentError{
				code: "workspace_exec_secret_unavailable",
				err:  errors.New("Workspace exec Secret resolution is revoked or incomplete"),
			}
		}
	}
	return rows.Err()
}

func lockWorkspaceExecAuthority(
	ctx context.Context,
	tx pgx.Tx,
	candidate ReadyWorkspaceExecCandidate,
) (workspaceExecAuthority, error) {
	var authority workspaceExecAuthority
	var manifest []byte
	err := tx.QueryRow(ctx, `
SELECT workspace_processes.id,
       workspace_processes.state_version,
       workspace_processes.org_id,
       workspace_processes.project_id,
       workspace_processes.environment_id,
       workspace_processes.workspace_id,
       workspace_processes.base_version_id,
       workspaces.deployment_definition_id,
       workspaces.region_id,
       workspaces.ownership_generation,
       workspaces.writer_generation,
       definitions.manifest
  FROM workspace_processes
  JOIN workspaces
    ON workspaces.environment_id = workspace_processes.environment_id
   AND workspaces.id = workspace_processes.workspace_id
  JOIN environments
    ON environments.id = workspaces.environment_id
   AND environments.org_id = workspace_processes.org_id
   AND environments.project_id = workspace_processes.project_id
  JOIN deployment_definitions AS definitions
    ON definitions.environment_id = workspaces.environment_id
   AND definitions.id = workspaces.deployment_definition_id
   AND definitions.kind = 'workspace'
   AND definitions.declared_id = workspaces.workspace_declared_id
  JOIN workspace_versions
    ON workspace_versions.workspace_id = workspaces.id
   AND workspace_versions.id = workspace_processes.base_version_id
   AND workspace_versions.state = 'committed'
 WHERE workspace_processes.org_id = $1
   AND workspace_processes.id = $2
   AND workspace_processes.state_version = $3
   AND workspace_processes.state = 'pending'
   AND workspaces.state = 'active'
   AND workspaces.desired_state IN ('active', 'stopped')
   AND workspaces.dirty_state = 'clean'
   AND workspaces.head_version_id = workspace_processes.base_version_id
   AND workspaces.owner_actor_id IS NULL
   AND workspaces.owner_run_id IS NULL
   AND NOT EXISTS (
       SELECT 1
         FROM workspace_leases
        WHERE workspace_leases.workspace_id = workspaces.id
          AND workspace_leases.state IN ('active', 'releasing')
   )
 FOR UPDATE OF workspace_processes, workspaces`,
		candidate.OrgID,
		candidate.ProcessID,
		candidate.ExpectedStateVersion,
	).Scan(
		&authority.processID,
		&authority.processStateVersion,
		&authority.orgID,
		&authority.projectID,
		&authority.environmentID,
		&authority.workspaceID,
		&authority.baseVersionID,
		&authority.workspaceDefinitionID,
		&authority.regionID,
		&authority.ownershipGeneration,
		&authority.writerGeneration,
		&manifest,
	)
	if err != nil {
		return workspaceExecAuthority{}, err
	}
	var workspaceManifest deployment.WorkspaceManifest
	decoder := json.NewDecoder(bytes.NewReader(manifest))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&workspaceManifest); err != nil {
		return workspaceExecAuthority{}, workspaceExecPermanentError{
			code: "workspace_exec_manifest_invalid",
			err:  fmt.Errorf("decode Workspace exec manifest: %w", err),
		}
	}
	if err := requireJSONEOF(decoder); err != nil {
		return workspaceExecAuthority{}, workspaceExecPermanentError{
			code: "workspace_exec_manifest_invalid",
			err:  fmt.Errorf("decode Workspace exec manifest: %w", err),
		}
	}
	resources, err := normalizeRunResources(workspaceManifest.Resources)
	if err != nil {
		return workspaceExecAuthority{}, err
	}
	network, err := json.Marshal(workspaceManifest.Network)
	if err != nil {
		return workspaceExecAuthority{}, fmt.Errorf("encode Workspace exec network policy: %w", err)
	}
	network, err = jsoncanon.Transform(network)
	if err != nil {
		return workspaceExecAuthority{}, fmt.Errorf("canonicalize Workspace exec network policy: %w", err)
	}
	authority.resources = resources
	authority.networkPolicy = network
	authority.architecture = platformArchitecture
	return authority, nil
}

func (d *Authority) createWorkspaceExecRuntime(
	ctx context.Context,
	tx pgx.Tx,
	authority workspaceExecAuthority,
	observationFreshAfter pgtype.Timestamptz,
) (WorkspaceExecPlacement, error) {
	runAuthority := authority.runAuthority()
	if err := d.checkWorkspaceExecPreparationBudget(ctx, tx, authority); err != nil {
		return WorkspaceExecPlacement{}, err
	}
	worker, err := selectRunWorker(ctx, tx, runAuthority, observationFreshAfter)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkspaceExecPlacement{}, ErrCapacityUnavailable
	}
	if err != nil {
		return WorkspaceExecPlacement{}, fmt.Errorf("select Workspace exec worker: %w", err)
	}
	if err := lockWorkerFence(ctx, tx, workerFence{
		GroupID:               worker.groupID,
		RegionID:              authority.regionID,
		WorkerInstanceID:      worker.workerID,
		WorkerEpoch:           worker.workerEpoch,
		WorkerProtocolVersion: worker.protocolVersion,
		ObservationFreshAfter: observationFreshAfter,
		Role:                  "run",
		RunArchitecture:       authority.architecture,
	}); err != nil {
		return WorkspaceExecPlacement{}, ErrCapacityUnavailable
	}
	if err := lockRunWorkerCapacity(ctx, tx, runAuthority, worker); err != nil {
		return WorkspaceExecPlacement{}, err
	}
	runtimeID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	runtime, err := db.New(tx).CreateWorkspaceExecRuntimeReservation(
		ctx,
		db.CreateWorkspaceExecRuntimeReservationParams{
			ID:                        runtimeID,
			OrgID:                     authority.orgID,
			WorkerGroupID:             worker.groupID,
			ProjectID:                 authority.projectID,
			EnvironmentID:             authority.environmentID,
			RegionID:                  authority.regionID,
			WorkerInstanceID:          worker.workerID,
			RuntimeIdentityID:         worker.runtimeIdentityID,
			DeploymentDefinitionID:    authority.workspaceDefinitionID,
			WorkerEpoch:               worker.workerEpoch,
			NetworkPolicy:             authority.networkPolicy,
			ReservedCpuMillis:         authority.resources.cpuMillis,
			ReservedMemoryBytes:       authority.resources.memoryBytes,
			ReservedWorkloadDiskBytes: authority.resources.workloadDisk,
			ReservedScratchBytes:      authority.resources.scratchBytes,
			ReservedExecutionSlots:    authority.resources.executionSlots,
			WorkspaceID:               authority.workspaceID,
			ProcessID:                 authority.processID,
			BaseWorkspaceVersionID:    authority.baseVersionID,
			ReservationExpiresAt:      pgvalue.Timestamptz(time.Now().Add(d.runPolicy.ReservationTTL)),
			NetworkSlotID:             worker.networkSlotID,
			NetworkSlotGeneration:     worker.networkSlotGeneration,
		},
	)
	if err != nil {
		if isConstraintConflict(err) {
			return WorkspaceExecPlacement{}, ErrCapacityUnavailable
		}
		return WorkspaceExecPlacement{}, fmt.Errorf("create Workspace exec runtime reservation: %w", err)
	}
	return WorkspaceExecPlacement{
		WorkerInstanceID:  runtime.WorkerInstanceID,
		WorkerEpoch:       runtime.WorkerEpoch,
		RuntimeInstanceID: runtime.ID,
	}, nil
}

func (d *Authority) checkWorkspaceExecPreparationBudget(
	ctx context.Context,
	tx pgx.Tx,
	authority workspaceExecAuthority,
) error {
	var active int64
	if err := tx.QueryRow(ctx, `
SELECT count(*)
  FROM runtime_instances
 WHERE environment_id = $1
   AND reserved_process_id IS NOT NULL
   AND reclaimed_at IS NULL`,
		authority.environmentID,
	).Scan(&active); err != nil {
		return fmt.Errorf("read Workspace exec preparation budget: %w", err)
	}
	if active >= d.runPolicy.PreparationLimit {
		return ErrCapacityUnavailable
	}
	return nil
}

func validateWorkspaceExecRuntime(authority workspaceExecAuthority, runtime runRuntime) error {
	network, err := jsoncanon.Transform(runtime.networkPolicy)
	if err != nil {
		return err
	}
	if runtime.deploymentDefinition != authority.workspaceDefinitionID ||
		runtime.restoreCheckpoint.Valid ||
		runtime.cpuMillis != authority.resources.cpuMillis ||
		runtime.memoryBytes != authority.resources.memoryBytes ||
		runtime.workloadDiskBytes != authority.resources.workloadDisk ||
		runtime.scratchBytes != authority.resources.scratchBytes ||
		runtime.executionSlots != authority.resources.executionSlots ||
		!bytes.Equal(network, authority.networkPolicy) {
		return errors.New("Workspace runtime does not match exec authority")
	}
	if runtime.reservedRunID.Valid {
		return errors.New("Workspace runtime is reserved by a Run")
	}
	if runtime.reservedProcessID.Valid &&
		(runtime.reservedProcessID != authority.processID ||
			runtime.reservedVersionID != authority.baseVersionID ||
			!runtime.reservationActive) {
		return errors.New("Workspace runtime reservation does not match exec authority")
	}
	return nil
}

func getWorkspaceExecMount(
	ctx context.Context,
	tx pgx.Tx,
	authority workspaceExecAuthority,
	runtime runRuntime,
) (runWorkspaceMount, error) {
	var mount runWorkspaceMount
	err := tx.QueryRow(ctx, `
SELECT id, worker_instance_id, worker_epoch, runtime_instance_id, state,
       fencing_generation
  FROM workspace_mounts
 WHERE org_id = $1
   AND project_id = $2
   AND environment_id = $3
   AND region_id = $4
   AND workspace_id = $5
   AND materialized_version_id = $6
   AND worker_group_id = $7
   AND worker_instance_id = $8
   AND worker_epoch = $9
   AND runtime_instance_id = $10
   AND state IN ('mounting', 'mounted', 'unmounting')
 FOR UPDATE`,
		authority.orgID,
		authority.projectID,
		authority.environmentID,
		authority.regionID,
		authority.workspaceID,
		authority.baseVersionID,
		runtime.groupID,
		runtime.workerID,
		runtime.workerEpoch,
		runtime.id,
	).Scan(
		&mount.id,
		&mount.workerID,
		&mount.epoch,
		&mount.runtimeID,
		&mount.state,
		&mount.fencingGeneration,
	)
	return mount, err
}

func (d *Authority) grantWorkspaceExec(
	ctx context.Context,
	tx pgx.Tx,
	authority workspaceExecAuthority,
	runtime runRuntime,
	mount runWorkspaceMount,
) error {
	leaseUUID := uuid.Must(uuid.NewV7())
	leaseID := pgvalue.UUID(leaseUUID)
	workspaceUUID, err := pgvalue.UUIDValue(authority.workspaceID)
	if err != nil {
		return err
	}
	ownershipGeneration := authority.ownershipGeneration + 1
	writerGeneration := authority.writerGeneration + 1
	mountGeneration := mount.fencingGeneration + 1
	capability, err := d.fencingKeys.DeriveActive(workspace.FenceInput{
		LeaseID:                leaseUUID,
		WorkspaceID:            workspaceUUID,
		OwnershipGeneration:    ownershipGeneration,
		WriterGeneration:       writerGeneration,
		MountFencingGeneration: mountGeneration,
	})
	if err != nil {
		return fmt.Errorf("derive Workspace exec fence: %w", err)
	}
	q := db.New(tx)
	if _, err := q.AdvanceWorkspaceExecWriter(ctx, db.AdvanceWorkspaceExecWriterParams{
		OwnershipGeneration:         ownershipGeneration,
		WriterGeneration:            writerGeneration,
		OrgID:                       authority.orgID,
		ProjectID:                   authority.projectID,
		EnvironmentID:               authority.environmentID,
		WorkspaceID:                 authority.workspaceID,
		BaseWorkspaceVersionID:      authority.baseVersionID,
		ExpectedOwnershipGeneration: authority.ownershipGeneration,
		ExpectedWriterGeneration:    authority.writerGeneration,
	}); err != nil {
		return fmt.Errorf("advance Workspace exec writer: %w", err)
	}
	if _, err := q.AdvanceWorkspaceExecMountFence(ctx, db.AdvanceWorkspaceExecMountFenceParams{
		FencingGeneration:         mountGeneration,
		ID:                        mount.id,
		OrgID:                     authority.orgID,
		ProjectID:                 authority.projectID,
		EnvironmentID:             authority.environmentID,
		RegionID:                  authority.regionID,
		WorkerGroupID:             runtime.groupID,
		WorkerInstanceID:          runtime.workerID,
		WorkerEpoch:               runtime.workerEpoch,
		RuntimeInstanceID:         runtime.id,
		WorkspaceID:               authority.workspaceID,
		BaseWorkspaceVersionID:    authority.baseVersionID,
		ExpectedFencingGeneration: mount.fencingGeneration,
	}); err != nil {
		return fmt.Errorf("advance Workspace exec mount fence: %w", err)
	}
	if _, err := q.BindWorkspaceExecRuntime(ctx, db.BindWorkspaceExecRuntimeParams{
		RegionID:               pgvalue.Text(authority.regionID),
		WorkerGroupID:          pgvalue.Text(runtime.groupID),
		WorkerInstanceID:       runtime.workerID,
		WorkerEpoch:            pgtype.Int8{Int64: runtime.workerEpoch, Valid: true},
		RuntimeInstanceID:      runtime.id,
		WorkspaceMountID:       mount.id,
		OrgID:                  authority.orgID,
		ProjectID:              authority.projectID,
		EnvironmentID:          authority.environmentID,
		WorkspaceID:            authority.workspaceID,
		ID:                     authority.processID,
		BaseWorkspaceVersionID: authority.baseVersionID,
		ExpectedStateVersion:   authority.processStateVersion,
	}); err != nil {
		return fmt.Errorf("bind Workspace exec runtime: %w", err)
	}
	if _, err := q.InsertWorkspaceExecLease(ctx, db.InsertWorkspaceExecLeaseParams{
		ID:                     leaseID,
		OrgID:                  authority.orgID,
		WorkerGroupID:          runtime.groupID,
		ProjectID:              authority.projectID,
		EnvironmentID:          authority.environmentID,
		RegionID:               authority.regionID,
		WorkerInstanceID:       runtime.workerID,
		WorkerEpoch:            runtime.workerEpoch,
		RuntimeInstanceID:      runtime.id,
		WorkspaceID:            authority.workspaceID,
		WorkspaceMountID:       mount.id,
		ProcessID:              authority.processID,
		BaseWorkspaceVersionID: authority.baseVersionID,
		OwnershipGeneration:    ownershipGeneration,
		WriterGeneration:       writerGeneration,
		MountFencingGeneration: mountGeneration,
		FencingKeyFingerprint:  capability.KeyFingerprint.Bytes(),
		FencingTokenHash:       capability.Hash,
		ExpiresAt:              pgvalue.Timestamptz(time.Now().Add(workspaceExecLeaseTTL)),
	}); err != nil {
		return fmt.Errorf("insert Workspace exec lease: %w", err)
	}
	consumed, err := q.ConsumeWorkspaceExecRuntimeReservation(ctx, db.ConsumeWorkspaceExecRuntimeReservationParams{
		ID:                     runtime.id,
		WorkspaceID:            authority.workspaceID,
		ProcessID:              authority.processID,
		BaseWorkspaceVersionID: authority.baseVersionID,
	})
	if err != nil {
		return fmt.Errorf("consume Workspace exec reservation: %w", err)
	}
	if consumed != 1 {
		return ErrCapacityUnavailable
	}
	return nil
}

func classifyWorkspaceExecCandidateError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCandidateChanged
	}
	return err
}

func (d *Authority) finishRejectedWorkspaceExec(
	ctx context.Context,
	tx pgx.Tx,
	candidate ReadyWorkspaceExecCandidate,
	cause error,
) error {
	var permanent workspaceExecPermanentError
	if !errors.As(cause, &permanent) {
		return classifyWorkspaceExecCandidateError(cause)
	}
	errorJSON, err := json.Marshal(map[string]string{
		"code":    permanent.code,
		"message": permanent.Error(),
	})
	if err != nil {
		return fmt.Errorf("encode Workspace exec rejection: %w", err)
	}
	if err := failPendingWorkspaceExec(
		ctx,
		tx,
		candidate,
		permanent.code,
		errorJSON,
	); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit rejected Workspace exec: %w", err)
	}
	return nil
}

func (d *Authority) FailPendingWorkspaceExec(
	ctx context.Context,
	candidate ReadyWorkspaceExecCandidate,
	reasonCode string,
) error {
	tx, err := d.begin(ctx)
	if err != nil {
		return fmt.Errorf("begin pending Workspace exec failure: %w", err)
	}
	defer rollback(ctx, tx)
	errorJSON, err := json.Marshal(map[string]string{"code": reasonCode})
	if err != nil {
		return err
	}
	if err := failPendingWorkspaceExec(
		ctx,
		tx,
		candidate,
		reasonCode,
		errorJSON,
	); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit pending Workspace exec failure: %w", err)
	}
	return nil
}

func failPendingWorkspaceExec(
	ctx context.Context,
	tx pgx.Tx,
	candidate ReadyWorkspaceExecCandidate,
	reasonCode string,
	errorJSON []byte,
) error {
	q := db.New(tx)
	failed, err := q.FailPendingWorkspaceExecProcess(
		ctx,
		db.FailPendingWorkspaceExecProcessParams{
			ReasonCode:           pgvalue.Text(reasonCode),
			Error:                errorJSON,
			OrgID:                candidate.OrgID,
			ProcessID:            candidate.ProcessID,
			ExpectedStateVersion: candidate.ExpectedStateVersion,
		},
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCandidateChanged
		}
		return fmt.Errorf("fail pending Workspace exec: %w", err)
	}
	claim, err := q.GetIdempotencyClaim(
		ctx,
		db.GetIdempotencyClaimParams{
			EnvironmentID: failed.EnvironmentID,
			ID:            failed.ClaimID,
		},
	)
	if err != nil {
		return fmt.Errorf("read pending Workspace exec claim: %w", err)
	}
	if claim.RetiredAt.Valid {
		return nil
	}
	receipt, err := json.Marshal(map[string]string{
		"process_id":  pgvalue.MustUUIDValue(failed.ID).String(),
		"reason_code": reasonCode,
	})
	if err != nil {
		return err
	}
	if _, err := q.FailIdempotencyClaim(
		ctx,
		db.FailIdempotencyClaimParams{
			Receipt:            receipt,
			EnvironmentID:      claim.EnvironmentID,
			ID:                 claim.ID,
			RequestFingerprint: claim.RequestFingerprint,
		},
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCandidateChanged
		}
		return fmt.Errorf("fail pending Workspace exec claim: %w", err)
	}
	return nil
}
