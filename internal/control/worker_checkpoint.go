package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultLiveWaitCheckpointDelay = 5 * time.Second
	shortTimerLiveMaxDuration      = 5 * time.Second
	shortTimerCheckpointGrace      = 1 * time.Second
	interactiveLiveWaitDelay       = 2 * time.Minute
)

func (s *Server) workerCaptureRunWaitWorkspace(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerRunWaitWorkspaceCaptureRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker run wait workspace capture request JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, forbidden(errors.New("worker lease does not belong to this worker")))
		return
	}
	orgID, runID, runLeaseID, err := workerWaitLeaseIDs(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	runWaitID, err := uuid.Parse(strings.TrimSpace(request.RunWaitID))
	if err != nil {
		writeError(w, badRequest(errors.New("run_wait_id must be a UUID")))
		return
	}
	checkpointID, err := uuid.Parse(strings.TrimSpace(request.CheckpointID))
	if err != nil {
		writeError(w, badRequest(errors.New("checkpoint_id must be a UUID")))
		return
	}
	if request.RequestVersion <= 0 {
		writeError(w, badRequest(errors.New("request_version must be positive")))
		return
	}
	if err := validateWorkerWorkspaceCapture(request.WorkspaceCapture); err != nil {
		writeError(w, badRequest(err))
		return
	}
	workspaceID, workspaceMountID, writeLeaseID, err := checkpointWorkspaceFence(request.Workspace)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var response api.WorkerRunWaitWorkspaceCaptureResponse
	err = s.inTx(r.Context(), func(work *txWork) error {
		scope, err := work.q.GetWorkerRunWaitScope(r.Context(), db.GetWorkerRunWaitScopeParams{
			OrgID: pgvalue.UUID(orgID), RunWaitID: pgvalue.UUID(runWaitID),
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
		})
		if isNoRows(err) {
			return conflict(errors.New("worker run lease is not active"))
		}
		if err != nil {
			return errors.New("load worker run wait scope")
		}
		if scope.RunID != pgvalue.UUID(runID) || scope.CurrentRunLeaseID != pgvalue.UUID(runLeaseID) ||
			scope.State != db.RunWaitStateCheckpointing || scope.CheckpointRequestVersion != request.RequestVersion {
			return conflict(errors.New("run wait checkpoint request is stale"))
		}
		capture := request.WorkspaceCapture
		if _, err := work.q.UpsertCasObject(r.Context(), db.UpsertCasObjectParams{
			OrgID: scope.OrgID, Digest: strings.TrimSpace(capture.Digest),
			SizeBytes: capture.SizeBytes, MediaType: strings.TrimSpace(capture.MediaType),
		}); err != nil {
			return errors.New("record run wait workspace capture CAS object")
		}
		artifact, err := work.q.CreateArtifact(r.Context(), db.CreateArtifactParams{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), OrgID: scope.OrgID,
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
			Digest: strings.TrimSpace(capture.Digest), Kind: db.ArtifactKindWorkspaceVersion,
			SizeBytes: capture.SizeBytes, MediaType: strings.TrimSpace(capture.MediaType),
			CreatedByWorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		})
		if err != nil {
			return errors.New("record run wait workspace capture artifact")
		}
		var versionPublicID string
		version, err := createWithPublicID(r.Context(), []publicIDSlot{{prefix: publicid.WorkspaceVersion, value: &versionPublicID}}, func() (db.CreateAndPromoteWorkspaceCaptureRow, error) {
			return work.q.CreateAndPromoteWorkspaceCapture(r.Context(), db.CreateAndPromoteWorkspaceCaptureParams{
				OrgID: scope.OrgID, WorkspaceID: workspaceID, WriteLeaseID: writeLeaseID,
				WorkspaceMountID: workspaceMountID, RunID: pgvalue.UUID(runID),
				RunWaitID: pgvalue.UUID(runWaitID), CheckpointRequestVersion: request.RequestVersion,
				CheckpointAttemptID: pgvalue.UUID(checkpointID),
				WorkerInstanceID:    pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
				FencingToken:       strings.TrimSpace(request.Workspace.WriteFencingToken),
				FencingGeneration:  request.Workspace.FencingGeneration,
				WorkspaceVersionID: pgvalue.UUID(uuid.Must(uuid.NewV7())), WorkspaceVersionPublicID: versionPublicID,
				ArtifactID: artifact.ID, ArtifactEncoding: strings.TrimSpace(capture.Encoding),
				ArtifactEntryCount: capture.EntryCount, ContentDigest: strings.TrimSpace(capture.Digest), SizeBytes: capture.SizeBytes,
			})
		})
		if isNoRows(err) {
			return conflict(codedError{code: "workspace_capture_rejected", message: "workspace capture is stale"})
		}
		if err != nil {
			return errors.New("promote run wait workspace capture")
		}
		response = api.WorkerRunWaitWorkspaceCaptureResponse{
			RunID: runID.String(), RunWaitID: runWaitID.String(), CheckpointID: strings.TrimSpace(request.CheckpointID),
			WorkspaceVersionID: pgvalue.MustUUIDValue(version.ID).String(),
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func checkpointWorkspaceFence(value api.WorkerWorkspace) (pgtype.UUID, pgtype.UUID, pgtype.UUID, error) {
	workspaceID, err := parseRequiredWorkspaceUUID("workspace.id", value.ID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	mountID, err := parseRequiredWorkspaceUUID("workspace.workspace_mount_id", value.WorkspaceMountID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	leaseID, err := parseRequiredWorkspaceUUID("workspace.write_lease_id", value.WriteLeaseID)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	if value.FencingGeneration <= 0 || strings.TrimSpace(value.WriteFencingToken) == "" {
		return pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, errors.New("workspace fencing generation and token are required")
	}
	return workspaceID, mountID, leaseID, nil
}

func validateWorkerWorkspaceCapture(capture api.WorkerWorkspaceArtifact) error {
	if strings.TrimSpace(capture.Digest) == "" {
		return errors.New("workspace_capture.digest is required")
	}
	if strings.TrimSpace(capture.MediaType) != workspace.ArtifactMediaType {
		return errors.New("workspace_capture.media_type is unsupported")
	}
	if strings.TrimSpace(capture.Encoding) != workspace.ArtifactEncoding {
		return errors.New("workspace_capture.encoding is unsupported")
	}
	if capture.SizeBytes <= 0 {
		return errors.New("workspace_capture.size_bytes must be positive")
	}
	if capture.SizeBytes > workspace.MaxArtifactArchiveBytes {
		return fmt.Errorf("workspace_capture.size_bytes exceeds max %d", workspace.MaxArtifactArchiveBytes)
	}
	if capture.EntryCount < 0 {
		return errors.New("workspace_capture.entry_count must be non-negative")
	}
	if capture.EntryCount > workspace.MaxArtifactEntries {
		return fmt.Errorf("workspace_capture.entry_count exceeds max %d", workspace.MaxArtifactEntries)
	}
	return nil
}

func (s *Server) workerMarkCheckpointReady(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerCheckpointReadyRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker run checkpoint ready request JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, forbidden(errors.New("worker lease does not belong to this worker")))
		return
	}
	orgID, runID, runLeaseID, err := workerWaitLeaseIDs(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	runWaitID, err := uuid.Parse(strings.TrimSpace(request.RunWaitID))
	if err != nil {
		writeError(w, badRequest(errors.New("run_wait_id must be a UUID")))
		return
	}
	runCheckpointID, err := uuid.Parse(strings.TrimSpace(request.CheckpointID))
	if err != nil {
		writeError(w, badRequest(errors.New("checkpoint_id must be a UUID")))
		return
	}
	if request.RequestVersion <= 0 {
		writeError(w, badRequest(errors.New("request_version must be positive")))
		return
	}
	workspaceID, workspaceMountID, writeLeaseID, err := checkpointWorkspaceFence(request.Workspace)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	workspaceVersionID, err := parseRequiredWorkspaceUUID("workspace_version_id", request.WorkspaceVersionID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := validateWorkerCheckpointManifest(request.Manifest); err != nil {
		writeError(w, badRequest(err))
		return
	}
	runtimeSubstrateID, err := checkpointRuntimeSubstrateID(request.Manifest)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	errReadyCheckpointReplay := errors.New("ready run checkpoint replay")
	replayConflict := conflict(errors.New("run checkpoint cannot be marked ready for this wait"))
	err = s.inTx(r.Context(), func(work *txWork) error {
		scope, err := work.q.GetWorkerRunWaitScope(r.Context(), db.GetWorkerRunWaitScopeParams{
			OrgID:            pgvalue.UUID(orgID),
			RunWaitID:        pgvalue.UUID(runWaitID),
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:      worker.WorkerEpoch,
		})
		if isNoRows(err) {
			replayConflict = conflict(errors.New("worker run lease is not active"))
			return errReadyCheckpointReplay
		}
		if err != nil {
			return errors.New("load worker run wait scope")
		}
		if scope.RunID != pgvalue.UUID(runID) || scope.CurrentRunLeaseID != pgvalue.UUID(runLeaseID) ||
			scope.State != db.RunWaitStateCheckpointing || scope.CheckpointRequestVersion != request.RequestVersion {
			return errReadyCheckpointReplay
		}
		artifactIDs, err := s.createRunCheckpointArtifacts(r.Context(), work.q, pgvalue.UUID(worker.WorkerInstanceID), scope, request.Manifest)
		if err != nil {
			return errors.New("create run checkpoint artifacts")
		}
		manifest, err := json.Marshal(request.Manifest)
		if err != nil {
			return errors.New("encode run checkpoint manifest")
		}
		created, err := work.q.CreateReadyRunCheckpointForRunWait(r.Context(), db.CreateReadyRunCheckpointForRunWaitParams{
			ID:                       pgvalue.UUID(runCheckpointID),
			SourceWorkspaceLeaseID:   writeLeaseID,
			WorkspaceMountID:         workspaceMountID,
			BaseWorkspaceVersionID:   workspaceVersionID,
			RuntimeBackend:           request.Manifest.RecoveryPoint.Runtime.Backend,
			RuntimeIdentityID:        scope.RuntimeIdentityID,
			RuntimeArch:              request.Manifest.RecoveryPoint.Runtime.Arch,
			RuntimeABI:               request.Manifest.RecoveryPoint.Runtime.ABI,
			KernelDigest:             request.Manifest.RecoveryPoint.Runtime.KernelDigest,
			InitramfsDigest:          request.Manifest.RecoveryPoint.Runtime.InitramfsDigest,
			RootfsDigest:             request.Manifest.RecoveryPoint.Runtime.RootfsDigest,
			RuntimeConfigDigest:      request.Manifest.RecoveryPoint.Runtime.ConfigDigest,
			RuntimeSubstrateID:       runtimeSubstrateID,
			RuntimeVcpus:             pgtype.Int4{Int32: int32((scope.RequestedMilliCpu + 999) / 1000), Valid: true},
			RuntimeMemoryMib:         pgtype.Int4{Int32: int32(scope.RequestedMemoryMib), Valid: true},
			RuntimeScratchDiskMib:    pgtype.Int4{Int32: int32(scope.RequestedDiskMib), Valid: scope.RequestedDiskMib > 0},
			CniProfile:               scope.CniProfile,
			SubstrateDigest:          checkpointSubstrateDigest(request.Manifest),
			Manifest:                 manifest,
			OrgID:                    scope.OrgID,
			RunID:                    pgvalue.UUID(runID),
			RunWaitID:                pgvalue.UUID(runWaitID),
			CheckpointRequestVersion: request.RequestVersion,
		})
		if isNoRows(err) {
			return errReadyCheckpointReplay
		}
		if err != nil {
			s.log.Error("mark run checkpoint ready failed",
				"error", err,
				"worker_instance_id", worker.WorkerInstanceID,
				"run_id", runID,
				"run_wait_id", runWaitID,
				"run_lease_id", runLeaseID,
				"run_checkpoint_id", runCheckpointID,
			)
			return errors.New("mark run checkpoint ready")
		}
		if err := s.createRunCheckpointArtifactRows(r.Context(), work.q, scope, created.ID, request.Manifest, artifactIDs); err != nil {
			s.log.Error("create run checkpoint artifact rows failed",
				"error", err,
				"worker_instance_id", worker.WorkerInstanceID,
				"run_id", runID,
				"run_wait_id", runWaitID,
				"run_checkpoint_id", runCheckpointID,
			)
			return errors.New("create run checkpoint artifact rows")
		}
		if _, err := work.q.SetRunWaitWorkspaceVersion(r.Context(), db.SetRunWaitWorkspaceVersionParams{
			OrgID: scope.OrgID, RunID: pgvalue.UUID(runID), RunWaitID: pgvalue.UUID(runWaitID),
			RunLeaseID: pgvalue.UUID(runLeaseID), CheckpointRequestVersion: request.RequestVersion,
			RunCheckpointID: created.ID, ReservedWorkspaceID: workspaceID,
			ReservedWorkspaceVersionID: workspaceVersionID,
			ActiveElapsedMsAtPark:      pgtype.Int8{Int64: request.ActiveDurationMs, Valid: true},
		}); isNoRows(err) {
			return errReadyCheckpointReplay
		} else if err != nil {
			return errors.New("commit run checkpoint")
		}
		if err := s.resolveReadyRunWait(r.Context(), work.q, scope, pgvalue.UUID(runWaitID)); err != nil {
			s.log.Error("resolve ready run wait failed",
				"error", err,
				"worker_instance_id", worker.WorkerInstanceID,
				"run_id", runID,
				"run_wait_id", runWaitID,
			)
			return errors.New("resolve ready run wait")
		}
		work.AfterCommit(func(ctx context.Context) {
			s.requeueResolvedRunWaits(ctx, scope.OrgID)
		})
		return nil
	})
	if errors.Is(err, errReadyCheckpointReplay) {
		replayed, replayErr := s.writeAcknowledgedReadyRunCheckpointReplay(r.Context(), w, s.db, orgID, runID, runLeaseID, worker.WorkerInstanceID, runWaitID, runCheckpointID, request.RequestVersion)
		if replayErr != nil {
			writeError(w, errors.New("load acknowledged ready run checkpoint replay"))
			return
		}
		if replayed {
			return
		}
		writeError(w, replayConflict)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerCheckpointResponse{
		RunID:        runID.String(),
		RunWaitID:    strings.TrimSpace(request.RunWaitID),
		CheckpointID: runCheckpointID.String(),
	})
}

func (s *Server) writeAcknowledgedReadyRunCheckpointReplay(ctx context.Context, w http.ResponseWriter, store db.Querier, orgID uuid.UUID, runID uuid.UUID, runLeaseID uuid.UUID, workerInstanceID uuid.UUID, runWaitID uuid.UUID, runCheckpointID uuid.UUID, requestVersion int64) (bool, error) {
	if requestVersion <= 0 {
		return false, nil
	}
	checkpoint, err := store.GetAcknowledgedReadyRunCheckpointForRunWait(ctx, db.GetAcknowledgedReadyRunCheckpointForRunWaitParams{
		OrgID: pgvalue.UUID(orgID), RunWaitID: pgvalue.UUID(runWaitID),
	})
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if checkpoint.RunID != pgvalue.UUID(runID) || checkpoint.ID != pgvalue.UUID(runCheckpointID) ||
		checkpoint.SourceRunLeaseID != pgvalue.UUID(runLeaseID) || checkpoint.SourceWorkerInstanceID != pgvalue.UUID(workerInstanceID) {
		return false, nil
	}
	writeJSON(w, http.StatusOK, api.WorkerCheckpointResponse{
		RunID:        runID.String(),
		RunWaitID:    runWaitID.String(),
		CheckpointID: runCheckpointID.String(),
	})
	return true, nil
}

func (s *Server) workerMarkCheckpointFailed(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerCheckpointFailedRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker run checkpoint failed request JSON: %w", err)))
		return
	}
	if request.ActiveDurationMs < 0 {
		writeError(w, badRequest(errors.New("active_duration_ms must be non-negative")))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, forbidden(errors.New("worker lease does not belong to this worker")))
		return
	}
	orgID, runID, runLeaseID, err := workerWaitLeaseIDs(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	runWaitID, err := uuid.Parse(strings.TrimSpace(request.RunWaitID))
	if err != nil {
		writeError(w, badRequest(errors.New("run_wait_id must be a UUID")))
		return
	}
	runCheckpointID, err := uuid.Parse(strings.TrimSpace(request.CheckpointID))
	if err != nil {
		writeError(w, badRequest(errors.New("checkpoint_id must be a UUID")))
		return
	}
	if request.RequestVersion <= 0 {
		writeError(w, badRequest(errors.New("request_version must be positive")))
		return
	}
	errorMessage := strings.TrimSpace(request.Error)
	if errorMessage == "" {
		errorMessage = "worker checkpoint failed"
	}
	errorPayload, err := json.Marshal(map[string]string{"message": errorMessage})
	if err != nil {
		writeError(w, errors.New("encode checkpoint failure"))
		return
	}
	err = s.inTx(r.Context(), func(work *txWork) error {
		scope, err := work.q.GetWorkerRunWaitScope(r.Context(), db.GetWorkerRunWaitScopeParams{
			OrgID:            pgvalue.UUID(orgID),
			RunWaitID:        pgvalue.UUID(runWaitID),
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:      worker.WorkerEpoch,
		})
		if isNoRows(err) {
			return conflict(errors.New("worker run lease is not active"))
		}
		if err != nil {
			return errors.New("load worker run wait scope")
		}
		if scope.RunID != pgvalue.UUID(runID) || scope.CurrentRunLeaseID != pgvalue.UUID(runLeaseID) ||
			scope.CheckpointRequestVersion != request.RequestVersion {
			return conflict(errors.New("run wait checkpoint request is stale"))
		}
		if _, err := work.q.FailRunCheckpointAttempt(r.Context(), db.FailRunCheckpointAttemptParams{
			ReasonCode: pgvalue.Text("checkpoint_failed"), Error: errorPayload,
			OrgID: scope.OrgID, RunID: pgvalue.UUID(runID), RunWaitID: pgvalue.UUID(runWaitID),
			RunLeaseID: pgvalue.UUID(runLeaseID), CheckpointRequestVersion: request.RequestVersion,
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
			RunCheckpointID: pgvalue.UUID(runCheckpointID), ErrorMessage: pgvalue.Text(errorMessage),
			ActiveDurationMs: request.ActiveDurationMs,
		}); isNoRows(err) {
			return conflict(errors.New("run wait is not parking for this run lease"))
		} else if err != nil {
			return errors.New("mark run checkpoint attempt failed")
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerCheckpointResponse{
		RunID:        runID.String(),
		RunWaitID:    strings.TrimSpace(request.RunWaitID),
		CheckpointID: strings.TrimSpace(request.CheckpointID),
	})
}

func (s *Server) workerAcknowledgeRestore(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerAcknowledgeRestoreRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker restore ack request JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, forbidden(errors.New("worker lease does not belong to this worker")))
		return
	}
	runWaitID, err := uuid.Parse(strings.TrimSpace(request.RunWaitID))
	if err != nil {
		writeError(w, badRequest(errors.New("run_wait_id must be a UUID")))
		return
	}
	runCheckpointID, err := uuid.Parse(strings.TrimSpace(request.CheckpointID))
	if err != nil {
		writeError(w, badRequest(errors.New("checkpoint_id must be a UUID")))
		return
	}
	leaseIDs, err := parseWorkerRunLease(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := s.workerCurrentRunningLease(r.Context(), worker, leaseIDs); isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is not active")))
		return
	} else if err != nil {
		writeError(w, errors.New("load worker restore lease"))
		return
	}
	if request.ResumeRequestVersion <= 0 {
		writeError(w, badRequest(errors.New("resume_request_version must be positive")))
		return
	}
	wait, err := s.db.MarkRunResumeWaitResumed(r.Context(), db.MarkRunResumeWaitResumedParams{
		ResumeRequestVersion: request.ResumeRequestVersion,
		RunCheckpointID:      pgvalue.UUID(runCheckpointID),
		OrgID:                pgvalue.UUID(leaseIDs.orgID), RunID: pgvalue.UUID(leaseIDs.runID),
		RunWaitID: pgvalue.UUID(runWaitID), RunLeaseID: pgvalue.UUID(leaseIDs.runLeaseID),
	})
	if err != nil && !isNoRows(err) {
		writeError(w, errors.New("acknowledge run wait restore"))
		return
	}
	if isNoRows(err) {
		writeError(w, conflict(errors.New("run wait is not ready for restore ack")))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerAcknowledgeRestoreResponse{
		RunID:        pgvalue.MustUUIDValue(wait.RunID).String(),
		RunWaitID:    strings.TrimSpace(request.RunWaitID),
		CheckpointID: strings.TrimSpace(request.CheckpointID),
	})
}

func validateWorkerCheckpointManifest(manifest api.WorkerCheckpointManifest) error {
	runtime := manifest.RecoveryPoint.Runtime
	required := map[string]string{
		"runtime.backend":          runtime.Backend,
		"runtime.id":               runtime.ID,
		"runtime.arch":             runtime.Arch,
		"runtime.abi":              runtime.ABI,
		"runtime.kernel_digest":    runtime.KernelDigest,
		"runtime.initramfs_digest": runtime.InitramfsDigest,
		"runtime.rootfs_digest":    runtime.RootfsDigest,
		"runtime.config_digest":    runtime.ConfigDigest,
	}
	for label, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", label)
		}
	}
	if runtime.Substrate != nil {
		substrateRequired := map[string]string{
			"runtime.substrate.digest":      runtime.Substrate.Digest,
			"runtime.substrate.format":      runtime.Substrate.Format,
			"runtime.substrate.builder_abi": runtime.Substrate.BuilderABI,
			"runtime.substrate.layout_abi":  runtime.Substrate.LayoutABI,
		}
		for label, value := range substrateRequired {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s is required", label)
			}
		}
		if manifest.RuntimeState.RuntimeSubstrate == nil {
			return errors.New("runtime_state.runtime_substrate is required")
		}
		if err := validateWorkerRuntimeSubstrate("runtime_state.runtime_substrate", *manifest.RuntimeState.RuntimeSubstrate, *runtime.Substrate); err != nil {
			return err
		}
	}
	for label, artifact := range map[string]api.WorkerCheckpointArtifact{
		"runtime_state.config_artifact":       manifest.RuntimeState.ConfigArtifact,
		"runtime_state.vm_state_artifact":     manifest.RuntimeState.VMStateArtifact,
		"runtime_state.scratch_disk_artifact": manifest.RuntimeState.ScratchDiskArtifact,
	} {
		if err := validateWorkerCheckpointArtifact(label, artifact); err != nil {
			return err
		}
	}
	for index, artifact := range manifest.RuntimeState.MemoryArtifacts {
		if err := validateWorkerCheckpointArtifact(fmt.Sprintf("runtime_state.memory_artifacts[%d]", index), artifact); err != nil {
			return err
		}
	}
	return nil
}
