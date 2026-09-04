package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	runauthority "github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const workerRuntimeReconcileLimit int32 = 64

func (s *Server) workerNextRuntimeReconcileTarget(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RuntimeReconcileRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid runtime reconcile request JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	rows, err := s.db.ListRuntimeReconcileTargets(r.Context(), db.ListRuntimeReconcileTargetsParams{
		WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
		ObservationFreshnessSeconds: workerapi.WorkerObservationFreshnessSeconds,
		RowLimit:                    workerRuntimeReconcileLimit,
	})
	if err != nil {
		writeError(w, errors.New("list runtime reconcile targets"))
		return
	}
	items := make([]workerapi.RuntimeReconcileTarget, 0, len(rows))
	for _, row := range rows {
		action := workerapi.RuntimeReconcilePrepare
		switch {
		case row.ObservedState == db.RuntimeObservedStateFailed:
			action = workerapi.RuntimeReconcileReclaim
		case row.DesiredState == db.RuntimeDesiredStateClosed:
			action = workerapi.RuntimeReconcileClose
		}
		source := workerapi.RuntimeSource{
			DeploymentDefinitionID: pgvalue.UUIDString(row.DeploymentDefinitionID),
			WorkspaceID:            pgvalue.UUIDString(row.WorkspaceID),
			RuntimeIdentityID:      row.RuntimeIdentityID,
			VMVCPUCount:            row.VMVCPUCount,
			CPUConfigDigest:        row.CPUConfigDigest,
			WorkspaceImage:         workerapi.CASObject{Digest: row.WorkspaceImageDigest, SizeBytes: row.WorkspaceImageSizeBytes, MediaType: row.WorkspaceImageMediaType},
			WorkspaceArchitecture:  row.WorkspaceArchitecture,
			RootfsDigest:           row.RootfsDigest,
			ReservedCPUMillis:      int32(row.ReservedCPUMillis), ReservedMemoryMiB: int32(row.ReservedMemoryBytes / 1048576),
			ReservedDiskMiB: row.ReservedGuestEphemeralDiskBytes / 1048576, ReservedExecutionSlots: row.ReservedExecutionSlots,
			VMRuntimeContract: row.VMRuntimeContract,
		}
		if action == workerapi.RuntimeReconcilePrepare {
			if err := populateRuntimePrepareSource(r.Context(), s.db, s.platformStore, &source, row); err != nil {
				writeError(w, err)
				return
			}
		}
		items = append(items, workerapi.RuntimeReconcileTarget{
			ID: pgvalue.UUIDString(row.ID), WorkerEpoch: row.WorkerEpoch,
			DesiredVersion: row.DesiredVersion, ObservedVersion: row.ObservedVersion,
			Action: action, Source: source,
		})
	}
	writeJSON(w, http.StatusOK, workerapi.RuntimeReconcileResponse{Items: items})
}

