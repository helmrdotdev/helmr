package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrTokenWaitReconcileAuthority = errors.New("Token Wait reconciliation authority is inconsistent")

const maxTokenWaitReconcileBatch = int32(1000)

type TokenWaitReconcileDB interface {
	Begin(context.Context) (pgx.Tx, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type TokenWaitReconcileBatch struct {
	Examined int
	Resolved int
	Deferred int
}

type TokenWaitRegistration struct {
	EnvironmentID                 uuid.UUID
	RunID                         uuid.UUID
	TokenID                       uuid.UUID
	WaitID                        uuid.UUID
	ResumeAttachID                uuid.UUID
	ExpectedRunStateVersion       int64
	AttemptNumber                 int32
	CurrentRunLeaseID             uuid.UUID
	LeaseSequence                 int64
	WorkerGroupID                 string
	WorkerInstanceID              uuid.UUID
	WorkerEpoch                   int64
	WorkerProtocolVersion         string
	RuntimeInstanceID             uuid.UUID
	RuntimeIdentityID             string
	RegionID                      string
	NetworkSlotID                 uuid.UUID
	NetworkSlotGeneration         int64
	WorkspaceMountID              uuid.UUID
	WorkspaceLeaseID              uuid.UUID
	OwnershipGeneration           int64
	WriterGeneration              int64
	MountFencingGeneration        int64
	RequestFingerprint            string
	ActorSpeculativeInputSequence pgtype.Int8
	TimeoutAt                     pgtype.Timestamptz
	IdleTimeoutMS                 pgtype.Int8
	CheckpointDueAt               pgtype.Timestamptz
	Metadata                      json.RawMessage
	Tags                          []string
}

type TokenWaitRegistrationResult struct {
	WaitID          uuid.UUID
	RunStateVersion int64
	ConditionState  WaitState
	SuspensionState RunWaitState
	Result          json.RawMessage
	ReasonCode      string
}

type TokenWaitReconciler struct {
	db TokenWaitReconcileDB
}

type tokenWaitReconcileCandidate struct {
	waitID uuid.UUID
	runID  uuid.UUID
}

func NewTokenWaitReconciler(database TokenWaitReconcileDB) (*TokenWaitReconciler, error) {
	if database == nil {
		return nil, errors.New("Token Wait reconciliation database is required")
	}
	return &TokenWaitReconciler{db: database}, nil
}

// RegisterWait serializes the Run-to-Token race. The Wait is inserted before
// the Token is locked, so either registration observes a prior terminal Token
// or a concurrent terminalization publishes an intent after this transaction.
func (r *TokenWaitReconciler) RegisterWait(
	ctx context.Context,
	request TokenWaitRegistration,
) (TokenWaitRegistrationResult, error) {
	if request.EnvironmentID == uuid.Nil || request.RunID == uuid.Nil || request.TokenID == uuid.Nil ||
		request.WaitID == uuid.Nil || request.ResumeAttachID == uuid.Nil || request.CurrentRunLeaseID == uuid.Nil ||
		request.WorkerInstanceID == uuid.Nil || request.RuntimeInstanceID == uuid.Nil || request.NetworkSlotID == uuid.Nil ||
		request.WorkspaceMountID == uuid.Nil || request.WorkspaceLeaseID == uuid.Nil {
		return TokenWaitRegistrationResult{}, errors.New("Token Wait registration IDs are required")
	}
	if request.ExpectedRunStateVersion < 0 || request.AttemptNumber <= 0 || request.LeaseSequence <= 0 ||
		request.WorkerEpoch <= 0 || request.NetworkSlotGeneration <= 0 || request.OwnershipGeneration <= 0 ||
		request.WriterGeneration <= 0 || request.MountFencingGeneration <= 0 || request.WorkerGroupID == "" ||
		request.WorkerProtocolVersion == "" || request.RuntimeIdentityID == "" || request.RegionID == "" ||
		len(request.RequestFingerprint) != 71 || request.RequestFingerprint[:7] != "sha256:" {
		return TokenWaitRegistrationResult{}, errors.New("Token Wait registration fences are invalid")
	}
	if request.ActorSpeculativeInputSequence.Valid && request.ActorSpeculativeInputSequence.Int64 < 0 {
		return TokenWaitRegistrationResult{}, errors.New("Token Wait Actor speculative cursor is invalid")
	}
	metadata := request.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}
	if !json.Valid(metadata) {
		return TokenWaitRegistrationResult{}, errors.New("Token Wait registration metadata is invalid")
	}
	tags := request.Tags
	if tags == nil {
		tags = []string{}
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return TokenWaitRegistrationResult{}, fmt.Errorf("begin Token Wait registration: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `
SELECT pg_advisory_xact_lock(
    hashtextextended(concat_ws(':', 'token_wait.register', $1::uuid::text), 0)
)`, request.WaitID); err != nil {
		return TokenWaitRegistrationResult{}, tokenWaitAuthorityError("serialize Token Wait registration", err)
	}
	if replay, found, err := replayTokenWaitRegistration(ctx, tx, request, metadata, tags); err != nil {
		return TokenWaitRegistrationResult{}, err
	} else if found {
		if err := tx.Commit(ctx); err != nil {
			return TokenWaitRegistrationResult{}, fmt.Errorf("commit Token Wait registration replay: %w", err)
		}
		return replay, nil
	}

	locator, err := loadTokenWaitRegistrationLocator(ctx, tx, request.EnvironmentID, request.RunID)
	if err != nil {
		return TokenWaitRegistrationResult{}, tokenWaitAuthorityError("load Token Wait registration locator", err)
	}
	var lockedActorCurrentRunID pgtype.UUID
	var lockedActorCommittedInputSequence, lockedActorNextInputSequence int64
	if locator.ownerActorID.Valid {
		var actorState string
		if err := tx.QueryRow(ctx, `
SELECT state, current_run_id, committed_input_sequence, next_input_sequence
  FROM actors
 WHERE id = $1
 FOR UPDATE`, locator.ownerActorID).Scan(
			&actorState, &lockedActorCurrentRunID,
			&lockedActorCommittedInputSequence, &lockedActorNextInputSequence,
		); err != nil {
			return TokenWaitRegistrationResult{}, tokenWaitAuthorityError("lock owning Actor", err)
		}
		if actorState != "open" && actorState != "closing" {
			return TokenWaitRegistrationResult{}, tokenWaitAuthorityError("owning Actor is not active", nil)
		}
	}

	lineage, run, err := lockTokenWaitLineage(ctx, tx, request.EnvironmentID, request.RunID)
	if err != nil {
		return TokenWaitRegistrationResult{}, err
	}
	if run.workspaceID != locator.workspaceID || run.status != RunStatusRunning ||
		run.stateVersion != request.ExpectedRunStateVersion || run.currentAttempt != request.AttemptNumber ||
		!run.currentRunLeaseID.Valid || uuid.UUID(run.currentRunLeaseID.Bytes) != request.CurrentRunLeaseID ||
		!run.activeStartedAt.Valid {
		return TokenWaitRegistrationResult{}, tokenWaitAuthorityError("Run registration fence does not match", nil)
	}
	if run.parentRunID.Valid {
		return TokenWaitRegistrationResult{}, tokenWaitAuthorityError("child Token Wait registration is not implemented", nil)
	}
	if err := validateAndLockTokenWaitWorkspace(ctx, tx, request.EnvironmentID, locator, lineage, lockedActorCurrentRunID); err != nil {
		return TokenWaitRegistrationResult{}, err
	}
	var attemptEntrypointKind string
	var attemptActorStartInputSequence pgtype.Int8
	var attemptTerminalAt pgtype.Timestamptz
	if err := tx.QueryRow(ctx, `
SELECT entrypoint_kind, actor_start_input_sequence, terminal_at
  FROM run_attempts
 WHERE run_id = $1
   AND number = $2
   AND workspace_id = $3
 FOR UPDATE`, request.RunID, request.AttemptNumber, locator.workspaceID).Scan(
		&attemptEntrypointKind, &attemptActorStartInputSequence, &attemptTerminalAt,
	); err != nil || attemptTerminalAt.Valid {
		return TokenWaitRegistrationResult{}, tokenWaitAuthorityError("lock current Run Attempt", err)
	}
	if err := validateTokenWaitActorCursor(
		request.ActorSpeculativeInputSequence, locator.ownerActorID, lockedActorCurrentRunID,
		lockedActorCommittedInputSequence, lockedActorNextInputSequence,
		run, attemptEntrypointKind, attemptActorStartInputSequence,
	); err != nil {
		return TokenWaitRegistrationResult{}, err
	}
	q := New(tx)
	workerGroup, err := q.LockRunLeaseClaimWorkerGroup(ctx, LockRunLeaseClaimWorkerGroupParams{
		ID: request.WorkerGroupID, RegionID: request.RegionID,
	})
	if err != nil ||
		(workerGroup.State != WorkerGroupStateActive && workerGroup.State != WorkerGroupStateDraining) ||
		!workerGroup.AllowsRun ||
		workerGroup.ProtocolVersion != request.WorkerProtocolVersion {
		return TokenWaitRegistrationResult{}, tokenWaitAuthorityError("lock active worker group", err)
	}
	worker, err := q.LockRunLeaseClaimWorker(ctx, LockRunLeaseClaimWorkerParams{
		ID: pgtype.UUID{Bytes: request.WorkerInstanceID, Valid: true}, WorkerGroupID: request.WorkerGroupID,
	})
	if err != nil ||
		(worker.State != WorkerInstanceStateActive && worker.State != WorkerInstanceStateDraining) ||
		!worker.CurrentEpoch.Valid ||
		worker.CurrentEpoch.Int64 != request.WorkerEpoch ||
		!worker.SupportsRun || worker.ProtocolVersion != request.WorkerProtocolVersion ||
		!worker.RuntimeIdentityID.Valid || worker.RuntimeIdentityID.String != request.RuntimeIdentityID {
		return TokenWaitRegistrationResult{}, tokenWaitAuthorityError("lock current worker epoch", err)
	}
	slot, err := q.LockRunLeaseClaimNetworkSlot(ctx, LockRunLeaseClaimNetworkSlotParams{
		ID: pgtype.UUID{Bytes: request.NetworkSlotID, Valid: true}, WorkerGroupID: request.WorkerGroupID,
		WorkerInstanceID: pgtype.UUID{Bytes: request.WorkerInstanceID, Valid: true},
		WorkerEpoch:      request.WorkerEpoch, Generation: request.NetworkSlotGeneration,
		RuntimeInstanceID: pgtype.UUID{Bytes: request.RuntimeInstanceID, Valid: true},
	})
	if err != nil || slot.State != WorkerNetworkSlotStateBound {
		return TokenWaitRegistrationResult{}, tokenWaitAuthorityError("lock bound worker network slot", err)
	}
	runtime, err := q.LockRunLeaseClaimRuntime(ctx, LockRunLeaseClaimRuntimeParams{
		ID: pgtype.UUID{Bytes: request.RuntimeInstanceID, Valid: true}, OrgID: locator.orgID,
		ProjectID: locator.projectID, EnvironmentID: pgtype.UUID{Bytes: request.EnvironmentID, Valid: true},
		RegionID: request.RegionID, WorkerGroupID: request.WorkerGroupID,
		WorkerInstanceID: pgtype.UUID{Bytes: request.WorkerInstanceID, Valid: true}, WorkerEpoch: request.WorkerEpoch,
		WorkspaceID: pgtype.UUID{Bytes: locator.workspaceID, Valid: true},
	})
	if err != nil || runtime.RuntimeIdentityID != request.RuntimeIdentityID ||
		runtime.DesiredState != RuntimeDesiredStateReady || runtime.ObservedState != RuntimeObservedStateReady ||
		runtime.ObservedDesiredVersion != runtime.DesiredVersion || runtime.TerminalAt.Valid {
		return TokenWaitRegistrationResult{}, tokenWaitAuthorityError("lock ready runtime", err)
	}
	var leaseState RunLeaseState
	if err := tx.QueryRow(ctx, `
SELECT state
  FROM run_leases
 WHERE id = $1 AND run_id = $2 AND attempt_number = $3 AND workspace_id = $4
   AND lease_sequence = $5 AND worker_group_id = $6 AND worker_instance_id = $7
   AND worker_epoch = $8 AND runtime_instance_id = $9 AND network_slot_id = $10
   AND network_slot_generation = $11 AND runtime_identity_id = $12
   AND worker_protocol_version = $13 AND region_id = $14
   AND state = 'running' AND expires_at > transaction_timestamp()
 FOR UPDATE`, request.CurrentRunLeaseID, request.RunID, request.AttemptNumber, locator.workspaceID,
		request.LeaseSequence, request.WorkerGroupID, request.WorkerInstanceID, request.WorkerEpoch,
		request.RuntimeInstanceID, request.NetworkSlotID, request.NetworkSlotGeneration,
		request.RuntimeIdentityID, request.WorkerProtocolVersion, request.RegionID,
	).Scan(&leaseState); err != nil || leaseState != RunLeaseStateRunning {
		return TokenWaitRegistrationResult{}, tokenWaitAuthorityError("lock current unexpired Run Lease", err)
	}
	mount, err := q.LockRunLeaseClaimMount(ctx, LockRunLeaseClaimMountParams{
		ID: pgtype.UUID{Bytes: request.WorkspaceMountID, Valid: true}, OrgID: locator.orgID,
		ProjectID: locator.projectID, EnvironmentID: pgtype.UUID{Bytes: request.EnvironmentID, Valid: true},
		RegionID: request.RegionID, WorkerGroupID: request.WorkerGroupID,
		WorkerInstanceID: pgtype.UUID{Bytes: request.WorkerInstanceID, Valid: true}, WorkerEpoch: request.WorkerEpoch,
		RuntimeInstanceID: pgtype.UUID{Bytes: request.RuntimeInstanceID, Valid: true},
		WorkspaceID:       pgtype.UUID{Bytes: locator.workspaceID, Valid: true},
	})
	if err != nil || mount.State != WorkspaceMountStateMounted || mount.FencingGeneration != request.MountFencingGeneration {
		return TokenWaitRegistrationResult{}, tokenWaitAuthorityError("lock mounted Workspace", err)
	}
	workspaceLease, err := q.LockRunLeaseClaimWorkspaceLease(ctx, LockRunLeaseClaimWorkspaceLeaseParams{
		ID: pgtype.UUID{Bytes: request.WorkspaceLeaseID, Valid: true}, OrgID: locator.orgID,
		ProjectID: locator.projectID, EnvironmentID: pgtype.UUID{Bytes: request.EnvironmentID, Valid: true},
		RegionID: request.RegionID, WorkerGroupID: request.WorkerGroupID,
		WorkerInstanceID: pgtype.UUID{Bytes: request.WorkerInstanceID, Valid: true}, WorkerEpoch: request.WorkerEpoch,
		RuntimeInstanceID: pgtype.UUID{Bytes: request.RuntimeInstanceID, Valid: true},
		WorkspaceID:       pgtype.UUID{Bytes: locator.workspaceID, Valid: true},
		WorkspaceMountID:  pgtype.UUID{Bytes: request.WorkspaceMountID, Valid: true},
	})
	if err != nil || !workspaceLease.OwnerRunLeaseID.Valid ||
		uuid.UUID(workspaceLease.OwnerRunLeaseID.Bytes) != request.CurrentRunLeaseID ||
		workspaceLease.OwnershipGeneration != request.OwnershipGeneration ||
		workspaceLease.WriterGeneration != request.WriterGeneration ||
		workspaceLease.MountFencingGeneration != request.MountFencingGeneration {
		return TokenWaitRegistrationResult{}, tokenWaitAuthorityError("lock current unexpired Workspace Lease", err)
	}
	if err := lockOuterTokenWait(ctx, tx, request.RunID); err != nil {
		return TokenWaitRegistrationResult{}, err
	}

	var waitingVersion int64
	if err := tx.QueryRow(ctx, `
UPDATE runs
   SET status = 'waiting',
       state_version = state_version + 1,
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND status = 'running'
   AND state_version = $2
   AND current_attempt_number = $3
   AND current_run_lease_id = $4
   AND active_started_at IS NOT NULL
   AND transaction_timestamp() < active_started_at
         + ((max_active_duration_ms - active_elapsed_ms) * interval '1 millisecond')
RETURNING state_version`, request.RunID, request.ExpectedRunStateVersion, request.AttemptNumber, request.CurrentRunLeaseID).Scan(&waitingVersion); err != nil {
		return TokenWaitRegistrationResult{}, tokenWaitAuthorityError("move Run to waiting", err)
	}

	command, err := tx.Exec(ctx, `
INSERT INTO run_waits (
    id, environment_id, run_id, workspace_id, kind, timeout_at,
    idle_timeout_ms, token_id, token_registration_run_state_version,
    registration_request_fingerprint, expected_run_state_version, attempt_number,
    actor_speculative_input_sequence, current_run_lease_id,
    checkpoint_due_at, resume_attach_id, metadata, tags
) VALUES (
    $1, $2, $3, $4, 'token', $5, $6, $7, $8, $9, $10, $11, $12, $13,
    $14, $15, $16::jsonb, $17
)`,
		request.WaitID, request.EnvironmentID, request.RunID, locator.workspaceID,
		request.TimeoutAt, request.IdleTimeoutMS, request.TokenID,
		request.ExpectedRunStateVersion, request.RequestFingerprint, waitingVersion, request.AttemptNumber,
		request.ActorSpeculativeInputSequence, request.CurrentRunLeaseID, request.CheckpointDueAt, request.ResumeAttachID,
		metadata, tags,
	)
	if err != nil || command.RowsAffected() != 1 {
		return TokenWaitRegistrationResult{}, tokenWaitAuthorityError("insert Token Wait", err)
	}

	var tokenState TokenState
	var completionData []byte
	if err := tx.QueryRow(ctx, `
SELECT state, completion_data
  FROM tokens
 WHERE environment_id = $1
   AND id = $2
 FOR UPDATE`, request.EnvironmentID, request.TokenID).Scan(&tokenState, &completionData); err != nil {
		return TokenWaitRegistrationResult{}, tokenWaitAuthorityError("lock Token registration condition", err)
	}

	result := TokenWaitRegistrationResult{
		WaitID: request.WaitID, RunStateVersion: waitingVersion,
		ConditionState: WaitStatePending, SuspensionState: RunWaitStateHot,
	}
	if tokenState != TokenStatePending {
		resolution, err := tokenWaitTerminalResolution(tokenState, completionData)
		if err != nil {
			return TokenWaitRegistrationResult{}, err
		}
		wait := tokenWaitLockedWait{
			id: request.WaitID, environmentID: request.EnvironmentID, runID: request.RunID,
			workspaceID: locator.workspaceID, tokenID: request.TokenID, kind: WaitKindToken,
			conditionState: WaitStatePending, suspensionState: RunWaitStateHot,
			expectedRunStateVersion: waitingVersion, attemptNumber: request.AttemptNumber,
			currentRunLeaseID: pgtype.UUID{Bytes: request.CurrentRunLeaseID, Valid: true},
		}
		run.stateVersion = waitingVersion
		run.status = RunStatusWaiting
		if err := reconcileHotTokenWait(ctx, tx, run, wait, resolution); err != nil {
			return TokenWaitRegistrationResult{}, err
		}
		result.RunStateVersion = waitingVersion + 1
		result.ConditionState = resolution.conditionState
		result.SuspensionState = RunWaitStateReleased
		result.Result = resolution.result
		if resolution.reasonCode != nil {
			result.ReasonCode = *resolution.reasonCode
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenWaitRegistrationResult{}, fmt.Errorf("commit Token Wait registration: %w", err)
	}
	return result, nil
}

func replayTokenWaitRegistration(
	ctx context.Context,
	tx pgx.Tx,
	request TokenWaitRegistration,
	metadata json.RawMessage,
	tags []string,
) (TokenWaitRegistrationResult, bool, error) {
	var result TokenWaitRegistrationResult
	var rawResult []byte
	var reason pgtype.Text
	err := tx.QueryRow(ctx, `
SELECT run_waits.id,
       runs.state_version,
       run_waits.condition_state,
       run_waits.suspension_state,
       run_waits.condition_result,
       run_waits.condition_reason_code
  FROM run_waits
  JOIN runs
    ON runs.environment_id = run_waits.environment_id
   AND runs.id = run_waits.run_id
  JOIN run_leases
    ON run_leases.id = $7
   AND run_leases.run_id = run_waits.run_id
   AND run_leases.attempt_number = run_waits.attempt_number
   AND run_leases.workspace_id = run_waits.workspace_id
  JOIN workspace_leases
    ON workspace_leases.id = $17
   AND workspace_leases.owner_run_lease_id = run_leases.id
   AND workspace_leases.workspace_id = run_waits.workspace_id
 WHERE run_waits.id = $1
   AND run_waits.environment_id = $2
   AND run_waits.run_id = $3
   AND run_waits.token_id = $4
   AND run_waits.kind = 'token'
   AND run_waits.resume_attach_id = $5
	AND run_waits.attempt_number = $6
	AND run_waits.registration_request_fingerprint = $8
	AND (run_waits.current_run_lease_id = $7 OR run_waits.prior_run_lease_id = $7)
	AND run_waits.metadata = $9::jsonb
	AND run_waits.tags = $10::text[]
	AND run_leases.lease_sequence = $11
	AND run_leases.worker_group_id = $12
	AND run_leases.worker_instance_id = $13
	AND run_leases.worker_epoch = $14
	AND run_leases.worker_protocol_version = $15
	AND run_leases.runtime_instance_id = $16
	AND run_leases.runtime_identity_id = $18
	AND workspace_leases.workspace_mount_id = $19
	AND workspace_leases.ownership_generation = $20
	AND workspace_leases.writer_generation = $21
	AND workspace_leases.mount_fencing_generation = $22
	AND run_leases.network_slot_id = $23
	AND run_leases.network_slot_generation = $24
	AND run_leases.region_id = $25
	AND run_waits.actor_speculative_input_sequence IS NOT DISTINCT FROM $26`,
		request.WaitID, request.EnvironmentID, request.RunID, request.TokenID,
		request.ResumeAttachID, request.AttemptNumber, request.CurrentRunLeaseID, request.RequestFingerprint,
		metadata, tags,
		request.LeaseSequence, request.WorkerGroupID, request.WorkerInstanceID,
		request.WorkerEpoch, request.WorkerProtocolVersion, request.RuntimeInstanceID,
		request.WorkspaceLeaseID, request.RuntimeIdentityID, request.WorkspaceMountID,
		request.OwnershipGeneration, request.WriterGeneration, request.MountFencingGeneration,
		request.NetworkSlotID, request.NetworkSlotGeneration, request.RegionID,
		request.ActorSpeculativeInputSequence,
	).Scan(
		&result.WaitID, &result.RunStateVersion, &result.ConditionState,
		&result.SuspensionState, &rawResult, &reason,
	)
	if err == nil {
		result.Result = json.RawMessage(rawResult)
		if reason.Valid {
			result.ReasonCode = reason.String
		}
		return result, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TokenWaitRegistrationResult{}, false, tokenWaitAuthorityError("load Token Wait registration replay", err)
	}
	var conflicting bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_waits WHERE id = $1)`, request.WaitID).Scan(&conflicting); err != nil {
		return TokenWaitRegistrationResult{}, false, tokenWaitAuthorityError("check Token Wait registration replay conflict", err)
	}
	if conflicting {
		return TokenWaitRegistrationResult{}, false, tokenWaitAuthorityError("Token Wait registration replay does not match", nil)
	}
	return TokenWaitRegistrationResult{}, false, nil
}

func (r *TokenWaitReconciler) ReconcileBatch(
	ctx context.Context,
	environmentID uuid.UUID,
	tokenID uuid.UUID,
	limit int32,
) (TokenWaitReconcileBatch, error) {
	if environmentID == uuid.Nil || tokenID == uuid.Nil {
		return TokenWaitReconcileBatch{}, errors.New("Token Wait reconciliation IDs are required")
	}
	if limit <= 0 {
		return TokenWaitReconcileBatch{}, errors.New("Token Wait reconciliation limit must be positive")
	}
	if limit > maxTokenWaitReconcileBatch {
		return TokenWaitReconcileBatch{}, fmt.Errorf(
			"Token Wait reconciliation limit must not exceed %d",
			maxTokenWaitReconcileBatch,
		)
	}

	rows, err := r.db.Query(ctx, `
SELECT id, run_id
  FROM run_waits
 WHERE environment_id = $1
   AND token_id = $2
   AND (condition_state = 'pending' OR suspension_state = 'checkpointing')
 ORDER BY token_id, condition_state, id
 LIMIT $3`, environmentID, tokenID, limit)
	if err != nil {
		return TokenWaitReconcileBatch{}, fmt.Errorf("discover pending Token Waits: %w", err)
	}
	candidates := make([]tokenWaitReconcileCandidate, 0, limit)
	for rows.Next() {
		var candidate tokenWaitReconcileCandidate
		if err := rows.Scan(&candidate.waitID, &candidate.runID); err != nil {
			rows.Close()
			return TokenWaitReconcileBatch{}, fmt.Errorf("scan pending Token Wait: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return TokenWaitReconcileBatch{}, fmt.Errorf("iterate pending Token Waits: %w", err)
	}
	rows.Close()

	batch := TokenWaitReconcileBatch{Examined: len(candidates)}
	for _, candidate := range candidates {
		resolved, deferred, err := r.reconcileOne(ctx, environmentID, tokenID, candidate)
		if err != nil {
			return batch, err
		}
		if resolved {
			batch.Resolved++
		}
		if deferred {
			batch.Deferred++
		}
	}
	return batch, nil
}

type tokenWaitLocator struct {
	waitID       uuid.UUID
	runID        uuid.UUID
	workspaceID  uuid.UUID
	attempt      int32
	ownerActorID pgtype.UUID
}

type tokenWaitRegistrationLocator struct {
	workspaceID  uuid.UUID
	ownerActorID pgtype.UUID
	orgID        pgtype.UUID
	projectID    pgtype.UUID
}

func loadTokenWaitRegistrationLocator(
	ctx context.Context,
	tx pgx.Tx,
	environmentID uuid.UUID,
	runID uuid.UUID,
) (tokenWaitRegistrationLocator, error) {
	var locator tokenWaitRegistrationLocator
	err := tx.QueryRow(ctx, `
SELECT runs.workspace_id, workspaces.owner_actor_id, runs.org_id, runs.project_id
  FROM runs
  JOIN workspaces
    ON workspaces.environment_id = runs.environment_id
   AND workspaces.id = runs.workspace_id
 WHERE runs.environment_id = $1
   AND runs.id = $2`, environmentID, runID).Scan(
		&locator.workspaceID, &locator.ownerActorID, &locator.orgID, &locator.projectID,
	)
	return locator, err
}

func validateAndLockTokenWaitWorkspace(
	ctx context.Context,
	tx pgx.Tx,
	environmentID uuid.UUID,
	locator tokenWaitRegistrationLocator,
	lineage []tokenWaitLockedRun,
	lockedActorCurrentRunID pgtype.UUID,
) error {
	var ownerActorID, ownerRunID pgtype.UUID
	var workspaceState WorkspaceState
	var desiredState WorkspaceDesiredState
	err := tx.QueryRow(ctx, `
SELECT owner_actor_id, owner_run_id, state, desired_state
  FROM workspaces
 WHERE id = $1
   AND environment_id = $2
 FOR UPDATE`, locator.workspaceID, environmentID).Scan(
		&ownerActorID,
		&ownerRunID,
		&workspaceState,
		&desiredState,
	)
	if err != nil {
		return tokenWaitAuthorityError("lock Run Workspace", err)
	}
	if workspaceState != WorkspaceStateActive || desiredState != WorkspaceDesiredStateActive ||
		ownerActorID != locator.ownerActorID {
		return tokenWaitAuthorityError("Workspace ownership changed", nil)
	}
	if ownerActorID.Valid {
		if !lockedActorCurrentRunID.Valid || !tokenWaitLineageContains(lineage, lockedActorCurrentRunID) {
			return tokenWaitAuthorityError("Actor current Run is outside the locked lineage", nil)
		}
	} else if !ownerRunID.Valid || !tokenWaitLineageContains(lineage, ownerRunID) {
		return tokenWaitAuthorityError("Workspace owner Run is outside the locked lineage", nil)
	}
	return nil
}

func validateTokenWaitActorCursor(
	cursor pgtype.Int8,
	ownerActorID pgtype.UUID,
	actorCurrentRunID pgtype.UUID,
	actorCommittedInputSequence int64,
	actorNextInputSequence int64,
	run tokenWaitLockedRun,
	attemptEntrypointKind string,
	attemptActorStartInputSequence pgtype.Int8,
) error {
	switch run.entrypointKind {
	case "task":
		if run.actorID.Valid || ownerActorID.Valid || cursor.Valid || attemptEntrypointKind != "task" ||
			attemptActorStartInputSequence.Valid {
			return tokenWaitAuthorityError("Task Token Wait carries Actor authority", nil)
		}
	case "actor":
		if !run.actorID.Valid || run.actorID != ownerActorID || !actorCurrentRunID.Valid ||
			uuid.UUID(actorCurrentRunID.Bytes) != run.id || attemptEntrypointKind != "actor" ||
			!attemptActorStartInputSequence.Valid || !cursor.Valid ||
			attemptActorStartInputSequence.Int64 > actorCommittedInputSequence ||
			cursor.Int64 < actorCommittedInputSequence ||
			cursor.Int64 > actorCommittedInputSequence+1 ||
			cursor.Int64 >= actorNextInputSequence {
			return tokenWaitAuthorityError("Actor Token Wait cursor authority does not match", nil)
		}
	default:
		return tokenWaitAuthorityError("Token Wait entrypoint kind is invalid", nil)
	}
	return nil
}

type tokenWaitLockedRun struct {
	id                uuid.UUID
	parentRunID       pgtype.UUID
	workspaceID       uuid.UUID
	actorID           pgtype.UUID
	entrypointKind    string
	status            RunStatus
	stateVersion      int64
	currentAttempt    int32
	currentRunLeaseID pgtype.UUID
	activeStartedAt   pgtype.Timestamptz
	depth             int32
	cycle             bool
}

type tokenWaitLockedWait struct {
	id                      uuid.UUID
	environmentID           uuid.UUID
	runID                   uuid.UUID
	workspaceID             uuid.UUID
	tokenID                 uuid.UUID
	kind                    WaitKind
	conditionState          WaitState
	suspensionState         RunWaitState
	expectedRunStateVersion int64
	attemptNumber           int32
	currentRunLeaseID       pgtype.UUID
	priorRunLeaseID         pgtype.UUID
	suspendCheckpointID     pgtype.UUID
	resumeRequestVersion    int64
}

type tokenWaitResolution struct {
	conditionState WaitState
	result         json.RawMessage
	reasonCode     *string
	conditionError json.RawMessage
}

func (r *TokenWaitReconciler) reconcileOne(
	ctx context.Context,
	environmentID uuid.UUID,
	tokenID uuid.UUID,
	candidate tokenWaitReconcileCandidate,
) (resolved bool, deferred bool, returnErr error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, false, fmt.Errorf("begin Token Wait reconciliation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	locator, err := loadTokenWaitLocator(ctx, tx, environmentID, tokenID, candidate)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return false, false, fmt.Errorf("commit stale Token Wait reconciliation: %w", err)
		}
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}

	var lockedActorCurrentRunID pgtype.UUID
	if locator.ownerActorID.Valid {
		var actorState string
		err = tx.QueryRow(ctx, `
SELECT state, current_run_id
  FROM actors
 WHERE id = $1
 FOR UPDATE`, locator.ownerActorID).Scan(&actorState, &lockedActorCurrentRunID)
		if err != nil {
			return false, false, tokenWaitAuthorityError("lock owning Actor", err)
		}
		if actorState != "open" && actorState != "closing" {
			return false, false, tokenWaitAuthorityError("owning Actor is not active", nil)
		}
	}

	lineage, addressedRun, err := lockTokenWaitLineage(ctx, tx, environmentID, locator.runID)
	if err != nil {
		return false, false, err
	}
	if addressedRun.workspaceID != locator.workspaceID || addressedRun.currentAttempt != locator.attempt {
		return false, false, tokenWaitAuthorityError("Run locator changed", nil)
	}
	if addressedRun.parentRunID.Valid {
		return false, false, tokenWaitAuthorityError("child Token Wait reconciliation is not implemented", nil)
	}

	var ownerActorID, ownerRunID pgtype.UUID
	var workspaceState WorkspaceState
	var desiredState WorkspaceDesiredState
	err = tx.QueryRow(ctx, `
SELECT owner_actor_id, owner_run_id, state, desired_state
  FROM workspaces
 WHERE id = $1
   AND environment_id = $2
 FOR UPDATE`, locator.workspaceID, environmentID).Scan(
		&ownerActorID,
		&ownerRunID,
		&workspaceState,
		&desiredState,
	)
	if err != nil {
		return false, false, tokenWaitAuthorityError("lock Run Workspace", err)
	}
	if workspaceState != WorkspaceStateActive || desiredState != WorkspaceDesiredStateActive ||
		ownerActorID != locator.ownerActorID {
		return false, false, tokenWaitAuthorityError("Workspace ownership changed", nil)
	}
	if ownerActorID.Valid {
		if !lockedActorCurrentRunID.Valid || !tokenWaitLineageContains(lineage, lockedActorCurrentRunID) {
			return false, false, tokenWaitAuthorityError("Actor current Run is outside the locked lineage", nil)
		}
	} else if !ownerRunID.Valid || !tokenWaitLineageContains(lineage, ownerRunID) {
		return false, false, tokenWaitAuthorityError("Workspace owner Run is outside the locked lineage", nil)
	}

	var attemptTerminalAt pgtype.Timestamptz
	err = tx.QueryRow(ctx, `
SELECT terminal_at
  FROM run_attempts
 WHERE run_id = $1
   AND number = $2
   AND workspace_id = $3
 FOR UPDATE`, locator.runID, locator.attempt, locator.workspaceID).Scan(&attemptTerminalAt)
	if err != nil || attemptTerminalAt.Valid {
		return false, false, tokenWaitAuthorityError("lock current Run Attempt", err)
	}

	if err := lockOuterTokenWait(ctx, tx, locator.runID); err != nil {
		return false, false, err
	}
	wait, err := lockCurrentTokenWait(ctx, tx, environmentID, tokenID, locator)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return false, false, fmt.Errorf("commit converged Token Wait reconciliation: %w", err)
		}
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if err := validateLockedTokenWait(addressedRun, wait); err != nil {
		return false, false, err
	}
	if wait.conditionState != WaitStatePending {
		if err := tx.Commit(ctx); err != nil {
			return false, false, fmt.Errorf("commit deferred Token Wait reconciliation: %w", err)
		}
		return false, true, nil
	}

	var tokenState TokenState
	var completionData []byte
	err = tx.QueryRow(ctx, `
SELECT state, completion_data
  FROM tokens
 WHERE environment_id = $1
   AND id = $2
 FOR UPDATE`, environmentID, tokenID).Scan(&tokenState, &completionData)
	if err != nil {
		return false, false, tokenWaitAuthorityError("lock terminal Token", err)
	}
	resolution, err := tokenWaitTerminalResolution(tokenState, completionData)
	if err != nil {
		return false, false, err
	}

	switch wait.suspensionState {
	case RunWaitStateHot:
		err = reconcileHotTokenWait(ctx, tx, addressedRun, wait, resolution)
	case RunWaitStateCheckpointing:
		err = reconcileCheckpointingTokenWait(ctx, tx, wait, resolution)
	case RunWaitStateParked:
		err = reconcileParkedTokenWait(ctx, tx, addressedRun, wait, resolution)
	default:
		err = tokenWaitAuthorityError("pending Token Wait has an ineligible suspension state", nil)
	}
	if err != nil {
		return false, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, false, fmt.Errorf("commit Token Wait reconciliation: %w", err)
	}
	return true, wait.suspensionState == RunWaitStateCheckpointing, nil
}

func loadTokenWaitLocator(
	ctx context.Context,
	tx pgx.Tx,
	environmentID uuid.UUID,
	tokenID uuid.UUID,
	candidate tokenWaitReconcileCandidate,
) (tokenWaitLocator, error) {
	var locator tokenWaitLocator
	err := tx.QueryRow(ctx, `
SELECT run_waits.id,
       run_waits.run_id,
       run_waits.workspace_id,
       run_waits.attempt_number,
       workspaces.owner_actor_id
  FROM run_waits
  JOIN runs
    ON runs.environment_id = run_waits.environment_id
   AND runs.id = run_waits.run_id
  JOIN workspaces
    ON workspaces.id = run_waits.workspace_id
 WHERE run_waits.id = $1
   AND run_waits.environment_id = $2
   AND run_waits.run_id = $3
   AND run_waits.token_id = $4
   AND run_waits.kind = 'token'
   AND (run_waits.condition_state = 'pending'
        OR run_waits.suspension_state = 'checkpointing')`,
		candidate.waitID,
		environmentID,
		candidate.runID,
		tokenID,
	).Scan(
		&locator.waitID,
		&locator.runID,
		&locator.workspaceID,
		&locator.attempt,
		&locator.ownerActorID,
	)
	if err != nil {
		return tokenWaitLocator{}, err
	}
	return locator, nil
}

func lockTokenWaitLineage(
	ctx context.Context,
	tx pgx.Tx,
	environmentID uuid.UUID,
	runID uuid.UUID,
) ([]tokenWaitLockedRun, tokenWaitLockedRun, error) {
	rows, err := tx.Query(ctx, `
WITH RECURSIVE lineage AS (
    SELECT id, parent_run_id, 0::integer AS depth, ARRAY[id] AS path, false AS cycle
      FROM runs
     WHERE environment_id = $1
       AND id = $2
    UNION ALL
    SELECT parent.id,
           parent.parent_run_id,
           child.depth + 1,
           child.path || parent.id,
           parent.id = ANY(child.path)
      FROM lineage AS child
      JOIN runs AS parent
        ON parent.environment_id = $1
       AND parent.id = child.parent_run_id
     WHERE NOT child.cycle
)
SELECT runs.id,
       runs.parent_run_id,
       runs.workspace_id,
       runs.actor_id,
	   runs.entrypoint_kind,
       runs.status,
       runs.state_version,
	       runs.current_attempt_number,
	       runs.current_run_lease_id,
	       runs.active_started_at,
	       lineage.depth,
       lineage.cycle
  FROM lineage
  JOIN runs ON runs.id = lineage.id
 ORDER BY lineage.depth DESC, runs.id
 FOR UPDATE OF runs`, environmentID, runID)
	if err != nil {
		return nil, tokenWaitLockedRun{}, tokenWaitAuthorityError("lock Run lineage", err)
	}
	defer rows.Close()
	var lineage []tokenWaitLockedRun
	var addressed tokenWaitLockedRun
	foundAddressed := false
	for rows.Next() {
		var row tokenWaitLockedRun
		if err := rows.Scan(
			&row.id,
			&row.parentRunID,
			&row.workspaceID,
			&row.actorID,
			&row.entrypointKind,
			&row.status,
			&row.stateVersion,
			&row.currentAttempt,
			&row.currentRunLeaseID,
			&row.activeStartedAt,
			&row.depth,
			&row.cycle,
		); err != nil {
			return nil, tokenWaitLockedRun{}, tokenWaitAuthorityError("scan locked Run lineage", err)
		}
		if row.cycle {
			return nil, tokenWaitLockedRun{}, tokenWaitAuthorityError("Run lineage contains a cycle", nil)
		}
		lineage = append(lineage, row)
		if row.depth == 0 && row.id == runID {
			addressed = row
			foundAddressed = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, tokenWaitLockedRun{}, tokenWaitAuthorityError("iterate locked Run lineage", err)
	}
	if !foundAddressed {
		return nil, tokenWaitLockedRun{}, tokenWaitAuthorityError("addressed Run does not exist", nil)
	}
	return lineage, addressed, nil
}

func tokenWaitLineageContains(lineage []tokenWaitLockedRun, id pgtype.UUID) bool {
	if !id.Valid {
		return false
	}
	for _, run := range lineage {
		if run.id == uuid.UUID(id.Bytes) {
			return true
		}
	}
	return false
}

func lockOuterTokenWait(ctx context.Context, tx pgx.Tx, runID uuid.UUID) error {
	rows, err := tx.Query(ctx, `
SELECT id
  FROM run_waits
 WHERE child_run_id = $1
   AND suspension_state IN ('hot', 'checkpointing', 'parked', 'resume_pending', 'resuming')
 ORDER BY id
 FOR UPDATE`, runID)
	if err != nil {
		return tokenWaitAuthorityError("lock enclosing Run Wait", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return tokenWaitAuthorityError("scan enclosing Run Wait", err)
		}
	}
	if err := rows.Err(); err != nil {
		return tokenWaitAuthorityError("iterate enclosing Run Wait", err)
	}
	return nil
}

func lockCurrentTokenWait(
	ctx context.Context,
	tx pgx.Tx,
	environmentID uuid.UUID,
	tokenID uuid.UUID,
	locator tokenWaitLocator,
) (tokenWaitLockedWait, error) {
	var wait tokenWaitLockedWait
	err := tx.QueryRow(ctx, `
SELECT id,
       environment_id,
       run_id,
       workspace_id,
       token_id,
       kind,
       condition_state,
       suspension_state,
       expected_run_state_version,
       attempt_number,
       current_run_lease_id,
       prior_run_lease_id,
       suspend_checkpoint_id,
       resume_request_version
  FROM run_waits
 WHERE id = $1
   AND environment_id = $2
   AND run_id = $3
   AND workspace_id = $4
   AND attempt_number = $5
   AND token_id = $6
   AND kind = 'token'
   AND (condition_state = 'pending' OR suspension_state = 'checkpointing')
 FOR UPDATE`,
		locator.waitID,
		environmentID,
		locator.runID,
		locator.workspaceID,
		locator.attempt,
		tokenID,
	).Scan(
		&wait.id,
		&wait.environmentID,
		&wait.runID,
		&wait.workspaceID,
		&wait.tokenID,
		&wait.kind,
		&wait.conditionState,
		&wait.suspensionState,
		&wait.expectedRunStateVersion,
		&wait.attemptNumber,
		&wait.currentRunLeaseID,
		&wait.priorRunLeaseID,
		&wait.suspendCheckpointID,
		&wait.resumeRequestVersion,
	)
	if err != nil {
		return tokenWaitLockedWait{}, err
	}
	return wait, nil
}

func validateLockedTokenWait(run tokenWaitLockedRun, wait tokenWaitLockedWait) error {
	if wait.kind != WaitKindToken ||
		wait.runID != run.id || wait.workspaceID != run.workspaceID ||
		wait.attemptNumber != run.currentAttempt || wait.expectedRunStateVersion != run.stateVersion ||
		run.status != RunStatusWaiting {
		return tokenWaitAuthorityError("Run and Token Wait fences do not match", nil)
	}
	switch wait.suspensionState {
	case RunWaitStateHot, RunWaitStateCheckpointing:
		if wait.conditionState != WaitStatePending && wait.suspensionState != RunWaitStateCheckpointing {
			return tokenWaitAuthorityError("terminal Token Wait is not awaiting checkpoint readiness", nil)
		}
		if !run.currentRunLeaseID.Valid || !wait.currentRunLeaseID.Valid ||
			run.currentRunLeaseID != wait.currentRunLeaseID || wait.priorRunLeaseID.Valid ||
			!run.activeStartedAt.Valid {
			return tokenWaitAuthorityError("hot Token Wait Lease fence does not match", nil)
		}
	case RunWaitStateParked:
		if wait.conditionState != WaitStatePending {
			return tokenWaitAuthorityError("parked Token Wait is already terminal", nil)
		}
		if run.currentRunLeaseID.Valid || wait.currentRunLeaseID.Valid ||
			!wait.priorRunLeaseID.Valid || !wait.suspendCheckpointID.Valid ||
			run.activeStartedAt.Valid {
			return tokenWaitAuthorityError("parked Token Wait provenance does not match", nil)
		}
	default:
		return tokenWaitAuthorityError("pending Token Wait suspension is not completable", nil)
	}
	return nil
}

func tokenWaitTerminalResolution(state TokenState, completionData []byte) (tokenWaitResolution, error) {
	switch state {
	case TokenStateCompleted:
		result := json.RawMessage(completionData)
		if len(result) == 0 {
			result = json.RawMessage(`null`)
		}
		return tokenWaitResolution{conditionState: WaitStateCompleted, result: result}, nil
	case TokenStateCancelled:
		reason := "token_cancelled"
		return tokenWaitResolution{
			conditionState: WaitStateCancelled,
			reasonCode:     &reason,
			conditionError: json.RawMessage(`{"code":"token_cancelled","retryable":false}`),
		}, nil
	case TokenStateExpired:
		reason := "token_expired"
		return tokenWaitResolution{
			conditionState: WaitStateFailed,
			reasonCode:     &reason,
			conditionError: json.RawMessage(`{"code":"token_expired","retryable":false}`),
		}, nil
	default:
		return tokenWaitResolution{}, tokenWaitAuthorityError("Token is not terminal", nil)
	}
}

func reconcileHotTokenWait(
	ctx context.Context,
	tx pgx.Tx,
	run tokenWaitLockedRun,
	wait tokenWaitLockedWait,
	resolution tokenWaitResolution,
) error {
	var nextVersion int64
	err := tx.QueryRow(ctx, `
UPDATE runs
   SET status = 'running',
       state_version = state_version + 1,
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND status = 'waiting'
   AND state_version = $2
   AND current_attempt_number = $3
   AND current_run_lease_id = $4
RETURNING state_version`,
		run.id,
		run.stateVersion,
		run.currentAttempt,
		run.currentRunLeaseID,
	).Scan(&nextVersion)
	if err != nil {
		return tokenWaitAuthorityError("resume hot Token Wait Run", err)
	}
	command, err := tx.Exec(ctx, `
UPDATE run_waits
   SET condition_state = $1,
       condition_result = $2::jsonb,
       condition_reason_code = $3,
       condition_error = $4::jsonb,
       condition_terminal_at = transaction_timestamp(),
       suspension_state = 'released',
       expected_run_state_version = $5,
       suspension_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = $6
   AND run_id = $7
   AND condition_state = 'pending'
   AND suspension_state = 'hot'
   AND expected_run_state_version = $8
   AND current_run_lease_id = $9`,
		resolution.conditionState,
		resolution.result,
		resolution.reasonCode,
		resolution.conditionError,
		nextVersion,
		wait.id,
		wait.runID,
		wait.expectedRunStateVersion,
		wait.currentRunLeaseID,
	)
	if err != nil || command.RowsAffected() != 1 {
		return tokenWaitAuthorityError("release hot Token Wait", err)
	}
	return nil
}

func reconcileCheckpointingTokenWait(
	ctx context.Context,
	tx pgx.Tx,
	wait tokenWaitLockedWait,
	resolution tokenWaitResolution,
) error {
	command, err := tx.Exec(ctx, `
UPDATE run_waits
   SET condition_state = $1,
       condition_result = $2::jsonb,
       condition_reason_code = $3,
       condition_error = $4::jsonb,
       condition_terminal_at = transaction_timestamp(),
       updated_at = transaction_timestamp()
 WHERE id = $5
   AND run_id = $6
   AND condition_state = 'pending'
   AND suspension_state = 'checkpointing'
   AND expected_run_state_version = $7
   AND current_run_lease_id = $8`,
		resolution.conditionState,
		resolution.result,
		resolution.reasonCode,
		resolution.conditionError,
		wait.id,
		wait.runID,
		wait.expectedRunStateVersion,
		wait.currentRunLeaseID,
	)
	if err != nil || command.RowsAffected() != 1 {
		return tokenWaitAuthorityError("complete checkpointing Token Wait", err)
	}
	return nil
}

func reconcileParkedTokenWait(
	ctx context.Context,
	tx pgx.Tx,
	run tokenWaitLockedRun,
	wait tokenWaitLockedWait,
	resolution tokenWaitResolution,
) error {
	nextResumeVersion := wait.resumeRequestVersion + 1
	var nextVersion int64
	err := tx.QueryRow(ctx, `
UPDATE runs
   SET status = 'queued',
       state_version = state_version + 1,
       updated_at = transaction_timestamp()
 WHERE id = $1
   AND status = 'waiting'
   AND state_version = $2
   AND current_attempt_number = $3
   AND current_run_lease_id IS NULL
RETURNING state_version`, run.id, run.stateVersion, run.currentAttempt).Scan(&nextVersion)
	if err != nil {
		return tokenWaitAuthorityError("queue parked Token Wait Run", err)
	}
	command, err := tx.Exec(ctx, `
UPDATE run_waits
   SET condition_state = $1,
       condition_result = $2::jsonb,
       condition_reason_code = $3,
       condition_error = $4::jsonb,
       condition_terminal_at = transaction_timestamp(),
       suspension_state = 'resume_pending',
       resume_request_version = $5,
       expected_run_state_version = $6,
       updated_at = transaction_timestamp()
 WHERE id = $7
   AND run_id = $8
   AND condition_state = 'pending'
   AND suspension_state = 'parked'
   AND expected_run_state_version = $9
   AND current_run_lease_id IS NULL
   AND prior_run_lease_id = $10
   AND suspend_checkpoint_id = $11`,
		resolution.conditionState,
		resolution.result,
		resolution.reasonCode,
		resolution.conditionError,
		nextResumeVersion,
		nextVersion,
		wait.id,
		wait.runID,
		wait.expectedRunStateVersion,
		wait.priorRunLeaseID,
		wait.suspendCheckpointID,
	)
	if err != nil || command.RowsAffected() != 1 {
		return tokenWaitAuthorityError("resume parked Token Wait", err)
	}
	command, err = tx.Exec(ctx, `
INSERT INTO outbox_messages (
    lane,
    topic,
    partition_key,
    payload,
    available_at
) VALUES (
    'control',
    'run.resume',
    $1::uuid::text,
    jsonb_build_object(
        'environmentId', $2::uuid::text,
        'runId', $3::uuid::text,
        'runWaitId', $4::uuid::text,
        'resumeRequestVersion', $5::bigint
    ),
    transaction_timestamp()
)`, wait.workspaceID, wait.environmentID, wait.runID, wait.id, nextResumeVersion)
	if err != nil || command.RowsAffected() != 1 {
		return tokenWaitAuthorityError("publish parked Token Wait resume intent", err)
	}
	return nil
}

func tokenWaitAuthorityError(operation string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrTokenWaitReconcileAuthority, operation)
	}
	return fmt.Errorf("%w: %s: %w", ErrTokenWaitReconcileAuthority, operation, cause)
}
