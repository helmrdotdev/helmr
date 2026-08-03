package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerNextRuntimeReconcileTarget(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerRuntimeReconcileRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid runtime reconcile request JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	row, err := s.db.GetNextRuntimeReconcileTarget(r.Context(), db.GetNextRuntimeReconcileTargetParams{
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, api.WorkerRuntimeReconcileResponse{})
		return
	}
	if err != nil {
		writeError(w, errors.New("get runtime reconcile target"))
		return
	}
	action := api.WorkerRuntimeReconcilePrepare
	switch {
	case row.ObservedState == db.RuntimeObservedStateFailed:
		action = api.WorkerRuntimeReconcileReclaim
	case row.DesiredState == db.RuntimeDesiredStateClosed:
		action = api.WorkerRuntimeReconcileClose
	}
	source := api.WorkerRuntimeSource{
		DeploymentDefinitionID: pgvalue.UUIDString(row.DeploymentDefinitionID),
		WorkspaceID:            pgvalue.UUIDString(row.WorkspaceID),
		RuntimeIdentityID:      row.RuntimeIdentityID,
		WorkspaceImage:         api.CASObject{Digest: row.WorkspaceImageDigest, SizeBytes: row.WorkspaceImageSizeBytes, MediaType: row.WorkspaceImageMediaType},
		WorkspaceArchitecture:  row.WorkspaceArchitecture,
		RootfsDigest:           row.RootfsDigest,
		ReservedCpuMillis:      int32(row.ReservedCpuMillis), ReservedMemoryMiB: int32(row.ReservedMemoryBytes / 1048576),
		ReservedDiskMiB: row.ReservedGuestEphemeralDiskBytes / 1048576, ReservedExecutionSlots: row.ReservedExecutionSlots,
		RuntimeABI: row.RuntimeABI,
	}
	if action == api.WorkerRuntimeReconcilePrepare {
		if err := populateRuntimePrepareSource(r.Context(), s.db, s.platformStore, &source, row); err != nil {
			writeError(w, err)
			return
		}
	}
	target := api.WorkerRuntimeReconcileTarget{
		ID: pgvalue.UUIDString(row.ID), WorkerEpoch: row.WorkerEpoch,
		DesiredState: string(row.DesiredState), DesiredVersion: row.DesiredVersion,
		ObservedState: string(row.ObservedState), ObservedVersion: row.ObservedVersion, ObservedDesiredVersion: row.ObservedDesiredVersion,
		Action: action, Source: source,
	}
	writeJSON(w, http.StatusOK, api.WorkerRuntimeReconcileResponse{Target: &target})
}