func populateRuntimePrepareSource(
	ctx context.Context,
	store db.Querier,
	platformStore cas.Reader,
	source *workerapi.RuntimeSource,
	row db.ListRuntimeReconcileTargetsRow,
) error {
	if !row.BaseWorkspaceVersionID.Valid || !row.WorkspaceContentDigest.Valid ||
		!row.WorkspaceLogicalSizeBytes.Valid || !row.WorkspaceEntryCount.Valid {
		return errors.New("runtime reservation has no exact workspace version")
	}
	if row.WorkspaceArchitecture == "" {
		return errors.New("runtime reservation has no workspace architecture")
	}
	source.WorkspaceTarget = &workerapi.WorkspaceResetTarget{
		BaseWorkspaceVersionID: pgvalue.UUIDString(row.BaseWorkspaceVersionID),
		Tree: workerapi.WorkspaceTreeIdentity{
			Digest: row.WorkspaceContentDigest.String, SizeBytes: row.WorkspaceLogicalSizeBytes.Int64,
			EntryCount: row.WorkspaceEntryCount.Int32,
		},
	}
	artifact := workerapi.WorkspaceArtifact{
		Digest:     row.WorkspaceArtifactDigest,
		MediaType:  row.WorkspaceArtifactMediaType,
		SizeBytes:  row.WorkspaceArtifactSizeBytes,
		EntryCount: row.WorkspaceEntryCount.Int32,
	}
	if artifact.Digest == "" {
		if artifact.SizeBytes != 0 || artifact.MediaType != "" || artifact.EntryCount != 0 ||
			source.WorkspaceTarget.Tree.Digest != workspace.CanonicalEmptyTreeDigest ||
			source.WorkspaceTarget.Tree.SizeBytes != 0 || source.WorkspaceTarget.Tree.EntryCount != 0 {
			return errors.New("runtime reservation has an invalid empty workspace root")
		}
		source.WorkspaceTarget.Empty = &workerapi.EmptyWorkspace{}
	} else {
		if artifact.SizeBytes <= 0 || artifact.MediaType != workspace.ArtifactMediaType || artifact.EntryCount < 0 {
			return errors.New("runtime reservation has an invalid workspace artifact")
		}
		artifact.Encoding = workspace.ArtifactEncoding
		source.WorkspaceTarget.Artifact = &artifact
	}
	if !row.ProgramDeploymentID.Valid {
		if row.ReservedRunID.Valid {
			return errors.New("run runtime reservation has no program deployment")
		}
		return nil
	}
	if !row.ProgramDeploymentAuthorityID.Valid ||
		row.ProgramDeploymentAuthorityID != row.ProgramDeploymentID ||
		!row.ProgramRuntimeDigest.Valid {
		return errors.New("runtime reservation program authority is incomplete")
	}
	program, err := projectRuntimeProgram(
		ctx,
		runtimeProgramAuthorityFromDeployment(
			row.ProgramDeploymentID,
			row.ProgramRuntimeDigest.String,
			row.ProgramArtifactDigest,
			row.ProgramArtifactSizeBytes,
			row.ProgramArtifactMediaType,
			row.ProgramIndexDigest,
		),
		row.WorkspaceArchitecture,
		platformStore,
	)
	if err != nil {
		return fmt.Errorf("project runtime reservation program: %w", err)
	}
	source.Program = &program
	if row.RestoreCheckpointID.Valid {
		if err := populateRuntimeRestoreSource(ctx, store, source, row); err != nil {
			return err
		}
	}
	return nil
}

func populateRuntimeRestoreSource(
	ctx context.Context,
	store db.Querier,
	source *workerapi.RuntimeSource,
	row db.ListRuntimeReconcileTargetsRow,
) error {
	if !row.RestoreCheckpointID.Valid || !row.ReservedRunID.Valid ||
		!row.ReservedAttemptNumber.Valid || store == nil {
		return errors.New("restored runtime reservation authority is incomplete")
	}
	checkpointRow, err := store.GetReadyRunCheckpoint(ctx, db.GetReadyRunCheckpointParams{
		RunID: row.ReservedRunID, AttemptNumber: row.ReservedAttemptNumber.Int32,
		ID: row.RestoreCheckpointID,
	})
	if err != nil {
		return fmt.Errorf("load restored runtime checkpoint authority: %w", err)
	}
	checkpoint := checkpointRow.RunCheckpoint
	projected, err := projectRunLeaseCheckpoint(
		checkpoint,
		checkpointArtifactAuthorityFromReady(checkpointRow),
	)
	if err != nil {
		return fmt.Errorf("project restored runtime checkpoint: %w", err)
	}
	source.Restore = &workerapi.RuntimeRestore{
		CheckpointID: pgvalue.UUIDString(row.RestoreCheckpointID),
		RunID:        pgvalue.UUIDString(checkpoint.RunID), AttemptNumber: checkpoint.AttemptNumber,
		RunWaitID: pgvalue.UUIDString(checkpoint.RunWaitID),
		Manifest:  projected.Manifest, Artifacts: projected.Artifacts,
	}
	return nil
}

func (s *Server) workerMarkRuntimeInstanceReady(w http.ResponseWriter, r *http.Request) {
	s.workerMarkRuntimeInstance(w, r, "ready")
}
func (s *Server) workerMarkRuntimeInstanceClosed(w http.ResponseWriter, r *http.Request) {
	s.workerMarkRuntimeInstance(w, r, "closed")
}
func (s *Server) workerMarkRuntimeInstanceFailed(w http.ResponseWriter, r *http.Request) {
	s.workerMarkRuntimeInstance(w, r, "failed")
}

