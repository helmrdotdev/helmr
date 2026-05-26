package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerCreateWaitpoint(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	var request api.WorkerCreateWaitpointRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker waitpoint request JSON: %w", err))
		return
	}
	leaseIDs, err := parseWorkerRunLease(request.Lease)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, http.StatusForbidden, errors.New("worker run lease belongs to another worker"))
		return
	}
	if _, _, err := s.workerExecutionLease(r.Context(), worker, leaseIDs); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return
	} else if err != nil {
		s.log.Error("worker queue lease lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get queue lease"))
		return
	}
	request.CorrelationID = strings.TrimSpace(request.CorrelationID)
	if request.CorrelationID == "" {
		writeError(w, http.StatusBadRequest, errors.New("correlation_id is required"))
		return
	}
	requestJSON := request.Request
	if len(requestJSON) == 0 {
		requestJSON = []byte(`{}`)
	}
	if !json.Valid(requestJSON) {
		writeError(w, http.StatusBadRequest, errors.New("request must be valid JSON"))
		return
	}
	kind, displayText, err := waitpointRequestFields(request.Kind, requestJSON, request.DisplayText)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	timeout := pgtype.Int4{}
	if request.TimeoutSeconds != nil {
		if *request.TimeoutSeconds <= 0 {
			writeError(w, http.StatusBadRequest, errors.New("timeout_seconds must be positive"))
			return
		}
		timeout = pgtype.Int4{Int32: *request.TimeoutSeconds, Valid: true}
	}
	policy, err := s.resolveWaitpointPolicy(r.Context(), leaseIDs.orgID, request.Policy)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	policyName := pgtype.Text{}
	var policySnapshot []byte
	if policy != nil {
		snapshot, err := policy.snapshot()
		if err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("encode waitpoint policy"))
			return
		}
		policyName = pgText(policy.Name)
		policySnapshot = snapshot
	}
	waitpointID := ids.New()
	checkpointID := ids.New()
	waitpoint, err := s.db.CreateWaitpointForExecution(r.Context(), db.CreateWaitpointForExecutionParams{
		OrgID:            ids.ToPG(leaseIDs.orgID),
		RunID:            ids.ToPG(leaseIDs.runID),
		ExecutionID:      ids.ToPG(leaseIDs.executionID),
		WorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
		CheckpointID:     ids.ToPG(checkpointID),
		CheckpointReason: checkpointReason(kind),
		ID:               ids.ToPG(waitpointID),
		CorrelationID:    request.CorrelationID,
		Kind:             kind,
		Request:          requestJSON,
		DisplayText:      displayText,
		TimeoutSeconds:   timeout,
		PolicyName:       policyName,
		PolicySnapshot:   policySnapshot,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return
	}
	if err != nil {
		s.log.Error("create waitpoint failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("create waitpoint"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerCreateWaitpointResponse{
		RunID:        request.Lease.RunID,
		WaitpointID:  ids.MustFromPG(waitpoint.ID).String(),
		CheckpointID: ids.MustFromPG(waitpoint.CheckpointID).String(),
	})
}

func (s *Server) workerCheckpointReady(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	if s.dispatchQueue == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run queue item queue is not configured"))
		return
	}
	var request api.WorkerCheckpointReadyRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker checkpoint ready request JSON: %w", err))
		return
	}
	leaseIDs, err := parseWorkerRunLease(request.Lease)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, http.StatusForbidden, errors.New("worker run lease belongs to another worker"))
		return
	}
	waitpointID, err := ids.Parse(request.WaitpointID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("waitpoint_id must be a UUID"))
		return
	}
	checkpointID, err := ids.Parse(request.CheckpointID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("checkpoint_id must be a UUID"))
		return
	}
	params, err := checkpointReadyParams(leaseIDs.orgID, leaseIDs, worker.WorkerInstanceID, waitpointID, checkpointID, request)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	_, lease, err := s.workerExecutionLease(r.Context(), worker, leaseIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease or checkpoint is stale"))
		return
	}
	if err != nil {
		s.log.Error("worker queue lease lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get queue lease"))
		return
	}
	waitpoint, err := s.db.MarkWaitpointCheckpointReady(r.Context(), params)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease or checkpoint is stale"))
		return
	}
	if err != nil {
		s.log.Error("mark checkpoint ready failed", "run_id", request.Lease.RunID, "checkpoint_id", request.CheckpointID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("mark checkpoint ready"))
		return
	}
	s.ackWorkerQueueLease(r.Context(), ids.ToPG(leaseIDs.runID), lease)
	go s.notifyPendingWaitpoint(context.Background(), checkpointReadyWaitpoint(waitpoint))
	writeJSON(w, http.StatusOK, api.WorkerCreateWaitpointResponse{
		RunID:        request.Lease.RunID,
		WaitpointID:  ids.MustFromPG(waitpoint.ID).String(),
		CheckpointID: ids.MustFromPG(waitpoint.CheckpointID).String(),
	})
}