func populateRuntimePrepareSource(
	ctx context.Context,
	store db.Querier,
	platformStore cas.Reader,
	source *api.WorkerRuntimeSource,
	row db.GetNextRuntimeReconcileTargetRow,
) error {
	if !row.BaseWorkspaceVersionID.Valid || !row.WorkspaceEntryCount.Valid {
		return errors.New("runtime reservation has no exact Workspace version")
	}
	if row.WorkspaceArchitecture == "" {
		return errors.New("runtime reservation has no Workspace architecture")
	}
	source.BaseVersionID = pgvalue.UUIDString(row.BaseWorkspaceVersionID)
	source.WorkspaceArtifact = api.WorkerWorkspaceArtifact{
		Digest:     row.WorkspaceArtifactDigest,
		MediaType:  row.WorkspaceArtifactMediaType,
		SizeBytes:  row.WorkspaceArtifactSizeBytes,
		EntryCount: row.WorkspaceEntryCount.Int32,
	}
	if source.WorkspaceArtifact.Digest == "" {
		if source.WorkspaceArtifact.SizeBytes != 0 ||
			source.WorkspaceArtifact.MediaType != "" ||
			source.WorkspaceArtifact.EntryCount != 0 {
			return errors.New("runtime reservation has an invalid empty Workspace root")
		}
	} else {
		if source.WorkspaceArtifact.SizeBytes <= 0 ||
			source.WorkspaceArtifact.MediaType != workspace.ArtifactMediaType ||
			source.WorkspaceArtifact.EntryCount < 0 {
			return errors.New("runtime reservation has an invalid Workspace Artifact")
		}
		source.WorkspaceArtifact.Encoding = workspace.ArtifactEncoding
	}
	if !row.ProgramDeploymentID.Valid {
		if row.ReservedRunID.Valid {
			return errors.New("run runtime reservation has no Program deployment")
		}
		return nil
	}
	if !row.ProgramDeploymentAuthorityID.Valid ||
		row.ProgramDeploymentAuthorityID != row.ProgramDeploymentID ||
		!row.ProgramBuildContractVersion.Valid {
		return errors.New("runtime reservation Program authority is incomplete")
	}
	program, err := projectRuntimeProgram(
		ctx,
		runtimeProgramAuthorityFromDeployment(
			row.ProgramDeploymentID,
			row.ProgramRuntimeDigest,
			row.ProgramArtifactDigest,
			row.ProgramArtifactSizeBytes,
			row.ProgramArtifactMediaType,
			row.ProgramBuildContractVersion.String,
			row.ProgramIndexDigest,
		),
		row.WorkspaceArchitecture,
		platformStore,
	)
	if err != nil {
		return fmt.Errorf("project runtime reservation Program: %w", err)
	}
	source.Program = &program
	if row.RestoreCheckpointID.Valid {
		if !row.ReservedRunID.Valid || !row.ReservedAttemptNumber.Valid || store == nil {
			return errors.New("restored runtime reservation authority is incomplete")
		}
		checkpoint, err := store.GetReadyRunCheckpoint(ctx, db.GetReadyRunCheckpointParams{
			RunID: row.ReservedRunID, AttemptNumber: row.ReservedAttemptNumber.Int32,
			ID: row.RestoreCheckpointID,
		})
		if err != nil {
			return fmt.Errorf("load restored runtime Checkpoint authority: %w", err)
		}
		artifacts, err := store.ListRunCheckpointArtifactAuthority(ctx, row.RestoreCheckpointID)
		if err != nil {
			return fmt.Errorf("load restored runtime Checkpoint Artifacts: %w", err)
		}
		projected, err := projectRunLeaseCheckpoint(checkpoint, artifacts)
		if err != nil {
			return fmt.Errorf("project restored runtime Checkpoint: %w", err)
		}
		source.Restore = &api.WorkerRuntimeRestore{
			CheckpointID: pgvalue.UUIDString(row.RestoreCheckpointID),
			RunID:        pgvalue.UUIDString(checkpoint.RunID), AttemptNumber: checkpoint.AttemptNumber,
			RunWaitID: pgvalue.UUIDString(checkpoint.RunWaitID),
			Kind:      projected.Kind, Manifest: projected.Manifest, Artifacts: projected.Artifacts,
		}
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
	var request api.WorkerRuntimeInstanceStateRequest
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
		runtimeSubstrateID, substrateErr := ids.Parse(request.RuntimeSubstrateID)
		if substrateErr != nil {
			writeError(w, badRequest(errors.New("runtime_substrate_id must be a canonical UUIDv7")))
			return
		}
		row, err = s.db.MarkRuntimeInstanceReady(r.Context(), db.MarkRuntimeInstanceReadyParams{
			DesiredVersion: request.DesiredVersion, ID: pgvalue.UUID(id), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:             worker.WorkerEpoch,
			ExpectedObservedVersion: request.ExpectedObservedVersion, RuntimeSubstrateID: pgvalue.UUID(runtimeSubstrateID),
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
		row, err = s.db.MarkRuntimeInstanceFailed(r.Context(), db.MarkRuntimeInstanceFailedParams{
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

func validateRuntimeCleanupProof(proof api.WorkerRuntimeCleanupProof, now time.Time) error {
	switch proof.Method {
	case api.WorkerRuntimeCleanupSessionClosed, api.WorkerRuntimeCleanupHostReconciled, api.WorkerRuntimeCleanupNotMaterialized:
	default:
		return errors.New("runtime cleanup proof method is unsupported")
	}
	if proof.CompletedAt.IsZero() || proof.CompletedAt.After(now.Add(time.Minute)) {
		return errors.New("runtime cleanup proof completed_at is required and cannot be in the future")
	}
	return nil
}

func validateRuntimeClosedCleanupProof(proof api.WorkerRuntimeCleanupProof, now time.Time) error {
	if proof.Method != api.WorkerRuntimeCleanupSessionClosed && proof.Method != api.WorkerRuntimeCleanupHostReconciled {
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

func runtimeInstanceResponse(row db.RuntimeInstance) api.WorkerRuntimeInstance {
	return api.WorkerRuntimeInstance{
		ID:                     pgvalue.UUIDString(row.ID),
		OrgID:                  pgvalue.UUIDString(row.OrgID),
		ProjectID:              pgvalue.UUIDString(row.ProjectID),
		EnvironmentID:          pgvalue.UUIDString(row.EnvironmentID),
		WorkerInstanceID:       pgvalue.UUIDString(row.WorkerInstanceID),
		RuntimeEpoch:           row.WorkerEpoch,
		RuntimeID:              row.RuntimeIdentityID,
		DeploymentDefinitionID: pgvalue.UUIDString(row.DeploymentDefinitionID),
		State:                  string(row.ObservedState),
		ReservedCpuMillis:      int32(row.ReservedCpuMillis),
		ReservedMemoryMiB:      int32(row.ReservedMemoryBytes / 1048576),
		ReservedDiskMiB:        row.ReservedGuestEphemeralDiskBytes / 1048576,
		ReservedExecutionSlots: row.ReservedExecutionSlots,
	}
}
