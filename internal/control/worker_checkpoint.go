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
	"github.com/helmrdotdev/helmr/internal/workspace"
)

const (
	defaultLiveWaitCheckpointDelay = 5 * time.Second
	shortTimerLiveMaxDuration      = 5 * time.Second
	shortTimerCheckpointGrace      = 1 * time.Second
	interactiveLiveWaitDelay       = 2 * time.Minute
)

func (s *Server) workerClaimRuntimeCheckpointWait(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerCheckpointClaimRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker checkpoint claim request JSON: %w", err)))
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
	scope, err := s.db.GetWorkerRunWaitScope(r.Context(), db.GetWorkerRunWaitScopeParams{
		OrgID:            pgvalue.UUID(orgID),
		RunID:            pgvalue.UUID(runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
	})
	if isNoRows(err) {
		stale, staleErr := s.writeStaleCheckpointCommandIfAdvanced(r.Context(), w, orgID, runID, runLeaseID, worker.WorkerInstanceID, runWaitID)
		if staleErr != nil {
			writeError(w, errors.New("load stale run wait checkpoint claim"))
			return
		}
		if stale {
			return
		}
		writeError(w, conflict(errors.New("worker run lease is not active")))
		return
	}
	if err != nil {
		writeError(w, errors.New("load worker run wait scope"))
		return
	}
	claim, err := s.db.ClaimRuntimeCheckpointWait(r.Context(), db.ClaimRuntimeCheckpointWaitParams{
		RuntimeCheckpointID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:               scope.OrgID,
		ProjectID:           scope.ProjectID,
		EnvironmentID:       scope.EnvironmentID,
		RunID:               pgvalue.UUID(runID),
		RunWaitID:           pgvalue.UUID(runWaitID),
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(worker.WorkerInstanceID),
	})
	if isNoRows(err) {
		stale, staleErr := s.writeStaleCheckpointCommandIfAdvanced(r.Context(), w, orgID, runID, runLeaseID, worker.WorkerInstanceID, runWaitID)
		if staleErr != nil {
			writeError(w, errors.New("load stale run wait checkpoint claim"))
			return
		}
		if stale {
			return
		}
		writeError(w, conflict(errors.New("run wait checkpoint claim lost")))
		return
	}
	if err != nil {
		writeError(w, errors.New("claim run wait checkpoint"))
		return
	}
	response := api.WorkerCheckpointClaimResponse{
		RunID:            runID.String(),
		RunWaitID:        runWaitID.String(),
		Status:           "claimed",
		CheckpointID:     pgvalue.MustUUIDValue(claim.RuntimeCheckpointID).String(),
		CaptureWorkspace: claim.DirtyGeneration > 0,
	}
	if claim.WorkspaceVersionID.Valid {
		response.WorkspaceVersionID = pgvalue.MustUUIDValue(claim.WorkspaceVersionID).String()
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) writeStaleCheckpointCommandIfAdvanced(ctx context.Context, w http.ResponseWriter, orgID uuid.UUID, runID uuid.UUID, runLeaseID uuid.UUID, workerInstanceID uuid.UUID, runWaitID uuid.UUID) (bool, error) {
	runWait, err := s.db.GetRunWaitByRun(ctx, db.GetRunWaitByRunParams{
		OrgID: pgvalue.UUID(orgID),
		RunID: pgvalue.UUID(runID),
		ID:    pgvalue.UUID(runWaitID),
	})
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if runWait.OwnerRunLeaseID != pgvalue.UUID(runLeaseID) ||
		runWait.OwnerWorkerInstanceID != pgvalue.UUID(workerInstanceID) ||
		!isStaleCheckpointCommandState(runWait.State) {
		return false, nil
	}
	writeJSON(w, http.StatusOK, api.WorkerCheckpointClaimResponse{
		RunID:     runID.String(),
		RunWaitID: runWaitID.String(),
		Status:    "stale",
	})
	return true, nil
}

func isStaleCheckpointCommandState(state db.RunWaitState) bool {
	switch state {
	case db.RunWaitStateCheckpointedWaiting,
		db.RunWaitStateResolvedLive,
		db.RunWaitStateResolvedCheckpointed,
		db.RunWaitStateExpired,
		db.RunWaitStateResuming,
		db.RunWaitStateResumed,
		db.RunWaitStateCancelled,
		db.RunWaitStateFailed:
		return true
	default:
		return false
	}
}

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
	if err := validateWorkerWorkspaceCapture(request.WorkspaceCapture); err != nil {
		writeError(w, badRequest(err))
		return
	}
	var response api.WorkerRunWaitWorkspaceCaptureResponse
	err = s.inTx(r.Context(), func(work *txWork) error {
		scope, err := work.q.GetWorkerRunWaitScope(r.Context(), db.GetWorkerRunWaitScopeParams{
			OrgID:            pgvalue.UUID(orgID),
			RunID:            pgvalue.UUID(runID),
			RunLeaseID:       pgvalue.UUID(runLeaseID),
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		})
		if isNoRows(err) {
			return conflict(errors.New("worker run lease is not active"))
		}
		if err != nil {
			return errors.New("load worker run wait scope")
		}
		runWait, err := work.q.GetRunWait(r.Context(), db.GetRunWaitParams{
			OrgID:         scope.OrgID,
			ProjectID:     scope.ProjectID,
			EnvironmentID: scope.EnvironmentID,
			ID:            pgvalue.UUID(runWaitID),
		})
		if isNoRows(err) {
			return notFound(errors.New("run wait not found"))
		}
		if err != nil {
			return errors.New("load run wait")
		}
		if pgvalue.MustUUIDValue(runWait.RunID) != runID || runWait.State != db.RunWaitStateCheckpointing {
			return conflict(errors.New("run wait is not checkpointing for this run"))
		}
		if runWait.WorkspaceVersionID.Valid {
			response = api.WorkerRunWaitWorkspaceCaptureResponse{
				RunID:              runID.String(),
				RunWaitID:          strings.TrimSpace(request.RunWaitID),
				CheckpointID:       strings.TrimSpace(request.CheckpointID),
				WorkspaceVersionID: pgvalue.MustUUIDValue(runWait.WorkspaceVersionID).String(),
			}
			return nil
		}
		capture := request.WorkspaceCapture
		if _, err := work.q.UpsertCasObject(r.Context(), db.UpsertCasObjectParams{
			Digest:    strings.TrimSpace(capture.Digest),
			SizeBytes: capture.SizeBytes,
			MediaType: strings.TrimSpace(capture.MediaType),
		}); err != nil {
			return errors.New("record run wait workspace capture CAS object")
		}
		artifact, err := work.q.CreateArtifact(r.Context(), db.CreateArtifactParams{
			ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:                     scope.OrgID,
			ProjectID:                 scope.ProjectID,
			EnvironmentID:             scope.EnvironmentID,
			Digest:                    strings.TrimSpace(capture.Digest),
			Kind:                      db.ArtifactKindWorkspaceVersion,
			SizeBytes:                 capture.SizeBytes,
			MediaType:                 strings.TrimSpace(capture.MediaType),
			CreatedByWorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		})
		if err != nil {
			return errors.New("record run wait workspace capture artifact")
		}
		version, err := work.q.PromoteWorkspaceCapture(r.Context(), db.PromoteWorkspaceCaptureParams{
			OrgID:              scope.OrgID,
			WriteLeaseID:       scope.WorkspaceLeaseID,
			FencingToken:       scope.WorkspaceFencingToken,
			DirtyGeneration:    scope.DirtyGeneration,
			ArtifactID:         artifact.ID,
			SizeBytes:          capture.SizeBytes,
			ArtifactEncoding:   strings.TrimSpace(capture.Encoding),
			ContentDigest:      strings.TrimSpace(capture.Digest),
			VersionID:          pgvalue.UUID(uuid.Must(uuid.NewV7())),
			Kind:               db.WorkspaceVersionKindSystem,
			ArtifactEntryCount: capture.EntryCount,
			Message:            "system capture before parked wait",
		})
		if isNoRows(err) {
			return conflict(codedError{code: "workspace_capture_rejected", message: "workspace capture is stale"})
		}
		if err != nil {
			return errors.New("promote run wait workspace capture")
		}
		if _, err := work.q.SetRunWaitWorkspaceVersion(r.Context(), db.SetRunWaitWorkspaceVersionParams{
			OrgID:              scope.OrgID,
			ProjectID:          scope.ProjectID,
			EnvironmentID:      scope.EnvironmentID,
			ID:                 pgvalue.UUID(runWaitID),
			RunID:              scope.RunID,
			WorkspaceVersionID: version.ID,
		}); err != nil {
			return errors.New("record run wait workspace version")
		}
		response = api.WorkerRunWaitWorkspaceCaptureResponse{
			RunID:              runID.String(),
			RunWaitID:          strings.TrimSpace(request.RunWaitID),
			CheckpointID:       strings.TrimSpace(request.CheckpointID),
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
		writeError(w, badRequest(fmt.Errorf("invalid worker runtime checkpoint ready request JSON: %w", err)))
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
	runtimeCheckpointID, err := uuid.Parse(strings.TrimSpace(request.CheckpointID))
	if err != nil {
		writeError(w, badRequest(errors.New("checkpoint_id must be a UUID")))
		return
	}
	if request.WorkerCommandID <= 0 {
		writeError(w, badRequest(errors.New("worker_command_id must be positive")))
		return
	}
	if err := validateWorkerCheckpointManifest(request.Manifest); err != nil {
		writeError(w, badRequest(err))
		return
	}
	runtimeSubstrateArtifactID, err := checkpointRuntimeSubstrateArtifactID(request.Manifest)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	errReadyCheckpointReplay := errors.New("ready runtime checkpoint replay")
	replayConflict := conflict(errors.New("runtime checkpoint cannot be marked ready for this wait"))
	err = s.inTx(r.Context(), func(work *txWork) error {
		scope, err := work.q.GetWorkerRunWaitScope(r.Context(), db.GetWorkerRunWaitScopeParams{
			OrgID:            pgvalue.UUID(orgID),
			RunID:            pgvalue.UUID(runID),
			RunLeaseID:       pgvalue.UUID(runLeaseID),
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		})
		if isNoRows(err) {
			replayConflict = conflict(errors.New("worker run lease is not active"))
			return errReadyCheckpointReplay
		}
		if err != nil {
			return errors.New("load worker run wait scope")
		}
		artifactIDs, err := s.createRuntimeCheckpointArtifacts(r.Context(), work.q, pgvalue.UUID(worker.WorkerInstanceID), scope, request.Manifest)
		if err != nil {
			return errors.New("create runtime checkpoint artifacts")
		}
		manifest, err := json.Marshal(request.Manifest)
		if err != nil {
			return errors.New("encode runtime checkpoint manifest")
		}
		created, err := work.q.CreateReadyRuntimeCheckpointForRunWait(r.Context(), db.CreateReadyRuntimeCheckpointForRunWaitParams{
			WorkerCommandID:            request.WorkerCommandID,
			OrgID:                      scope.OrgID,
			ProjectID:                  scope.ProjectID,
			EnvironmentID:              scope.EnvironmentID,
			RunWaitID:                  pgvalue.UUID(runWaitID),
			RunID:                      pgvalue.UUID(runID),
			RunLeaseID:                 pgvalue.UUID(runLeaseID),
			WorkerInstanceID:           pgvalue.UUID(worker.WorkerInstanceID),
			RuntimeCheckpointID:        pgvalue.UUID(runtimeCheckpointID),
			RuntimeBackend:             request.Manifest.RecoveryPoint.Runtime.Backend,
			RuntimeID:                  request.Manifest.RecoveryPoint.Runtime.ID,
			RuntimeArch:                request.Manifest.RecoveryPoint.Runtime.Arch,
			RuntimeABI:                 request.Manifest.RecoveryPoint.Runtime.ABI,
			KernelDigest:               request.Manifest.RecoveryPoint.Runtime.KernelDigest,
			InitramfsDigest:            request.Manifest.RecoveryPoint.Runtime.InitramfsDigest,
			RootfsDigest:               request.Manifest.RecoveryPoint.Runtime.RootfsDigest,
			RuntimeConfigDigest:        request.Manifest.RecoveryPoint.Runtime.ConfigDigest,
			RuntimeSubstrateArtifactID: runtimeSubstrateArtifactID,
			CniProfile:                 scope.WorkerCniProfile,
			SubstrateDigest:            checkpointSubstrateDigest(request.Manifest),
			Manifest:                   manifest,
		})
		if isNoRows(err) {
			return errReadyCheckpointReplay
		}
		if err != nil {
			s.log.Error("mark runtime checkpoint ready failed",
				"error", err,
				"worker_instance_id", worker.WorkerInstanceID,
				"run_id", runID,
				"run_wait_id", runWaitID,
				"run_lease_id", runLeaseID,
				"runtime_checkpoint_id", runtimeCheckpointID,
			)
			return errors.New("mark runtime checkpoint ready")
		}
		if err := s.createRuntimeCheckpointArtifactRows(r.Context(), work.q, scope, created.ID, request.Manifest, artifactIDs); err != nil {
			s.log.Error("create runtime checkpoint artifact rows failed",
				"error", err,
				"worker_instance_id", worker.WorkerInstanceID,
				"run_id", runID,
				"run_wait_id", runWaitID,
				"runtime_checkpoint_id", runtimeCheckpointID,
			)
			return errors.New("create runtime checkpoint artifact rows")
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
		if err := acknowledgeCheckpointWorkerCommand(r.Context(), work.q, scope, request.WorkerCommandID, runID, runWaitID, runLeaseID, worker.WorkerInstanceID); err != nil {
			return err
		}
		work.AfterCommit(func(ctx context.Context) error {
			s.requeueResolvedRunWaits(ctx, scope.OrgID)
			return nil
		})
		return nil
	})
	if errors.Is(err, errReadyCheckpointReplay) {
		replayed, replayErr := s.writeAcknowledgedReadyRuntimeCheckpointReplay(r.Context(), w, s.db, orgID, runID, runLeaseID, worker.WorkerInstanceID, runWaitID, runtimeCheckpointID, request.WorkerCommandID)
		if replayErr != nil {
			writeError(w, errors.New("load acknowledged ready runtime checkpoint replay"))
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
		CheckpointID: runtimeCheckpointID.String(),
	})
}

func (s *Server) writeAcknowledgedReadyRuntimeCheckpointReplay(ctx context.Context, w http.ResponseWriter, store db.Querier, orgID uuid.UUID, runID uuid.UUID, runLeaseID uuid.UUID, workerInstanceID uuid.UUID, runWaitID uuid.UUID, runtimeCheckpointID uuid.UUID, workerCommandID int64) (bool, error) {
	if workerCommandID <= 0 {
		return false, nil
	}
	_, err := store.GetAcknowledgedReadyRuntimeCheckpointForRunWait(ctx, db.GetAcknowledgedReadyRuntimeCheckpointForRunWaitParams{
		OrgID:               pgvalue.UUID(orgID),
		RunID:               pgvalue.UUID(runID),
		RuntimeCheckpointID: pgvalue.UUID(runtimeCheckpointID),
		RunWaitID:           pgvalue.UUID(runWaitID),
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerInstanceID),
		WorkerCommandID:     workerCommandID,
	})
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	writeJSON(w, http.StatusOK, api.WorkerCheckpointResponse{
		RunID:        runID.String(),
		RunWaitID:    runWaitID.String(),
		CheckpointID: runtimeCheckpointID.String(),
	})
	return true, nil
}

func acknowledgeCheckpointWorkerCommand(ctx context.Context, store db.Querier, scope db.GetWorkerRunWaitScopeRow, commandID int64, runID uuid.UUID, runWaitID uuid.UUID, runLeaseID uuid.UUID, workerInstanceID uuid.UUID) error {
	_, err := store.AcknowledgeWorkerCommandForRunWait(ctx, db.AcknowledgeWorkerCommandForRunWaitParams{
		WorkerInstanceID: pgvalue.UUID(workerInstanceID),
		ID:               commandID,
		OrgID:            scope.OrgID,
		RunID:            pgvalue.UUID(runID),
		RunWaitID:        pgvalue.UUID(runWaitID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		Kind:             db.WorkerCommandKindRuntimeCheckpointWait,
	})
	if isNoRows(err) {
		return conflict(errors.New("worker checkpoint command is not active for this run wait"))
	}
	if err != nil {
		return errors.New("acknowledge checkpoint worker command")
	}
	return nil
}

func (s *Server) workerMarkCheckpointFailed(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerCheckpointFailedRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker runtime checkpoint failed request JSON: %w", err)))
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
	runtimeCheckpointID, err := uuid.Parse(strings.TrimSpace(request.CheckpointID))
	if err != nil {
		writeError(w, badRequest(errors.New("checkpoint_id must be a UUID")))
		return
	}
	if request.WorkerCommandID <= 0 {
		writeError(w, badRequest(errors.New("worker_command_id must be positive")))
		return
	}
	err = s.inTx(r.Context(), func(work *txWork) error {
		scope, err := work.q.GetWorkerRunWaitScope(r.Context(), db.GetWorkerRunWaitScopeParams{
			OrgID:            pgvalue.UUID(orgID),
			RunID:            pgvalue.UUID(runID),
			RunLeaseID:       pgvalue.UUID(runLeaseID),
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		})
		if isNoRows(err) {
			return conflict(errors.New("worker run lease is not active"))
		}
		if err != nil {
			return errors.New("load worker run wait scope")
		}
		if _, err := work.q.FailRuntimeCheckpointAttempt(r.Context(), db.FailRuntimeCheckpointAttemptParams{
			OrgID:               scope.OrgID,
			ProjectID:           scope.ProjectID,
			EnvironmentID:       scope.EnvironmentID,
			RunID:               pgvalue.UUID(runID),
			RunWaitID:           pgvalue.UUID(runWaitID),
			RunLeaseID:          pgvalue.UUID(runLeaseID),
			WorkerInstanceID:    pgvalue.UUID(worker.WorkerInstanceID),
			RuntimeCheckpointID: pgvalue.UUID(runtimeCheckpointID),
			WorkerCommandID:     request.WorkerCommandID,
			ErrorMessage:        strings.TrimSpace(request.Error),
		}); isNoRows(err) {
			return conflict(errors.New("run wait is not parking for this run lease"))
		} else if err != nil {
			return errors.New("mark runtime checkpoint attempt failed")
		}
		return acknowledgeCheckpointWorkerCommand(r.Context(), work.q, scope, request.WorkerCommandID, runID, runWaitID, runLeaseID, worker.WorkerInstanceID)
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
	runtimeCheckpointID, err := uuid.Parse(strings.TrimSpace(request.CheckpointID))
	if err != nil {
		writeError(w, badRequest(errors.New("checkpoint_id must be a UUID")))
		return
	}
	orgID, runID, runLeaseID, err := workerWaitLeaseIDs(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	workerLeaseIDs := workerRunLeaseIDs{
		orgID:           orgID,
		runID:           runID,
		runLeaseID:      runLeaseID,
		protocolVersion: strings.TrimSpace(request.Lease.ProtocolVersion),
		attemptNumber:   request.Lease.AttemptNumber,
		queueMessageID:  strings.TrimSpace(request.Lease.DispatchMessageID),
		queueLeaseID:    strings.TrimSpace(request.Lease.DispatchLeaseID),
	}
	if _, err := s.workerCurrentRunningLease(r.Context(), worker, workerLeaseIDs); isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is not active")))
		return
	} else if err != nil {
		writeError(w, errors.New("load worker restore lease"))
		return
	}
	restorePhases, err := json.Marshal(request.Phases)
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid restore phases: %w", err)))
		return
	}
	wait, err := s.db.MarkRuntimeResumeWaitResumed(r.Context(), db.MarkRuntimeResumeWaitResumedParams{
		OrgID:               pgvalue.UUID(orgID),
		ID:                  pgvalue.UUID(runWaitID),
		RunID:               pgvalue.UUID(runID),
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		RuntimeCheckpointID: pgvalue.UUID(runtimeCheckpointID),
		RestorePhases:       restorePhases,
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
		if manifest.RuntimeState.RuntimeSubstrateArtifact == nil {
			return errors.New("runtime_state.runtime_substrate_artifact is required")
		}
		if err := validateWorkerRuntimeSubstrateArtifact("runtime_state.runtime_substrate_artifact", *manifest.RuntimeState.RuntimeSubstrateArtifact, *runtime.Substrate); err != nil {
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