func (s *Server) workerCheckpointFailed(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	var request api.WorkerCheckpointFailedRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker checkpoint failed request JSON: %w", err))
		return
	}
	leaseIDs, err := parseWorkerRunLease(request.Lease)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, http.StatusForbidden, errors.New("worker run lease belongs to another worker"))
		return
	}
	if _, _, err := s.workerExecutionLease(r.Context(), worker, leaseIDs); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease or checkpoint is stale"))
		return
	} else if err != nil {
		s.log.Error("worker queue lease lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get queue lease"))
		return
	}
	waitpointID, err := ids.Parse(request.WaitpointID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("waitpoint_id must be a UUID"))
		return
	}
	checkpointID, err := ids.Parse(request.CheckpointID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("checkpoint_id must be a UUID"))
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
		WaitpointID:      ids.ToPG(waitpointID),
		ExecutionID:      ids.ToPG(leaseIDs.executionID),
		WorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
		ErrorMessage:     pgtype.Text{String: message, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease or checkpoint is stale"))
		return
	}
	if err != nil {
		s.log.Error("mark checkpoint failed failed", "run_id", request.Lease.RunID, "checkpoint_id", request.CheckpointID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("mark checkpoint failed"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerCreateWaitpointResponse{
		RunID:        request.Lease.RunID,
		WaitpointID:  ids.MustFromPG(waitpoint.ID).String(),
		CheckpointID: ids.MustFromPG(waitpoint.CheckpointID).String(),
	})
}

func (s *Server) approveWaitpoint(w http.ResponseWriter, r *http.Request) {
	s.resolveApprovalWaitpoint(w, r, true)
}

func (s *Server) denyWaitpoint(w http.ResponseWriter, r *http.Request) {
	s.resolveApprovalWaitpoint(w, r, false)
}

func (s *Server) resolveApprovalWaitpoint(w http.ResponseWriter, r *http.Request, approved bool) {
	var request api.ResumeApprovalRequest
	if err := decodeOptionalJSON(r.Body, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid approval response JSON: %w", err))
		return
	}
	kind, payload, eventPayload, err := approvalWaitpointResolution(approved, "operator", request.Reason, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode approval response"))
		return
	}
	s.resolveWaitpoint(w, r, db.WaitpointKindApproval, kind, payload, eventPayload)
}

func (s *Server) messageWaitpoint(w http.ResponseWriter, r *http.Request) {
	var request api.ResumeMessageRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid message response JSON: %w", err))
		return
	}
	kind, payload, eventPayload, err := messageWaitpointResolution("operator", request.Text, request.Attachments, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode message response"))
		return
	}
	s.resolveWaitpoint(w, r, db.WaitpointKindMessage, kind, payload, eventPayload)
}

