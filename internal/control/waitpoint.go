package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

const checkpointRuntimeBackendFirecracker = "firecracker"

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
	leaseRow, _, err := s.workerExecutionLease(r.Context(), worker, leaseIDs)
	if errors.Is(err, pgx.ErrNoRows) {
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
	timeout, err := waitpointTimeout(kind, request.TimeoutSeconds)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	policy, err := s.resolveWaitpointPolicy(r.Context(), leaseIDs.orgID, leaseRow.ProjectID, leaseRow.EnvironmentID, request.Policy)
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
	runWaitID := ids.New()
	waitpointID := ids.New()
	if linkedWaitpointID, ok, err := waitpointRequestLinkedID(kind, requestJSON); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	} else if ok {
		waitpointID = linkedWaitpointID
	}
	checkpointID := ids.New()
	waitpoint, err := s.db.CreateWaitpointForExecution(r.Context(), db.CreateWaitpointForExecutionParams{
		OrgID:            ids.ToPG(leaseIDs.orgID),
		RunID:            ids.ToPG(leaseIDs.runID),
		SessionID:        ids.ToPG(leaseIDs.sessionID),
		WorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
		CheckpointID:     ids.ToPG(checkpointID),
		CheckpointReason: checkpointReason(kind),
		RunWaitID:        ids.ToPG(runWaitID),
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
		RunWaitID:    ids.MustFromPG(waitpoint.RunWaitID).String(),
		WaitpointID:  ids.MustFromPG(waitpoint.ID).String(),
		CheckpointID: ids.MustFromPG(waitpoint.CheckpointID).String(),
	})
}