func (s *Server) workerMarkRuntimeInstance(w http.ResponseWriter, r *http.Request, state string) {
	var request workerapi.RuntimeInstanceStateRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker runtime instance %s request JSON: %w", state, err)))
		return
	}
	id, err := ids.Parse(request.ID)
	if err != nil {
		writeError(w, badRequest(errors.New("id must be a canonical UUIDv7")))
		return
	}
	if request.WorkerEpoch <= 0 || request.DesiredVersion <= 0 || request.ExpectedObservedVersion < 0 {
		writeError(w, badRequest(errors.New("runtime epoch, desired version, and observed version fences are required")))
		return
	}
	worker := workerFromContext(r.Context())
	if request.WorkerEpoch != worker.WorkerEpoch {
		writeError(w, forbidden(errors.New("runtime instance belongs to another worker epoch")))
		return
	}
	var row db.RuntimeInstance
	switch state {
	case "ready":
		if request.VMVCPUCount <= 0 {
			writeError(w, badRequest(errors.New("vm_vcpu_count must be positive")))
			return
		}
		if !sha256sum.ValidDigest(request.CPUConfigDigest) {
			writeError(w, badRequest(errors.New("cpu_config_digest must be a canonical SHA-256 digest")))
			return
		}
		runtimeSubstrateID, substrateErr := ids.Parse(request.RuntimeSubstrateID)
		if substrateErr != nil {
			writeError(w, badRequest(errors.New("runtime_substrate_id must be a canonical UUIDv7")))
			return
		}
		row, err = s.db.MarkRuntimeInstanceReady(r.Context(), db.MarkRuntimeInstanceReadyParams{
			DesiredVersion: request.DesiredVersion, ID: pgvalue.UUID(id), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:             worker.WorkerEpoch,
			ExpectedObservedVersion: request.ExpectedObservedVersion, RuntimeSubstrateID: pgvalue.UUID(runtimeSubstrateID),
			VMVCPUCount: request.VMVCPUCount, CPUConfigDigest: request.CPUConfigDigest,
		})
	case "closed":
		if request.CleanupProof == nil {
			writeError(w, badRequest(errors.New("runtime cleanup proof is required when marking a runtime closed")))
			return
		}
		if proofErr := validateRuntimeClosedCleanupProof(*request.CleanupProof, time.Now()); proofErr != nil {
			writeError(w, badRequest(proofErr))
			return
		}
		proof, proofErr := json.Marshal(request.CleanupProof)
		if proofErr != nil {
			writeError(w, badRequest(errors.New("encode runtime cleanup proof")))
			return
		}
		reason := strings.TrimSpace(request.ReasonCode)
		if reason == "" {
			reason = "desired_state_reconciled"
		}
		row, err = s.db.MarkRuntimeInstanceClosed(r.Context(), db.MarkRuntimeInstanceClosedParams{
			ReasonCode: pgtype.Text{String: reason, Valid: true}, ID: pgvalue.UUID(id), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
			DesiredVersion:          request.DesiredVersion,
			ExpectedObservedVersion: request.ExpectedObservedVersion,
			CleanupProof:            proof,
		})
	case "failed":
		reason := strings.TrimSpace(request.ReasonCode)
		if reason == "" {
			reason = "runtime_reconcile_failed"
		}
		if request.CleanupProof != nil {
			if proofErr := validateRuntimeCleanupProof(*request.CleanupProof, time.Now()); proofErr != nil {
				writeError(w, badRequest(proofErr))
				return
			}
			proof, proofErr := json.Marshal(request.CleanupProof)
			if proofErr != nil {
				writeError(w, badRequest(errors.New("encode runtime cleanup proof")))
				return
			}
			reclaimed, reclaimErr := s.db.ReclaimFailedRuntimeInstance(r.Context(), db.ReclaimFailedRuntimeInstanceParams{
				ID: pgvalue.UUID(id), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
				DesiredVersion: request.DesiredVersion, ExpectedObservedVersion: request.ExpectedObservedVersion,
				CleanupProof: proof,
			})
			if reclaimErr == nil {
				writeJSON(w, http.StatusOK, runtimeInstanceResponse(db.RuntimeInstance(reclaimed)))
				return
			}
			if !errors.Is(reclaimErr, pgx.ErrNoRows) {
				writeError(w, errors.New("reclaim failed runtime instance"))
				return
			}
		}
		row, err = s.markRuntimeInstanceFailed(r.Context(), worker.WorkerGroupID, db.MarkRuntimeInstanceFailedParams{
			ReasonCode: pgtype.Text{String: reason, Valid: true}, Error: normalizedJSONRawMessage(request.Error),
			ID: pgvalue.UUID(id), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
			DesiredVersion:          request.DesiredVersion,
			ExpectedObservedVersion: request.ExpectedObservedVersion,
		})
		if err == nil && request.CleanupProof != nil {
			proof, _ := json.Marshal(request.CleanupProof)
			row, err = s.db.ReclaimFailedRuntimeInstance(r.Context(), db.ReclaimFailedRuntimeInstanceParams{
				ID: pgvalue.UUID(id), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
				DesiredVersion: request.DesiredVersion, ExpectedObservedVersion: row.ObservedVersion,
				CleanupProof: proof,
			})
		}
	default:
		writeError(w, errors.New("unsupported runtime instance state"))
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(errors.New("runtime instance fence is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("mark runtime instance "+state))
		return
	}
	writeJSON(w, http.StatusOK, runtimeInstanceResponse(row))
}

func (s *Server) markRuntimeInstanceFailed(
	ctx context.Context,
	workerGroupID uuid.UUID,
	params db.MarkRuntimeInstanceFailedParams,
) (db.RuntimeInstance, error) {
	workerFatal := params.ReasonCode.Valid &&
		params.ReasonCode.String == workerapi.RuntimeFailureWorkerInvalid
	authorityParams := db.GetRuntimePreparationFailureAuthorityParams{
		ID: params.ID, WorkerInstanceID: params.WorkerInstanceID,
		WorkerEpoch: params.WorkerEpoch, DesiredVersion: params.DesiredVersion,
		ExpectedObservedVersion: params.ExpectedObservedVersion,
	}
	discovered, err := s.db.GetRuntimePreparationFailureAuthority(ctx, authorityParams)
	if err != nil {
		if workerFatal && errors.Is(err, pgx.ErrNoRows) {
			if fenceErr := s.fenceInvalidWorkerEpoch(
				ctx, workerGroupID, params.WorkerInstanceID, params.WorkerEpoch,
			); fenceErr != nil {
				return db.RuntimeInstance{}, fenceErr
			}
		}
		return db.RuntimeInstance{}, err
	}
	if discovered.WorkerGroupID != pgvalue.UUID(workerGroupID) {
		return db.RuntimeInstance{}, errors.New("runtime preparation Worker Group authority changed")
	}
	if !discovered.ReservedRunID.Valid && !workerFatal {
		return s.db.MarkRuntimeInstanceFailed(ctx, params)
	}
	if discovered.ReservedRunID.Valid &&
		(!discovered.ReservedAttemptNumber.Valid || !discovered.RunAuthorityValid) &&
		!workerFatal {
		return db.RuntimeInstance{}, errors.New("runtime preparation authority is incomplete")
	}
	if s.tx == nil {
		return db.RuntimeInstance{}, errors.New("runtime preparation transaction authority is unavailable")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return db.RuntimeInstance{}, fmt.Errorf("begin runtime preparation failure: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	queries := db.New(tx)
	var worker db.WorkerInstance
	lockWorkerSupply := func() error {
		group, err := queries.LockWorkerGroupForPoolMutation(ctx, discovered.WorkerGroupID)
		if err != nil {
			return fmt.Errorf("lock runtime preparation Worker Group: %w", err)
		}
		if group.State != db.WorkerGroupStateActive && group.State != db.WorkerGroupStatePaused &&
			group.State != db.WorkerGroupStateDraining {
			return errors.New("runtime preparation Worker Group is inactive")
		}
		pool, err := queries.LockWorkerPool(ctx, db.LockWorkerPoolParams{
			WorkerGroupID: discovered.WorkerGroupID,
			WorkerPoolID:  discovered.WorkerPoolID,
		})
		if err != nil {
			return fmt.Errorf("lock runtime preparation Worker Pool: %w", err)
		}
		if pool.State != "active" && pool.State != "draining" {
			return errors.New("runtime preparation Worker Pool is inactive")
		}
		worker, err = queries.LockWorkerInstanceForActivation(
			ctx,
			db.LockWorkerInstanceForActivationParams{
				WorkerInstanceID: params.WorkerInstanceID,
				WorkerGroupID:    discovered.WorkerGroupID,
				WorkerPoolID:     discovered.WorkerPoolID,
				WorkerEpoch:      pgtype.Int8{Int64: params.WorkerEpoch, Valid: true},
			},
		)
		if err != nil {
			return fmt.Errorf("lock runtime preparation Worker epoch: %w", err)
		}
		if worker.State != db.WorkerInstanceStateActive && worker.State != db.WorkerInstanceStateDraining {
			return errors.New("runtime preparation Worker epoch is inactive")
		}
		return nil
	}
	var graph runauthority.OwnedFinalization
	if discovered.ReservedRunID.Valid {
		graph, err = runauthority.LockOwnedFinalizationWithRuntimeFence(
			ctx,
			tx,
			runauthority.OwnedFinalizationRequest{
				OrgID:         uuid.UUID(discovered.OrgID.Bytes),
				ProjectID:     uuid.UUID(discovered.ProjectID.Bytes),
				EnvironmentID: uuid.UUID(discovered.EnvironmentID.Bytes),
				RunID:         uuid.UUID(discovered.ReservedRunID.Bytes),
			},
			lockWorkerSupply,
		)
		if err != nil {
			return db.RuntimeInstance{}, fmt.Errorf("lock runtime preparation run graph: %w", err)
		}
	} else if err := lockWorkerSupply(); err != nil {
		return db.RuntimeInstance{}, err
	}
	locked, err := queries.LockRuntimePreparationFailureAuthority(
		ctx,
		db.LockRuntimePreparationFailureAuthorityParams(authorityParams),
	)
	if err != nil {
		return db.RuntimeInstance{}, err
	}
	if locked.ReservedRunID != discovered.ReservedRunID ||
		locked.ReservedAttemptNumber != discovered.ReservedAttemptNumber ||
		locked.WorkerGroupID != discovered.WorkerGroupID ||
		locked.WorkerPoolID != discovered.WorkerPoolID ||
		locked.OrgID != discovered.OrgID || locked.ProjectID != discovered.ProjectID ||
		locked.EnvironmentID != discovered.EnvironmentID {
		return db.RuntimeInstance{}, errors.New("runtime preparation authority changed while locking")
	}
	if locked.ReservedRunID.Valid && !locked.RunAuthorityValid && !workerFatal {
		return db.RuntimeInstance{}, errors.New("runtime preparation authority is stale")
	}
	row, err := queries.MarkRuntimeInstanceFailed(ctx, params)
	if err != nil {
		return db.RuntimeInstance{}, err
	}
	if locked.ReservedRunID.Valid && locked.RunAuthorityValid {
		if _, err := graph.ChargeRuntimePreparationFailure(ctx); err != nil {
			return db.RuntimeInstance{}, err
		}
	}
	if workerFatal && worker.State == db.WorkerInstanceStateActive {
		drained, err := queries.DrainWorkerInstance(ctx, db.DrainWorkerInstanceParams{
			ID:                   params.WorkerInstanceID,
			WorkerGroupID:        discovered.WorkerGroupID,
			ExpectedEpoch:        pgtype.Int8{Int64: params.WorkerEpoch, Valid: true},
			ExpectedClaimVersion: worker.ClaimVersion,
		})
		if err != nil {
			return db.RuntimeInstance{}, fmt.Errorf("fence invalid Worker runtime epoch: %w", err)
		}
		if drained.State != db.WorkerInstanceStateDraining {
			return db.RuntimeInstance{}, errors.New("invalid Worker runtime epoch was not fenced")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return db.RuntimeInstance{}, fmt.Errorf("commit runtime preparation failure: %w", err)
	}
	return row, nil
}

func (s *Server) fenceInvalidWorkerEpoch(
	ctx context.Context,
	workerGroupID uuid.UUID,
	workerInstanceID pgtype.UUID,
	workerEpoch int64,
) error {
	if s.tx == nil {
		return errors.New("invalid Worker epoch transaction authority is unavailable")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin invalid Worker epoch fence: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	queries := db.New(tx)
	poolID, err := queries.GetWorkerInstancePoolID(ctx, db.GetWorkerInstancePoolIDParams{
		WorkerInstanceID: workerInstanceID,
		WorkerGroupID:    pgvalue.UUID(workerGroupID),
		WorkerEpoch:      pgtype.Int8{Int64: workerEpoch, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("resolve invalid Worker epoch Pool: %w", err)
	}
	group, err := queries.LockWorkerGroupForPoolMutation(ctx, pgvalue.UUID(workerGroupID))
	if err != nil {
		return fmt.Errorf("lock invalid Worker epoch Group: %w", err)
	}
	if group.State != db.WorkerGroupStateActive && group.State != db.WorkerGroupStatePaused &&
		group.State != db.WorkerGroupStateDraining {
		return errors.New("invalid Worker epoch Group is inactive")
	}
	pool, err := queries.LockWorkerPool(ctx, db.LockWorkerPoolParams{
		WorkerGroupID: pgvalue.UUID(workerGroupID),
		WorkerPoolID:  poolID,
	})
	if err != nil {
		return fmt.Errorf("lock invalid Worker epoch Pool: %w", err)
	}
	if pool.State != "active" && pool.State != "draining" {
		return errors.New("invalid Worker epoch Pool is inactive")
	}
	worker, err := queries.LockWorkerInstanceForActivation(ctx, db.LockWorkerInstanceForActivationParams{
		WorkerInstanceID: workerInstanceID,
		WorkerGroupID:    pgvalue.UUID(workerGroupID),
		WorkerPoolID:     poolID,
		WorkerEpoch:      pgtype.Int8{Int64: workerEpoch, Valid: true},
	})
	if err != nil {
		return fmt.Errorf("lock invalid Worker epoch: %w", err)
	}
	if worker.State != db.WorkerInstanceStateActive && worker.State != db.WorkerInstanceStateDraining {
		return errors.New("invalid Worker epoch is inactive")
	}
	if worker.State == db.WorkerInstanceStateActive {
		if _, err := queries.DrainWorkerInstance(ctx, db.DrainWorkerInstanceParams{
			ID:                   workerInstanceID,
			WorkerGroupID:        pgvalue.UUID(workerGroupID),
			ExpectedEpoch:        pgtype.Int8{Int64: workerEpoch, Valid: true},
			ExpectedClaimVersion: worker.ClaimVersion,
		}); err != nil {
			return fmt.Errorf("fence invalid Worker epoch: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit invalid Worker epoch fence: %w", err)
	}
	return nil
}

func validateRuntimeCleanupProof(proof workerapi.RuntimeCleanupProof, now time.Time) error {
	switch proof.Method {
	case workerapi.RuntimeCleanupSessionClosed, workerapi.RuntimeCleanupHostReconciled, workerapi.RuntimeCleanupNotMaterialized:
	default:
		return errors.New("runtime cleanup proof method is unsupported")
	}
	if proof.CompletedAt.IsZero() || proof.CompletedAt.After(now.Add(time.Minute)) {
		return errors.New("runtime cleanup proof completed_at is required and cannot be in the future")
	}
	return nil
}

func validateRuntimeClosedCleanupProof(proof workerapi.RuntimeCleanupProof, now time.Time) error {
	if proof.Method != workerapi.RuntimeCleanupSessionClosed && proof.Method != workerapi.RuntimeCleanupHostReconciled {
		return errors.New("closed runtime cleanup proof must confirm a closed session or exact host reconciliation")
	}
	return validateRuntimeCleanupProof(proof, now)
}

func normalizedJSONRawMessage(raw json.RawMessage) []byte {
	if strings.TrimSpace(string(raw)) == "" {
		return []byte(`{}`)
	}
	return []byte(raw)
}

func runtimeInstanceResponse(row db.RuntimeInstance) workerapi.RuntimeInstance {
	return workerapi.RuntimeInstance{
		ID:                     pgvalue.UUIDString(row.ID),
		OrgID:                  pgvalue.UUIDString(row.OrgID),
		ProjectID:              pgvalue.UUIDString(row.ProjectID),
		EnvironmentID:          pgvalue.UUIDString(row.EnvironmentID),
		WorkerInstanceID:       pgvalue.UUIDString(row.WorkerInstanceID),
		RuntimeEpoch:           row.WorkerEpoch,
		RuntimeID:              row.RuntimeIdentityID,
		VMVCPUCount:            row.VMVCPUCount,
		CPUConfigDigest:        row.CPUConfigDigest,
		DeploymentDefinitionID: pgvalue.UUIDString(row.DeploymentDefinitionID),
		State:                  string(row.ObservedState),
		ReservedCPUMillis:      int32(row.ReservedCPUMillis),
		ReservedMemoryMiB:      int32(row.ReservedMemoryBytes / 1048576),
		ReservedDiskMiB:        row.ReservedGuestEphemeralDiskBytes / 1048576,
		ReservedExecutionSlots: row.ReservedExecutionSlots,
	}
}