func (s *Server) resolveWaitpoint(w http.ResponseWriter, r *http.Request, expectedKind db.WaitpointKind, resolutionKind string, resolutionJSON []byte, eventPayload map[string]any) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	runID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	waitpointID, err := ids.Parse(chi.URLParam(r, "waitpointID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("waitpointID must be a UUID"))
		return
	}
	actor := actorFromContext(r.Context())
	run, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{
		OrgID: ids.ToPG(actor.OrgID),
		ID:    ids.ToPG(runID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("pending waitpoint not found"))
		return
	}
	if err != nil {
		s.log.Error("get run before resolving waitpoint failed", "run_id", runID.String(), "waitpoint_id", waitpointID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("resolve waitpoint"))
		return
	}
	scope, err := s.runScope(r.Context(), actor.OrgID, getRunSummary(run))
	if err != nil {
		s.log.Error("resolve run scope before resolving waitpoint failed", "run_id", runID.String(), "waitpoint_id", waitpointID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("resolve waitpoint"))
		return
	}
	if !actor.HasPermission(auth.PermissionWaitpointsRespond, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	if err := s.resolveWaitpointRecord(r.Context(), waitpointResolution{
		OrgID:          actor.OrgID,
		RunID:          runID,
		WaitpointID:    waitpointID,
		ExpectedKind:   expectedKind,
		ResolutionKind: resolutionKind,
		ResolutionJSON: resolutionJSON,
		EventPayload:   eventPayload,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("pending waitpoint not found"))
			return
		}
		s.log.Error("resolve waitpoint failed", "run_id", runID.String(), "waitpoint_id", waitpointID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("resolve waitpoint"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type waitpointResolution struct {
	OrgID          uuid.UUID
	RunID          uuid.UUID
	WaitpointID    uuid.UUID
	ExpectedKind   db.WaitpointKind
	ResolutionKind string
	ResolutionJSON []byte
	EventPayload   map[string]any
}

func (s *Server) resolveWaitpointRecord(ctx context.Context, resolution waitpointResolution) error {
	eventPayload := resolution.EventPayload
	if eventPayload == nil {
		eventPayload = map[string]any{}
	}
	runID := resolution.RunID
	waitpointID := resolution.WaitpointID
	eventPayload["run_id"] = runID.String()
	eventPayload["waitpoint_id"] = waitpointID.String()
	eventJSON, err := json.Marshal(eventPayload)
	if err != nil {
		return fmt.Errorf("encode waitpoint resolved event: %w", err)
	}
	_, err = s.db.ResolveWaitpoint(ctx, db.ResolveWaitpointParams{
		ResolutionKind: pgtype.Text{String: resolution.ResolutionKind, Valid: true},
		Resolution:     resolution.ResolutionJSON,
		OrgID:          ids.ToPG(resolution.OrgID),
		RunID:          ids.ToPG(runID),
		ID:             ids.ToPG(waitpointID),
		Kind:           resolution.ExpectedKind,
		Payload:        eventJSON,
	})
	return err
}

func waitpointRequestFields(kind api.WorkerWaitpointKind, request json.RawMessage, displayText string) (db.WaitpointKind, string, error) {
	displayText = strings.TrimSpace(displayText)
	switch kind {
	case api.WorkerWaitpointKindApproval:
		var payload struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(request, &payload); err != nil {
			return "", "", fmt.Errorf("decode approval wait request: %w", err)
		}
		if displayText == "" {
			displayText = payload.Message
		}
		return db.WaitpointKindApproval, displayText, nil
	case api.WorkerWaitpointKindMessage:
		var payload struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal(request, &payload); err != nil {
			return "", "", fmt.Errorf("decode message wait request: %w", err)
		}
		if displayText == "" {
			displayText = payload.Prompt
		}
		return db.WaitpointKindMessage, displayText, nil
	default:
		return "", "", fmt.Errorf("unsupported waitpoint kind %q", kind)
	}
}

func checkpointReason(kind db.WaitpointKind) string {
	switch kind {
	case db.WaitpointKindApproval:
		return "wait_approval"
	case db.WaitpointKindMessage:
		return "wait_message"
	default:
		return "wait"
	}
}

func checkpointReadyParams(orgID uuid.UUID, leaseIDs workerRunLeaseIDs, workerInstanceID uuid.UUID, waitpointID uuid.UUID, checkpointID uuid.UUID, request api.WorkerCheckpointReadyRequest) (db.MarkWaitpointCheckpointReadyParams, error) {
	if request.ActiveDurationMs < 0 {
		return db.MarkWaitpointCheckpointReadyParams{}, errors.New("active_duration_ms must be non-negative")
	}
	if request.ActiveDurationMs > maxStoredActiveDurationMilliseconds {
		return db.MarkWaitpointCheckpointReadyParams{}, fmt.Errorf("active_duration_ms exceeds max %d", maxStoredActiveDurationMilliseconds)
	}
	manifest := request.Manifest.RuntimeManifest
	if len(manifest) == 0 {
		manifest = []byte(`{}`)
	}
	if !json.Valid(manifest) {
		return db.MarkWaitpointCheckpointReadyParams{}, errors.New("manifest.runtime_manifest must be valid JSON")
	}
	runtimeSpec, err := checkpointRuntimeSpec(manifest)
	if err != nil {
		return db.MarkWaitpointCheckpointReadyParams{}, err
	}
	if request.Manifest.Runtime.Backend != "firecracker" {
		return db.MarkWaitpointCheckpointReadyParams{}, errors.New("manifest.runtime.backend must be firecracker")
	}
	if strings.TrimSpace(request.Manifest.Runtime.Arch) == "" {
		return db.MarkWaitpointCheckpointReadyParams{}, errors.New("manifest.runtime.arch is required")
	}
	if strings.TrimSpace(request.Manifest.Runtime.ABI) == "" {
		return db.MarkWaitpointCheckpointReadyParams{}, errors.New("manifest.runtime.abi is required")
	}
	if strings.TrimSpace(request.Manifest.Runtime.KernelDigest) == "" {
		return db.MarkWaitpointCheckpointReadyParams{}, errors.New("manifest.runtime.kernel_digest is required")
	}
	if strings.TrimSpace(request.Manifest.Runtime.RootfsDigest) == "" {
		return db.MarkWaitpointCheckpointReadyParams{}, errors.New("manifest.runtime.rootfs_digest is required")
	}
	if strings.TrimSpace(request.Manifest.Runtime.ConfigDigest) == "" {
		return db.MarkWaitpointCheckpointReadyParams{}, errors.New("manifest.runtime.config_digest is required")
	}
	if strings.TrimSpace(request.Manifest.RuntimeState.Manifest.Digest) == "" {
		return db.MarkWaitpointCheckpointReadyParams{}, errors.New("manifest.runtime_state.manifest.digest is required")
	}
	if strings.TrimSpace(request.Manifest.RuntimeState.VMState.Digest) == "" {
		return db.MarkWaitpointCheckpointReadyParams{}, errors.New("manifest.runtime_state.vm_state.digest is required")
	}
	if request.Manifest.RuntimeState.ScratchDisk == nil || strings.TrimSpace(request.Manifest.RuntimeState.ScratchDisk.Digest) == "" {
		return db.MarkWaitpointCheckpointReadyParams{}, errors.New("manifest.runtime_state.scratch_disk.digest is required")
	}
	workspace := request.Manifest.Workspace.Base
	if strings.TrimSpace(workspace.Kind) == "" {
		return db.MarkWaitpointCheckpointReadyParams{}, errors.New("manifest.workspace.base.kind is required")
	}
	if strings.TrimSpace(workspace.ArtifactDigest) == "" {
		return db.MarkWaitpointCheckpointReadyParams{}, errors.New("manifest.workspace.base.artifact_digest is required")
	}
	if strings.TrimSpace(workspace.MountPath) == "" {
		return db.MarkWaitpointCheckpointReadyParams{}, errors.New("manifest.workspace.base.mount_path is required")
	}
	if strings.TrimSpace(workspace.VolumeKind) == "" {
		return db.MarkWaitpointCheckpointReadyParams{}, errors.New("manifest.workspace.base.volume_kind is required")
	}
	if len(request.Manifest.RuntimeState.Memory) == 0 {
		return db.MarkWaitpointCheckpointReadyParams{}, errors.New("manifest.runtime_state.memory is required")
	}
	checkpointArtifacts, err := checkpointArtifactParams(request.Manifest)
	if err != nil {
		return db.MarkWaitpointCheckpointReadyParams{}, err
	}
	checkpointArtifactsJSON, err := json.Marshal(checkpointArtifacts)
	if err != nil {
		return db.MarkWaitpointCheckpointReadyParams{}, fmt.Errorf("encode checkpoint artifacts: %w", err)
	}
	checkpointPayload, err := json.Marshal(map[string]any{
		"run_id":        request.Lease.RunID,
		"waitpoint_id":  waitpointID.String(),
		"checkpoint_id": checkpointID.String(),
		"backend":       request.Manifest.Runtime.Backend,
		"runtime_abi":   request.Manifest.Runtime.ABI,
	})
	if err != nil {
		return db.MarkWaitpointCheckpointReadyParams{}, fmt.Errorf("encode checkpoint event: %w", err)
	}
	return db.MarkWaitpointCheckpointReadyParams{
		OrgID:                      ids.ToPG(orgID),
		RunID:                      ids.ToPG(leaseIDs.runID),
		ExecutionID:                ids.ToPG(leaseIDs.executionID),
		WorkerInstanceID:           ids.ToPG(workerInstanceID),
		CheckpointArtifacts:        checkpointArtifactsJSON,
		Manifest:                   manifest,
		RuntimeBackend:             pgtype.Text{String: request.Manifest.Runtime.Backend, Valid: true},
		RuntimeArch:                pgtype.Text{String: request.Manifest.Runtime.Arch, Valid: true},
		RuntimeABI:                 pgtype.Text{String: request.Manifest.Runtime.ABI, Valid: true},
		KernelDigest:               pgTextPtr(optionalTrimmedString(request.Manifest.Runtime.KernelDigest)),
		RootfsDigest:               pgTextPtr(optionalTrimmedString(request.Manifest.Runtime.RootfsDigest)),
		RuntimeVcpus:               pgInt4Ptr(runtimeSpec.VCPUCount),
		RuntimeMemoryMib:           pgInt4Ptr(runtimeSpec.MemoryMiB),
		RuntimeScratchDiskMib:      pgInt4Ptr(runtimeSpec.ScratchDiskMiB),
		CniProfile:                 pgTextPtr(runtimeSpec.CNIProfile),
		ImageKey:                   pgTextPtr(request.Manifest.Runtime.ImageKey),
		RuntimeConfigDigest:        pgTextPtr(optionalTrimmedString(request.Manifest.Runtime.ConfigDigest)),
		WorkspaceBaseKind:          pgTextPtr(optionalTrimmedString(workspace.Kind)),
		WorkspaceRepository:        pgTextPtr(optionalTrimmedString(workspace.Repository)),
		WorkspaceRef:               pgTextPtr(optionalTrimmedString(workspace.Ref)),
		WorkspaceSha:               pgTextPtr(optionalTrimmedString(workspace.SHA)),
		WorkspaceSubpath:           pgTextPtr(optionalTrimmedString(workspace.Subpath)),
		WorkspaceRefKind:           pgTextPtr(optionalTrimmedString(string(workspace.RefKind))),
		WorkspaceRefName:           pgTextPtr(optionalTrimmedString(workspace.RefName)),
		WorkspaceFullRef:           pgTextPtr(optionalTrimmedString(workspace.FullRef)),
		WorkspaceDefaultBranch:     pgTextPtr(optionalTrimmedString(workspace.DefaultBranch)),
		WorkspaceArtifactDigest:    pgTextPtr(optionalTrimmedString(workspace.ArtifactDigest)),
		WorkspaceArtifactMediaType: pgTextPtr(optionalTrimmedString(workspace.ArtifactMediaType)),
		WorkspaceArtifactEncoding:  pgTextPtr(optionalTrimmedString(workspace.ArtifactEncoding)),
		WorkspaceMountPath:         pgTextPtr(optionalTrimmedString(workspace.MountPath)),
		WorkspaceVolumeKind:        pgTextPtr(optionalTrimmedString(workspace.VolumeKind)),
		ActiveDurationMs:           request.ActiveDurationMs,
		CheckpointID:               ids.ToPG(checkpointID),
		WaitpointID:                ids.ToPG(waitpointID),
		CheckpointPayload:          checkpointPayload,
	}, nil
}

func checkpointReadyWaitpoint(waitpoint db.MarkWaitpointCheckpointReadyRow) db.Waitpoint {
	return db.Waitpoint{
		ID:             waitpoint.ID,
		OrgID:          waitpoint.OrgID,
		RunID:          waitpoint.RunID,
		ExecutionID:    waitpoint.ExecutionID,
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
		Runtime struct {
			VCPUCount      int64 `json:"vcpu_count"`
			MemoryMiB      int64 `json:"memory_mib"`
			ScratchDiskMiB int64 `json:"scratch_disk_mib"`
			Network        struct {
				Profile string `json:"profile"`
			} `json:"network"`
		} `json:"runtime"`
	}
	if err := json.Unmarshal(manifest, &payload); err != nil {
		return checkpointRuntime{}, fmt.Errorf("decode checkpoint runtime manifest: %w", err)
	}
	vcpuCount, err := optionalPositiveInt32(payload.Runtime.VCPUCount, "manifest.runtime_manifest.runtime.vcpu_count")
	if err != nil {
		return checkpointRuntime{}, err
	}
	memoryMiB, err := optionalPositiveInt32(payload.Runtime.MemoryMiB, "manifest.runtime_manifest.runtime.memory_mib")
	if err != nil {
		return checkpointRuntime{}, err
	}
	scratchDiskMiB, err := optionalPositiveInt32(payload.Runtime.ScratchDiskMiB, "manifest.runtime_manifest.runtime.scratch_disk_mib")
	if err != nil {
		return checkpointRuntime{}, err
	}
	return checkpointRuntime{VCPUCount: vcpuCount, MemoryMiB: memoryMiB, ScratchDiskMiB: scratchDiskMiB, CNIProfile: optionalTrimmedString(payload.Runtime.Network.Profile)}, nil
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

func checkpointArtifactParams(manifest api.WorkerCheckpointManifest) ([]checkpointArtifactParam, error) {
	params := []checkpointArtifactParam{}
	seen := map[string]api.WorkerCheckpointArtifact{}
	add := func(role db.CheckpointArtifactRole, ordinal int, artifact api.WorkerCheckpointArtifact, mediaType string, field string) error {
		if err := validateCheckpointArtifact(artifact, mediaType, field); err != nil {
			return err
		}
		if existing, ok := seen[artifact.Digest]; ok && (existing.SizeBytes != artifact.SizeBytes || existing.MediaType != artifact.MediaType) {
			return fmt.Errorf("checkpoint artifact %q has conflicting metadata", artifact.Digest)
		}
		seen[artifact.Digest] = artifact
		params = append(params, checkpointArtifactParam{
			Role:              role,
			Ordinal:           int32(ordinal),
			Digest:            artifact.Digest,
			SizeBytes:         artifact.SizeBytes,
			MediaType:         artifact.MediaType,
			EncryptDurationMs: artifact.EncryptDurationMs,
			StoreDurationMs:   artifact.StoreDurationMs,
		})
		return nil
	}
	if err := add(db.CheckpointArtifactRoleRuntimeManifest, 0, manifest.RuntimeState.Manifest, cas.CheckpointManifestMediaType, "manifest.runtime_state.manifest"); err != nil {
		return nil, err
	}
	if err := add(db.CheckpointArtifactRoleRuntimeVmstate, 0, manifest.RuntimeState.VMState, cas.CheckpointVMStateMediaType, "manifest.runtime_state.vm_state"); err != nil {
		return nil, err
	}
	if manifest.RuntimeState.ScratchDisk == nil {
		return nil, errors.New("manifest.runtime_state.scratch_disk is required")
	}
	if err := add(db.CheckpointArtifactRoleRuntimeScratchDisk, 0, *manifest.RuntimeState.ScratchDisk, cas.CheckpointScratchDiskMediaType, "manifest.runtime_state.scratch_disk"); err != nil {
		return nil, err
	}
	for i, artifact := range manifest.RuntimeState.Memory {
		if err := add(db.CheckpointArtifactRoleRuntimeMemory, i, artifact, cas.CheckpointMemoryMediaType, fmt.Sprintf("manifest.runtime_state.memory[%d]", i)); err != nil {
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

func pgTextPtr(value *string) pgtype.Text {
	if value == nil || strings.TrimSpace(*value) == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *value, Valid: true}
}

func pgInt4Ptr(value *int32) pgtype.Int4 {
	if value == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *value, Valid: true}
}

func optionalPositiveInt32(value int64, field string) (*int32, error) {
	if value == 0 {
		return nil, nil
	}
	if value < 0 || value > math.MaxInt32 {
		return nil, fmt.Errorf("%s must be between 1 and %d", field, math.MaxInt32)
	}
	out := int32(value)
	return &out, nil
}

func optionalTrimmedString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

func decodeOptionalJSON(r io.Reader, out any) error {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