func (s *Server) workerAcknowledgeRestore(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	var request api.WorkerAcknowledgeRestoreRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker restore ack request JSON: %w", err))
		return
	}
	worker, leaseIDs, ok := s.workerRunLeaseForWrite(w, r, request.Lease)
	if !ok {
		return
	}
	runWaitID, err := ids.Parse(request.RunWaitID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("run_wait_id must be a UUID"))
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
	waitpoint, err := s.db.AcknowledgeRestore(r.Context(), db.AcknowledgeRestoreParams{
		OrgID:            ids.ToPG(leaseIDs.orgID),
		RunID:            ids.ToPG(leaseIDs.runID),
		SessionID:        ids.ToPG(leaseIDs.sessionID),
		WorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
		CheckpointID:     ids.ToPG(checkpointID),
		RunWaitID:        ids.ToPG(runWaitID),
		WaitpointID:      ids.ToPG(waitpointID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker restore acknowledgement is stale"))
		return
	}
	if err != nil {
		s.log.Error("acknowledge restore failed", "run_id", request.Lease.RunID, "checkpoint_id", request.CheckpointID, "waitpoint_id", request.WaitpointID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("acknowledge restore"))
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
	runWaitID, err := ids.Parse(request.RunWaitID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("run_wait_id must be a UUID"))
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
	params, err := checkpointReadyParams(leaseIDs.orgID, leaseIDs, worker.WorkerInstanceID, runWaitID, waitpointID, checkpointID, request)
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
	runtimeRelease, err := s.db.GetRunExecutionSessionRuntimeRelease(r.Context(), db.GetRunExecutionSessionRuntimeReleaseParams{
		OrgID:            ids.ToPG(leaseIDs.orgID),
		RunID:            ids.ToPG(leaseIDs.runID),
		SessionID:        ids.ToPG(leaseIDs.sessionID),
		WorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		s.log.Warn("checkpoint ready runtime release missing", "run_id", request.Lease.RunID, "session_id", request.Lease.ID, "checkpoint_id", request.CheckpointID)
		writeError(w, http.StatusConflict, errors.New("worker run lease runtime is unavailable"))
		return
	}
	if err != nil {
		s.log.Error("worker runtime release lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get worker runtime release"))
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
		writeError(w, http.StatusConflict, err)
		return
	}
	if err := s.verifyCheckpointReadyArtifacts(r.Context(), request.Manifest); err != nil {
		s.log.Warn("checkpoint ready artifact rejected", "run_id", request.Lease.RunID, "session_id", request.Lease.ID, "checkpoint_id", request.CheckpointID, "error", err)
		writeError(w, http.StatusConflict, err)
		return
	}
	waitpoint, resumed, err := s.markWaitpointCheckpointReady(r.Context(), ids.ToPG(leaseIDs.orgID), ids.ToPG(waitpointID), params)
	if errors.Is(err, pgx.ErrNoRows) {
		s.log.Warn(
			"checkpoint ready rejected",
			"run_id", request.Lease.RunID,
			"session_id", request.Lease.ID,
			"checkpoint_id", request.CheckpointID,
			"runtime_backend", params.RuntimeBackend,
			"runtime_id", params.RuntimeID,
		)
		writeError(w, http.StatusConflict, errors.New("worker run lease or checkpoint is stale"))
		return
	}
	if err != nil {
		s.log.Error("mark checkpoint ready failed", "run_id", request.Lease.RunID, "checkpoint_id", request.CheckpointID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("mark checkpoint ready"))
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
	runWaitID, err := ids.Parse(request.RunWaitID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("run_wait_id must be a UUID"))
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
		RunWaitID:        ids.ToPG(runWaitID),
		WaitpointID:      ids.ToPG(waitpointID),
		SessionID:        ids.ToPG(leaseIDs.sessionID),
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
		RunWaitID:    ids.MustFromPG(waitpoint.RunWaitID).String(),
		WaitpointID:  ids.MustFromPG(waitpoint.ID).String(),
		CheckpointID: ids.MustFromPG(waitpoint.CheckpointID).String(),
	})
}

func (s *Server) createWaitpoint(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	var request api.CreateWaitpointRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid waitpoint request JSON: %w", err))
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
	if request.ExpiresAt.IsZero() || !request.ExpiresAt.After(time.Now().UTC()) {
		writeError(w, http.StatusBadRequest, errors.New("expires_at must be in the future"))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !actor.HasPermission(auth.PermissionWaitpointsRespond, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	idempotencyKey := strings.TrimSpace(request.IdempotencyKey)
	idempotencyKeyHash := pgtype.Text{}
	idempotencyKeyExpiresAt := pgtype.Timestamptz{}
	if idempotencyKey != "" {
		idempotencyKeyHash = pgtype.Text{String: waitpointCreationRequestHash(requestJSON, request.DisplayText, request.ExpiresAt), Valid: true}
		expiresAt, err := waitpointTokenExpiry(time.Now().UTC(), request.IdempotencyKeyExpiresAt, request.IdempotencyKeyTTLSeconds, defaultIdempotencyKeyTTL, "idempotency_key_expires")
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		idempotencyKeyExpiresAt = pgTimeToPG(expiresAt)
	}
	waitpoint, err := s.db.CreateHumanWaitpoint(r.Context(), db.CreateHumanWaitpointParams{
		ID:                      ids.ToPG(ids.New()),
		OrgID:                   ids.ToPG(actor.OrgID),
		ProjectID:               projectID,
		EnvironmentID:           environmentID,
		Request:                 requestJSON,
		DisplayText:             strings.TrimSpace(request.DisplayText),
		ExpiresAt:               pgTimeToPG(request.ExpiresAt.UTC()),
		IdempotencyKey:          pgText(idempotencyKey),
		IdempotencyRequestHash:  idempotencyKeyHash,
		IdempotencyKeyExpiresAt: idempotencyKeyExpiresAt,
		IdempotencyKeyOptions:   []byte(`{}`),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("idempotency key reused with a different request"))
		return
	}
	if err != nil {
		s.log.Error("create waitpoint failed", "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("create waitpoint"))
		return
	}
	writeJSON(w, http.StatusCreated, waitpointResponseFromCreate(waitpoint))
}

func (s *Server) respondWaitpoint(w http.ResponseWriter, r *http.Request) {
	var request api.RespondWaitpointRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid waitpoint response JSON: %w", err))
		return
	}
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	waitpointID, err := ids.Parse(chi.URLParam(r, "waitpointID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("waitpointID must be a UUID"))
		return
	}
	actor := actorFromContext(r.Context())
	waitpoint, err := s.db.GetWaitpointForRespond(r.Context(), db.GetWaitpointForRespondParams{
		OrgID:       ids.ToPG(actor.OrgID),
		WaitpointID: ids.ToPG(waitpointID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("pending waitpoint not found"))
		return
	}
	if err != nil {
		s.log.Error("get waitpoint before resolving failed", "waitpoint_id", waitpointID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("resolve waitpoint"))
		return
	}
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     ids.MustFromPG(waitpoint.ProjectID).String(),
		EnvironmentID: ids.MustFromPG(waitpoint.EnvironmentID).String(),
	}
	if err := s.requireActorScopeForRecord(r, actor, waitpoint.ProjectID, waitpoint.EnvironmentID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, errors.New("pending waitpoint not found"))
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !actor.HasPermission(auth.PermissionWaitpointsRespond, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	responseKey, principal, err := waitpointActorResponseIdentity(actor)
	if err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	expectedKind := db.WaitpointKindHuman
	response, err := waitpointResponsePayload(expectedKind, principal, request.Value, request.Metadata, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	outcome, err := s.resolveWaitpointRecord(r.Context(), waitpointResolution{
		OrgID:           actor.OrgID,
		WaitpointID:     waitpointID,
		ResponseKey:     responseKey,
		Principal:       principal,
		ExternalSubject: request.ExternalSubject,
		ExpectedKind:    expectedKind,
		ResolutionKind:  response.ResolutionKind,
		OutputJSON:      response.Output,
		ResolutionJSON:  response.Resolution,
		EventPayload:    response.EventPayload,
		Metadata:        response.Metadata,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusConflict, errors.New("waitpoint cannot be resolved"))
			return
		}
		s.log.Error("resolve waitpoint failed", "waitpoint_id", waitpointID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("resolve waitpoint"))
		return
	}
	if !outcome.Resumed {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type waitpointResolution struct {
	OrgID           uuid.UUID
	WaitpointID     uuid.UUID
	ResponseKey     string
	Principal       string
	ExternalSubject string
	ExpectedKind    db.WaitpointKind
	ResolutionKind  string
	OutputJSON      []byte
	ResolutionJSON  []byte
	EventPayload    map[string]any
	Metadata        []byte
}

type waitpointResolveOutcome struct {
	Resumed bool
}

func waitpointResolveOutcomeFromStatus(status db.RunWaitStatus) waitpointResolveOutcome {
	return waitpointResolveOutcome{Resumed: status == db.RunWaitStatusResuming || status == db.RunWaitStatusRestored}
}

func waitpointResponse(row db.Waitpoint) api.WaitpointResponse {
	expiresAt := pgTime(row.ExpiresAt)
	var expiresAtPtr *time.Time
	if row.ExpiresAt.Valid {
		expiresAtPtr = &expiresAt
	}
	return api.WaitpointResponse{
		ID:            ids.MustFromPG(row.ID).String(),
		ProjectID:     ids.MustFromPG(row.ProjectID).String(),
		EnvironmentID: ids.MustFromPG(row.EnvironmentID).String(),
		Kind:          string(row.Kind),
		Status:        string(row.Status),
		Request:       row.Request,
		DisplayText:   row.DisplayText,
		ExpiresAt:     expiresAtPtr,
		CreatedAt:     pgTime(row.CreatedAt),
	}
}

func waitpointResponseFromCreate(row db.CreateHumanWaitpointRow) api.WaitpointResponse {
	expiresAt := pgTime(row.ExpiresAt)
	var expiresAtPtr *time.Time
	if row.ExpiresAt.Valid {
		expiresAtPtr = &expiresAt
	}
	return api.WaitpointResponse{
		ID:            ids.MustFromPG(row.ID).String(),
		ProjectID:     ids.MustFromPG(row.ProjectID).String(),
		EnvironmentID: ids.MustFromPG(row.EnvironmentID).String(),
		Kind:          string(row.Kind),
		Status:        string(row.Status),
		Request:       row.Request,
		DisplayText:   row.DisplayText,
		ExpiresAt:     expiresAtPtr,
		CreatedAt:     pgTime(row.CreatedAt),
	}
}

func waitpointCreationRequestHash(request json.RawMessage, displayText string, expiresAt time.Time) string {
	payload, _ := json.Marshal(map[string]any{
		"kind":         "human",
		"request":      json.RawMessage(request),
		"display_text": strings.TrimSpace(displayText),
		"expires_at":   expiresAt.UTC().Format(time.RFC3339Nano),
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func (s *Server) resolveWaitpointRecord(ctx context.Context, resolution waitpointResolution) (waitpointResolveOutcome, error) {
	eventPayload := resolution.EventPayload
	if eventPayload == nil {
		eventPayload = map[string]any{}
	}
	waitpointID := resolution.WaitpointID
	eventPayload["waitpoint_id"] = waitpointID.String()
	eventJSON, err := json.Marshal(eventPayload)
	if err != nil {
		return waitpointResolveOutcome{}, fmt.Errorf("encode waitpoint resolved event: %w", err)
	}
	recordParams := db.RecordWaitpointResponseParams{
		ID:                   ids.ToPG(ids.New()),
		ResponseKey:          resolution.ResponseKey,
		RequestHash:          waitpointResponseRequestHash(resolution.OutputJSON, resolution.ExternalSubject, resolution.Metadata),
		Action:               "respond",
		ResolutionKind:       pgtype.Text{String: resolution.ResolutionKind, Valid: true},
		Resolution:           resolution.ResolutionJSON,
		EventPayload:         eventJSON,
		CompletedByPrincipal: pgtype.Text{String: resolution.Principal, Valid: true},
		CompletedVia:         pgtype.Text{String: "authenticated_api", Valid: true},
		ExternalSubject:      pgText(resolution.ExternalSubject),
		Metadata:             resolution.Metadata,
		OrgID:                ids.ToPG(resolution.OrgID),
		WaitpointID:          ids.ToPG(waitpointID),
		Kind:                 resolution.ExpectedKind,
	}
	resolveParams := db.ResolveWaitpointParams{
		ResolutionKind: pgtype.Text{String: resolution.ResolutionKind, Valid: true},
		Output:         resolution.OutputJSON,
		Resolution:     resolution.ResolutionJSON,
		OrgID:          ids.ToPG(resolution.OrgID),
		ID:             ids.ToPG(waitpointID),
		Kind:           resolution.ExpectedKind,
	}
	return s.recordAndResolveWaitpoint(ctx, recordParams, resolveParams)
}

func (s *Server) recordAndResolveWaitpoint(ctx context.Context, recordParams db.RecordWaitpointResponseParams, resolveParams db.ResolveWaitpointParams) (waitpointResolveOutcome, error) {
	if s.tx == nil {
		if store, ok := s.db.(interface {
			RecordAndResolveWaitpoint(context.Context, db.RecordWaitpointResponseParams, db.ResolveWaitpointParams) ([]db.UnblockRunWaitsForWaitpointRow, error)
		}); ok {
			resumed, err := store.RecordAndResolveWaitpoint(ctx, recordParams, resolveParams)
			if err != nil {
				return waitpointResolveOutcome{}, err
			}
			return waitpointResolveOutcome{Resumed: len(resumed) > 0}, nil
		}
		return waitpointResolveOutcome{}, errors.New("transactional waitpoint storage is not configured")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return waitpointResolveOutcome{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := db.New(tx)
	if _, err := queries.RecordWaitpointResponse(ctx, recordParams); err != nil {
		return waitpointResolveOutcome{}, err
	}
	if _, err := queries.ResolveWaitpoint(ctx, resolveParams); err != nil {
		return waitpointResolveOutcome{}, err
	}
	resumed, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{
		OrgID:       resolveParams.OrgID,
		WaitpointID: resolveParams.ID,
	})
	if err != nil {
		return waitpointResolveOutcome{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return waitpointResolveOutcome{}, err
	}
	return waitpointResolveOutcome{Resumed: len(resumed) > 0}, nil
}

func waitpointActorResponseIdentity(actor auth.Actor) (string, string, error) {
	responseKey, err := auth.ActorPrincipal(actor)
	if err != nil {
		return "", "", err
	}
	switch actor.Kind {
	case auth.ActorKindSession:
		return responseKey, actor.UserID.String(), nil
	case auth.ActorKindAPIKey:
		return responseKey, responseKey, nil
	default:
		return "", "", errors.New("supported actor identity is required")
	}
}

func waitpointRequestFields(kind api.WorkerWaitpointKind, request json.RawMessage, displayText string) (db.WaitpointKind, string, error) {
	displayText = strings.TrimSpace(displayText)
	switch kind {
	case api.WorkerWaitpointKindHuman:
		return db.WaitpointKindHuman, displayText, nil
	case api.WorkerWaitpointKindDelay:
		return db.WaitpointKindDelay, displayText, nil
	default:
		return "", "", fmt.Errorf("unsupported waitpoint kind %q", kind)
	}
}

func waitpointRequestLinkedID(kind db.WaitpointKind, request json.RawMessage) (uuid.UUID, bool, error) {
	if kind != db.WaitpointKindHuman {
		return uuid.Nil, false, nil
	}
	trimmed := bytes.TrimSpace(request)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return uuid.Nil, false, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return uuid.Nil, false, err
	}
	raw, ok, err := optionalStringField(payload, "waitpoint_id")
	if err != nil {
		return uuid.Nil, false, err
	}
	if !ok {
		raw, ok, err = optionalStringField(payload, "waitpointId")
		if err != nil {
			return uuid.Nil, false, err
		}
	}
	if raw == "" {
		return uuid.Nil, false, nil
	}
	id, err := ids.Parse(raw)
	if err != nil {
		return uuid.Nil, false, errors.New("request.waitpoint_id must be a UUID")
	}
	return id, true, nil
}

func optionalStringField(payload map[string]json.RawMessage, name string) (string, bool, error) {
	rawJSON, ok := payload[name]
	if !ok {
		return "", false, nil
	}
	var value string
	if err := json.Unmarshal(rawJSON, &value); err != nil {
		return "", false, nil
	}
	return strings.TrimSpace(value), true, nil
}

func waitpointTimeout(kind db.WaitpointKind, timeoutSeconds *int32) (pgtype.Int4, error) {
	if timeoutSeconds == nil {
		if kind == db.WaitpointKindDelay {
			return pgtype.Int4{}, errors.New("timeout_seconds is required for delay waitpoints")
		}
		return pgtype.Int4{}, nil
	}
	if *timeoutSeconds <= 0 {
		return pgtype.Int4{}, errors.New("timeout_seconds must be positive")
	}
	return pgtype.Int4{Int32: *timeoutSeconds, Valid: true}, nil
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
		RuntimeVcpus:               pgInt4Ptr(runtimeSpec.VCPUCount),
		RuntimeMemoryMib:           pgInt4Ptr(runtimeSpec.MemoryMiB),
		RuntimeScratchDiskMib:      pgInt4Ptr(runtimeSpec.ScratchDiskMiB),
		CniProfile:                 *runtimeSpec.CNIProfile,
		ImageKey:                   pgTextPtr(runtimeInfo.ImageKey),
		WorkspaceArtifactDigest:    pgTextPtr(optionalTrimmedString(workspace.ArtifactDigest)),
		WorkspaceArtifactSizeBytes: pgtype.Int8{Int64: workspace.ArtifactSizeBytes, Valid: true},
		WorkspaceArtifactMediaType: pgTextPtr(optionalTrimmedString(workspace.ArtifactMediaType)),
		WorkspaceArtifactEncoding:  pgTextPtr(optionalTrimmedString(workspace.ArtifactEncoding)),
		WorkspaceMountPath:         pgTextPtr(optionalTrimmedString(workspace.MountPath)),
		WorkspaceVolumeKind:        pgTextPtr(optionalTrimmedString(workspace.VolumeKind)),
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
	if strings.TrimSpace(recovery.Runtime.ConfigDigest) != cas.DigestBytes(runtimeConfig) {
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
