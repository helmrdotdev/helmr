package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/jackc/pgx/v5/pgtype"
)

const checkpointRuntimeBackendFirecracker = "firecracker"

func (s *Server) workerAcknowledgeRestore(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerAcknowledgeRestoreRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker restore ack request JSON: %w", err)))
		return
	}
	worker, leaseIDs, ok := s.workerRunLeaseForWrite(w, r, request.Lease)
	if !ok {
		return
	}
	runWaitID, err := ids.Parse(request.RunWaitID)
	if err != nil {
		writeError(w, badRequest(errors.New("run_wait_id must be a UUID")))
		return
	}
	waitpointID, err := ids.Parse(request.WaitpointID)
	if err != nil {
		writeError(w, badRequest(errors.New("waitpoint_id must be a UUID")))
		return
	}
	checkpointID, err := ids.Parse(request.CheckpointID)
	if err != nil {
		writeError(w, badRequest(errors.New("checkpoint_id must be a UUID")))
		return
	}
	waitpoint, err := s.db.AcknowledgeRestore(r.Context(), db.AcknowledgeRestoreParams{
		OrgID:            ids.ToPG(leaseIDs.orgID),
		RunID:            ids.ToPG(leaseIDs.runID),
		SessionID:        ids.ToPG(leaseIDs.sessionID),
		WorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
		CheckpointID:     ids.ToPG(checkpointID),
		RunWaitID:        ids.ToPG(runWaitID),
		WaitpointID:      ids.ToPG(waitpointID),
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker restore acknowledgement is stale")))
		return
	}
	if err != nil {
		s.log.Error("acknowledge restore failed", "run_id", request.Lease.RunID, "checkpoint_id", request.CheckpointID, "waitpoint_id", request.WaitpointID, "error", err)
		writeError(w, errors.New("acknowledge restore"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerAcknowledgeRestoreResponse{
		RunID:        request.Lease.RunID,
		RunWaitID:    ids.MustFromPG(waitpoint.RunWaitID).String(),
		WaitpointID:  ids.MustFromPG(waitpoint.ID).String(),
		CheckpointID: ids.MustFromPG(waitpoint.CheckpointID).String(),
	})
}

func (s *Server) workerCheckpointReady(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	if s.dispatchQueue == nil {
		writeError(w, unavailable(errors.New("run queue item queue is not configured")))
		return
	}
	var request api.WorkerCheckpointReadyRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker checkpoint ready request JSON: %w", err)))
		return
	}
	leaseIDs, err := parseWorkerRunLease(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, forbidden(errors.New("worker run lease belongs to another worker")))
		return
	}
	runWaitID, err := ids.Parse(request.RunWaitID)
	if err != nil {
		writeError(w, badRequest(errors.New("run_wait_id must be a UUID")))
		return
	}
	waitpointID, err := ids.Parse(request.WaitpointID)
	if err != nil {
		writeError(w, badRequest(errors.New("waitpoint_id must be a UUID")))
		return
	}
	checkpointID, err := ids.Parse(request.CheckpointID)
	if err != nil {
		writeError(w, badRequest(errors.New("checkpoint_id must be a UUID")))
		return
	}
	params, err := checkpointReadyParams(leaseIDs.orgID, leaseIDs, worker.WorkerInstanceID, runWaitID, waitpointID, checkpointID, request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	_, lease, err := s.workerExecutionLease(r.Context(), worker, leaseIDs)
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease or checkpoint is stale")))
		return
	}
	if err != nil {
		s.log.Error("worker queue lease lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("get queue lease"))
		return
	}
	runtimeRelease, err := s.db.GetRunExecutionSessionRuntimeRelease(r.Context(), db.GetRunExecutionSessionRuntimeReleaseParams{
		OrgID:            ids.ToPG(leaseIDs.orgID),
		RunID:            ids.ToPG(leaseIDs.runID),
		SessionID:        ids.ToPG(leaseIDs.sessionID),
		WorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
	})
	if isNoRows(err) {
		s.log.Warn("checkpoint ready runtime release missing", "run_id", request.Lease.RunID, "session_id", request.Lease.ID, "checkpoint_id", request.CheckpointID)
		writeError(w, conflict(errors.New("worker run lease runtime is unavailable")))
		return
	}
	if err != nil {
		s.log.Error("worker runtime release lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("get worker runtime release"))
		return
	}
	if err := validateCheckpointReadyRuntime(runtimeRelease, params); err != nil {
		s.log.Warn(
			"checkpoint ready runtime rejected",
			"run_id", request.Lease.RunID,
			"session_id", request.Lease.ID,
			"checkpoint_id", request.CheckpointID,
			"runtime_backend", params.RuntimeBackend,
			"runtime_id", params.RuntimeID,
			"session_runtime_id", runtimeRelease.RuntimeID,
		)
		writeError(w, conflict(err))
		return
	}
	if err := s.verifyCheckpointReadyArtifacts(r.Context(), request.Manifest); err != nil {
		s.log.Warn("checkpoint ready artifact rejected", "run_id", request.Lease.RunID, "session_id", request.Lease.ID, "checkpoint_id", request.CheckpointID, "error", err)
		writeError(w, conflict(err))
		return
	}
	waitpoint, resumed, err := s.markWaitpointCheckpointReady(r.Context(), ids.ToPG(leaseIDs.orgID), ids.ToPG(waitpointID), params)
	if isNoRows(err) {
		s.log.Warn(
			"checkpoint ready rejected",
			"run_id", request.Lease.RunID,
			"session_id", request.Lease.ID,
			"checkpoint_id", request.CheckpointID,
			"runtime_backend", params.RuntimeBackend,
			"runtime_id", params.RuntimeID,
		)
		writeError(w, conflict(errors.New("worker run lease or checkpoint is stale")))
		return
	}
	if err != nil {
		s.log.Error("mark checkpoint ready failed", "run_id", request.Lease.RunID, "checkpoint_id", request.CheckpointID, "error", err)
		writeError(w, errors.New("mark checkpoint ready"))
		return
	}
	s.ackWorkerQueueLease(r.Context(), ids.ToPG(leaseIDs.runID), lease)
	if waitpoint.Status == db.RunWaitStatusWaiting && !resumed {
		go s.notifyPendingWaitpoint(context.Background(), checkpointReadyWaitpoint(waitpoint))
	}
	writeJSON(w, http.StatusOK, api.WorkerCreateWaitpointResponse{
		RunID:        request.Lease.RunID,
		RunWaitID:    ids.MustFromPG(waitpoint.RunWaitID).String(),
		WaitpointID:  ids.MustFromPG(waitpoint.ID).String(),
		CheckpointID: ids.MustFromPG(waitpoint.CheckpointID).String(),
	})
}

func validateCheckpointReadyRuntime(runtimeRelease db.GetRunExecutionSessionRuntimeReleaseRow, params db.MarkWaitpointCheckpointDurableReadyParams) error {
	// Keep this backend guard in sync with the SQL expected_runtime fence.
	if params.RuntimeBackend != checkpointRuntimeBackendFirecracker {
		return fmt.Errorf("checkpoint runtime backend %q is not supported", params.RuntimeBackend)
	}
	if params.RuntimeID != runtimeRelease.RuntimeID ||
		params.RuntimeArch != runtimeRelease.RuntimeArch ||
		params.RuntimeABI != runtimeRelease.RuntimeABI ||
		params.KernelDigest != runtimeRelease.KernelDigest ||
		params.InitramfsDigest != runtimeRelease.InitramfsDigest ||
		params.RootfsDigest != runtimeRelease.RootfsDigest ||
		params.CniProfile != runtimeRelease.CniProfile {
		return errors.New("checkpoint runtime does not match worker lease runtime")
	}
	return nil
}

func (s *Server) verifyCheckpointReadyArtifacts(ctx context.Context, manifest api.WorkerCheckpointManifest) error {
	if s.cas == nil {
		return nil
	}
	artifacts, err := checkpointArtifactParams(manifest)
	if err != nil {
		return err
	}
	for _, artifact := range artifacts {
		if err := s.verifyCheckpointCASObject(ctx, artifact.Digest, artifact.SizeBytes, artifact.MediaType, fmt.Sprintf("checkpoint artifact %s[%d]", artifact.Role, artifact.Ordinal)); err != nil {
			return err
		}
	}
	workspace := manifest.WorkspaceState.Base
	if strings.TrimSpace(workspace.ArtifactDigest) != "" {
		if err := s.verifyCheckpointCASObject(ctx, workspace.ArtifactDigest, workspace.ArtifactSizeBytes, workspace.ArtifactMediaType, "checkpoint workspace artifact"); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) verifyCheckpointCASObject(ctx context.Context, digest string, sizeBytes int64, mediaType string, label string) error {
	stat, err := s.cas.Stat(ctx, strings.TrimSpace(digest))
	if err != nil {
		return fmt.Errorf("%s %s is missing from CAS: %w", label, digest, err)
	}
	if stat.SizeBytes != sizeBytes {
		return fmt.Errorf("%s %s size mismatch", label, digest)
	}
	if strings.TrimSpace(stat.MediaType) != strings.TrimSpace(mediaType) {
		return fmt.Errorf("%s %s media_type mismatch", label, digest)
	}
	return nil
}

func (s *Server) markWaitpointCheckpointReady(ctx context.Context, orgID pgtype.UUID, waitpointID pgtype.UUID, params db.MarkWaitpointCheckpointDurableReadyParams) (db.MarkWaitpointCheckpointDurableReadyRow, bool, error) {
	if s.tx == nil {
		waitpoint, err := s.db.MarkWaitpointCheckpointDurableReady(ctx, params)
		if err != nil {
			return db.MarkWaitpointCheckpointDurableReadyRow{}, false, err
		}
		resumed := false
		if waitpoint.Status == db.RunWaitStatusWaiting {
			resumedWaits, err := s.db.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{
				OrgID:       orgID,
				WaitpointID: waitpointID,
			})
			if err != nil {
				return db.MarkWaitpointCheckpointDurableReadyRow{}, false, err
			}
			resumed = len(resumedWaits) > 0
		}
		return waitpoint, resumed, nil
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return db.MarkWaitpointCheckpointDurableReadyRow{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := db.New(tx)
	waitpoint, err := queries.MarkWaitpointCheckpointDurableReady(ctx, params)
	if err != nil {
		return db.MarkWaitpointCheckpointDurableReadyRow{}, false, err
	}
	resumed := false
	if waitpoint.Status == db.RunWaitStatusWaiting {
		resumedWaits, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{
			OrgID:       orgID,
			WaitpointID: waitpointID,
		})
		if err != nil {
			return db.MarkWaitpointCheckpointDurableReadyRow{}, false, err
		}
		resumed = len(resumedWaits) > 0
	}
	if err := tx.Commit(ctx); err != nil {
		return db.MarkWaitpointCheckpointDurableReadyRow{}, false, err
	}
	return waitpoint, resumed, nil
}

func (s *Server) workerCheckpointFailed(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerCheckpointFailedRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker checkpoint failed request JSON: %w", err)))
		return
	}
	leaseIDs, err := parseWorkerRunLease(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, forbidden(errors.New("worker run lease belongs to another worker")))
		return
	}
	if _, _, err := s.workerExecutionLease(r.Context(), worker, leaseIDs); isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease or checkpoint is stale")))
		return
	} else if err != nil {
		s.log.Error("worker queue lease lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("get queue lease"))
		return
	}
	runWaitID, err := ids.Parse(request.RunWaitID)
	if err != nil {
		writeError(w, badRequest(errors.New("run_wait_id must be a UUID")))
		return
	}
	waitpointID, err := ids.Parse(request.WaitpointID)
	if err != nil {
		writeError(w, badRequest(errors.New("waitpoint_id must be a UUID")))
		return
	}
	checkpointID, err := ids.Parse(request.CheckpointID)
	if err != nil {
		writeError(w, badRequest(errors.New("checkpoint_id must be a UUID")))
		return
	}
	message := strings.TrimSpace(request.Error)
	if message == "" {
		message = "checkpoint failed"
	}
	waitpoint, err := s.db.MarkWaitpointCheckpointFailed(r.Context(), db.MarkWaitpointCheckpointFailedParams{
		OrgID:            ids.ToPG(leaseIDs.orgID),
		RunID:            ids.ToPG(leaseIDs.runID),
		CheckpointID:     ids.ToPG(checkpointID),
		RunWaitID:        ids.ToPG(runWaitID),
		WaitpointID:      ids.ToPG(waitpointID),
		SessionID:        ids.ToPG(leaseIDs.sessionID),
		WorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
		ErrorMessage:     pgtype.Text{String: message, Valid: true},
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease or checkpoint is stale")))
		return
	}
	if err != nil {
		s.log.Error("mark checkpoint failed failed", "run_id", request.Lease.RunID, "checkpoint_id", request.CheckpointID, "error", err)
		writeError(w, errors.New("mark checkpoint failed"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerCreateWaitpointResponse{
		RunID:        request.Lease.RunID,
		RunWaitID:    ids.MustFromPG(waitpoint.RunWaitID).String(),
		WaitpointID:  ids.MustFromPG(waitpoint.ID).String(),
		CheckpointID: ids.MustFromPG(waitpoint.CheckpointID).String(),
	})
}

func checkpointReason(kind db.WaitpointKind) string {
	switch kind {
	case db.WaitpointKindHuman:
		return "wait_human"
	case db.WaitpointKindDelay:
		return "wait_delay"
	default:
		return "wait"
	}
}

func checkpointReadyParams(orgID uuid.UUID, leaseIDs workerRunLeaseIDs, workerInstanceID uuid.UUID, runWaitID uuid.UUID, waitpointID uuid.UUID, checkpointID uuid.UUID, request api.WorkerCheckpointReadyRequest) (db.MarkWaitpointCheckpointDurableReadyParams, error) {
	if request.ActiveDurationMs < 0 {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, errors.New("active_duration_ms must be non-negative")
	}
	if request.ActiveDurationMs > maxStoredActiveDurationMilliseconds {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, fmt.Errorf("active_duration_ms exceeds max %d", maxStoredActiveDurationMilliseconds)
	}
	manifest, err := json.Marshal(request.Manifest)
	if err != nil {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, fmt.Errorf("encode checkpoint manifest: %w", err)
	}
	if len(request.Manifest.RuntimeState.Config) == 0 {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, errors.New("manifest.runtime_state.config is required")
	}
	if !json.Valid(request.Manifest.RuntimeState.Config) {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, errors.New("manifest.runtime_state.config must be valid JSON")
	}
	if err := validateCheckpointRecoveryPoint(request.Manifest.RecoveryPoint, leaseIDs.runID, waitpointID, checkpointID, request.Manifest.RuntimeState.Config); err != nil {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, err
	}
	runtimeSpec, err := checkpointRuntimeSpec(request.Manifest.RuntimeState.Config)
	if err != nil {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, err
	}
	if runtimeSpec.CNIProfile == nil || strings.TrimSpace(*runtimeSpec.CNIProfile) == "" {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, errors.New("manifest.runtime_state.config.recovery_point.runtime.network.profile is required")
	}
	runtimeInfo := request.Manifest.RecoveryPoint.Runtime
	if runtimeInfo.Backend != checkpointRuntimeBackendFirecracker {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, fmt.Errorf("manifest.recovery_point.runtime.backend must be %s", checkpointRuntimeBackendFirecracker)
	}
	if strings.TrimSpace(runtimeInfo.ID) == "" {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, errors.New("manifest.recovery_point.runtime.id is required")
	}
	if strings.TrimSpace(runtimeInfo.Arch) == "" {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, errors.New("manifest.recovery_point.runtime.arch is required")
	}
	if strings.TrimSpace(runtimeInfo.ABI) == "" {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, errors.New("manifest.recovery_point.runtime.abi is required")
	}
	if strings.TrimSpace(runtimeInfo.KernelDigest) == "" {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, errors.New("manifest.recovery_point.runtime.kernel_digest is required")
	}
	if strings.TrimSpace(runtimeInfo.InitramfsDigest) == "" {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, errors.New("manifest.recovery_point.runtime.initramfs_digest is required")
	}
	if strings.TrimSpace(runtimeInfo.RootfsDigest) == "" {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, errors.New("manifest.recovery_point.runtime.rootfs_digest is required")
	}
	if strings.TrimSpace(runtimeInfo.ConfigDigest) == "" {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, errors.New("manifest.recovery_point.runtime.config_digest is required")
	}
	if _, err := requiredCheckpointManifestArtifact(request.Manifest.RuntimeState.ConfigArtifact, cas.CheckpointRuntimeConfigMediaType, "manifest.runtime_state.config_artifact"); err != nil {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, err
	}
	if _, err := requiredCheckpointManifestArtifact(request.Manifest.RuntimeState.VMStateArtifact, cas.CheckpointVMStateMediaType, "manifest.runtime_state.vm_state_artifact"); err != nil {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, err
	}
	if _, err := requiredCheckpointManifestArtifact(request.Manifest.RuntimeState.ScratchDiskArtifact, cas.CheckpointScratchDiskMediaType, "manifest.runtime_state.scratch_disk_artifact"); err != nil {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, err
	}
	workspace := request.Manifest.WorkspaceState.Base
	if strings.TrimSpace(workspace.ArtifactDigest) == "" {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, errors.New("manifest.workspace_state.base.artifact_digest is required")
	}
	if _, err := cas.ObjectKey("", workspace.ArtifactDigest); err != nil {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, fmt.Errorf("manifest.workspace_state.base.artifact_digest is invalid: %w", err)
	}
	if workspace.ArtifactSizeBytes < 0 {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, errors.New("manifest.workspace_state.base.artifact_size_bytes must be non-negative")
	}
	if strings.TrimSpace(workspace.ArtifactMediaType) == "" {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, errors.New("manifest.workspace_state.base.artifact_media_type is required")
	}
	if strings.TrimSpace(workspace.MountPath) == "" {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, errors.New("manifest.workspace_state.base.mount_path is required")
	}
	if strings.TrimSpace(workspace.VolumeKind) == "" {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, errors.New("manifest.workspace_state.base.volume_kind is required")
	}
	if len(request.Manifest.RuntimeState.MemoryArtifacts) != 1 {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, fmt.Errorf("manifest.runtime_state.memory_artifacts must contain exactly one artifact, got %d", len(request.Manifest.RuntimeState.MemoryArtifacts))
	}
	checkpointArtifacts, err := checkpointArtifactParams(request.Manifest)
	if err != nil {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, err
	}
	checkpointArtifactsJSON, err := json.Marshal(checkpointArtifacts)
	if err != nil {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, fmt.Errorf("encode checkpoint artifacts: %w", err)
	}
	checkpointPayload, err := json.Marshal(map[string]any{
		"run_id":        request.Lease.RunID,
		"waitpoint_id":  waitpointID.String(),
		"checkpoint_id": checkpointID.String(),
		"backend":       runtimeInfo.Backend,
		"runtime_id":    runtimeInfo.ID,
		"runtime_abi":   runtimeInfo.ABI,
	})
	if err != nil {
		return db.MarkWaitpointCheckpointDurableReadyParams{}, fmt.Errorf("encode checkpoint event: %w", err)
	}
	return db.MarkWaitpointCheckpointDurableReadyParams{
		OrgID:                      ids.ToPG(orgID),
		RunID:                      ids.ToPG(leaseIDs.runID),
		SessionID:                  ids.ToPG(leaseIDs.sessionID),
		WorkerInstanceID:           ids.ToPG(workerInstanceID),
		CheckpointArtifacts:        checkpointArtifactsJSON,
		Manifest:                   manifest,
		RuntimeBackend:             runtimeInfo.Backend,
		RuntimeID:                  runtimeInfo.ID,
		RuntimeArch:                runtimeInfo.Arch,
		RuntimeABI:                 runtimeInfo.ABI,
		KernelDigest:               runtimeInfo.KernelDigest,
		InitramfsDigest:            runtimeInfo.InitramfsDigest,
		RootfsDigest:               runtimeInfo.RootfsDigest,
		RuntimeVcpus:               pgvalue.Int4Ptr(runtimeSpec.VCPUCount),
		RuntimeMemoryMib:           pgvalue.Int4Ptr(runtimeSpec.MemoryMiB),
		RuntimeScratchDiskMib:      pgvalue.Int4Ptr(runtimeSpec.ScratchDiskMiB),
		CniProfile:                 *runtimeSpec.CNIProfile,
		ImageKey:                   pgvalue.TextPtr(runtimeInfo.ImageKey),
		WorkspaceArtifactDigest:    pgvalue.TextPtr(optionalTrimmedString(workspace.ArtifactDigest)),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: workspace.ArtifactSizeBytes, Valid: true},
		WorkspaceArtifactMediaType: pgvalue.TextPtr(optionalTrimmedString(workspace.ArtifactMediaType)),
		WorkspaceArtifactEncoding:  pgvalue.TextPtr(optionalTrimmedString(workspace.ArtifactEncoding)),
		WorkspaceMountPath:         pgvalue.TextPtr(optionalTrimmedString(workspace.MountPath)),
		WorkspaceVolumeKind:        pgvalue.TextPtr(optionalTrimmedString(workspace.VolumeKind)),
		ActiveDurationMs:           request.ActiveDurationMs,
		CheckpointID:               ids.ToPG(checkpointID),
		RunWaitID:                  ids.ToPG(runWaitID),
		WaitpointID:                ids.ToPG(waitpointID),
		CheckpointPayload:          checkpointPayload,
	}, nil
}

func validateCheckpointRecoveryPoint(recovery api.WorkerCheckpointRecoveryPoint, runID uuid.UUID, waitpointID uuid.UUID, checkpointID uuid.UUID, runtimeConfig json.RawMessage) error {
	if strings.TrimSpace(recovery.ID) != checkpointID.String() {
		return fmt.Errorf("manifest.recovery_point.id must match checkpoint_id %s", checkpointID.String())
	}
	if strings.TrimSpace(recovery.RunID) != runID.String() {
		return fmt.Errorf("manifest.recovery_point.run_id must match lease.run_id %s", runID.String())
	}
	if strings.TrimSpace(recovery.WaitpointID) != waitpointID.String() {
		return fmt.Errorf("manifest.recovery_point.waitpoint_id must match waitpoint_id %s", waitpointID.String())
	}
	if strings.TrimSpace(recovery.Runtime.ConfigDigest) != sha256sum.DigestBytes(runtimeConfig) {
		return fmt.Errorf("manifest.recovery_point.runtime.config_digest must match manifest.runtime_state.config digest")
	}
	return nil
}

func checkpointReadyWaitpoint(waitpoint db.MarkWaitpointCheckpointDurableReadyRow) waitpointView {
	return waitpointView{
		ID:             waitpoint.ID,
		RunWaitID:      waitpoint.RunWaitID,
		OrgID:          waitpoint.OrgID,
		RunID:          waitpoint.RunID,
		SessionID:      waitpoint.SessionID,
		CheckpointID:   waitpoint.CheckpointID,
		CorrelationID:  waitpoint.CorrelationID,
		Kind:           waitpoint.Kind,
		Request:        waitpoint.Request,
		DisplayText:    waitpoint.DisplayText,
		TimeoutSeconds: waitpoint.TimeoutSeconds,
		PolicyName:     waitpoint.PolicyName,
		PolicySnapshot: waitpoint.PolicySnapshot,
		Status:         waitpoint.Status,
		ResolutionKind: waitpoint.ResolutionKind,
		Resolution:     waitpoint.Resolution,
		CreatedAt:      waitpoint.CreatedAt,
		RequestedAt:    waitpoint.RequestedAt,
		ResolvedAt:     waitpoint.ResolvedAt,
	}
}

type checkpointRuntime struct {
	VCPUCount      *int32
	MemoryMiB      *int32
	ScratchDiskMiB *int32
	CNIProfile     *string
}

func checkpointRuntimeSpec(manifest json.RawMessage) (checkpointRuntime, error) {
	var payload struct {
		RecoveryPoint struct {
			Runtime struct {
				VCPUCount      int64 `json:"vcpu_count"`
				MemoryMiB      int64 `json:"memory_mib"`
				ScratchDiskMiB int64 `json:"scratch_disk_mib"`
				Network        struct {
					Profile string `json:"profile"`
				} `json:"network"`
			} `json:"runtime"`
		} `json:"recovery_point"`
	}
	if err := json.Unmarshal(manifest, &payload); err != nil {
		return checkpointRuntime{}, fmt.Errorf("decode checkpoint runtime manifest: %w", err)
	}
	runtimeInfo := payload.RecoveryPoint.Runtime
	vcpuCount, err := optionalPositiveInt32(runtimeInfo.VCPUCount, "manifest.runtime_state.config.recovery_point.runtime.vcpu_count")
	if err != nil {
		return checkpointRuntime{}, err
	}
	memoryMiB, err := optionalPositiveInt32(runtimeInfo.MemoryMiB, "manifest.runtime_state.config.recovery_point.runtime.memory_mib")
	if err != nil {
		return checkpointRuntime{}, err
	}
	scratchDiskMiB, err := optionalPositiveInt32(runtimeInfo.ScratchDiskMiB, "manifest.runtime_state.config.recovery_point.runtime.scratch_disk_mib")
	if err != nil {
		return checkpointRuntime{}, err
	}
	return checkpointRuntime{VCPUCount: vcpuCount, MemoryMiB: memoryMiB, ScratchDiskMiB: scratchDiskMiB, CNIProfile: optionalTrimmedString(runtimeInfo.Network.Profile)}, nil
}

type checkpointArtifactParam struct {
	Role              db.CheckpointArtifactRole `json:"role"`
	Ordinal           int32                     `json:"ordinal"`
	Digest            string                    `json:"digest"`
	SizeBytes         int64                     `json:"size_bytes"`
	MediaType         string                    `json:"media_type"`
	EncryptDurationMs int64                     `json:"encrypt_duration_ms"`
	StoreDurationMs   int64                     `json:"store_duration_ms"`
}

func requiredCheckpointManifestArtifact(artifact api.WorkerCheckpointArtifact, mediaType string, field string) (api.WorkerCheckpointArtifact, error) {
	if err := validateCheckpointArtifact(artifact, mediaType, field); err != nil {
		return api.WorkerCheckpointArtifact{}, err
	}
	return artifact, nil
}

func checkpointArtifactParams(manifest api.WorkerCheckpointManifest) ([]checkpointArtifactParam, error) {
	params := []checkpointArtifactParam{}
	seen := map[string]api.WorkerCheckpointArtifact{}
	add := func(dbRole db.CheckpointArtifactRole, ordinal int, artifact api.WorkerCheckpointArtifact, mediaType string, field string) error {
		artifact, err := requiredCheckpointManifestArtifact(artifact, mediaType, field)
		if err != nil {
			return err
		}
		if existing, ok := seen[artifact.Digest]; ok && (existing.SizeBytes != artifact.SizeBytes || existing.MediaType != artifact.MediaType) {
			return fmt.Errorf("checkpoint artifact %q has conflicting metadata", artifact.Digest)
		}
		seen[artifact.Digest] = artifact
		params = append(params, checkpointArtifactParam{
			Role:              dbRole,
			Ordinal:           int32(ordinal),
			Digest:            artifact.Digest,
			SizeBytes:         artifact.SizeBytes,
			MediaType:         artifact.MediaType,
			EncryptDurationMs: artifact.EncryptDurationMs,
			StoreDurationMs:   artifact.StoreDurationMs,
		})
		return nil
	}
	if err := add(db.CheckpointArtifactRoleRuntimeConfig, 0, manifest.RuntimeState.ConfigArtifact, cas.CheckpointRuntimeConfigMediaType, "manifest.runtime_state.config_artifact"); err != nil {
		return nil, err
	}
	if err := add(db.CheckpointArtifactRoleRuntimeVmstate, 0, manifest.RuntimeState.VMStateArtifact, cas.CheckpointVMStateMediaType, "manifest.runtime_state.vm_state_artifact"); err != nil {
		return nil, err
	}
	if err := add(db.CheckpointArtifactRoleRuntimeScratchDisk, 0, manifest.RuntimeState.ScratchDiskArtifact, cas.CheckpointScratchDiskMediaType, "manifest.runtime_state.scratch_disk_artifact"); err != nil {
		return nil, err
	}
	for i, artifact := range manifest.RuntimeState.MemoryArtifacts {
		if err := add(db.CheckpointArtifactRoleRuntimeMemory, i, artifact, cas.CheckpointMemoryMediaType, fmt.Sprintf("manifest.runtime_state.memory_artifacts[%d]", i)); err != nil {
			return nil, err
		}
	}
	sort.Slice(params, func(i, j int) bool {
		if params[i].Role != params[j].Role {
			return params[i].Role < params[j].Role
		}
		return params[i].Ordinal < params[j].Ordinal
	})
	return params, nil
}

func validateCheckpointArtifact(artifact api.WorkerCheckpointArtifact, mediaType string, field string) error {
	if _, err := cas.ObjectKey("", artifact.Digest); err != nil {
		return fmt.Errorf("%s.digest is invalid: %w", field, err)
	}
	if artifact.SizeBytes < 0 {
		return fmt.Errorf("%s.size_bytes must be non-negative", field)
	}
	if strings.TrimSpace(artifact.MediaType) == "" {
		return fmt.Errorf("%s.media_type is required", field)
	}
	if mediaType != "" && artifact.MediaType != mediaType {
		return fmt.Errorf("%s.media_type is %q, expected %q", field, artifact.MediaType, mediaType)
	}
	if artifact.EncryptDurationMs < 0 {
		return fmt.Errorf("%s.encrypt_duration_ms must be non-negative", field)
	}
	if artifact.StoreDurationMs < 0 {
		return fmt.Errorf("%s.store_duration_ms must be non-negative", field)
	}
	return nil
}

const maxStoredActiveDurationMilliseconds = int64(math.MaxInt64) / int64(time.Millisecond)
