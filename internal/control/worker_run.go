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
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) logRunWorkspaceReuseDiagnostics(ctx context.Context, orgID pgtype.UUID, runID pgtype.UUID, workerInstanceID pgtype.UUID, trigger string) {
	if s.log == nil || s.db == nil {
		return
	}
	diagnostics, err := s.db.ClassifyRunWorkspaceReuse(ctx, db.ClassifyRunWorkspaceReuseParams{
		OrgID:            orgID,
		RunID:            runID,
		WorkerInstanceID: workerInstanceID,
	})
	if isNoRows(err) {
		s.log.Info("worker run workspace reuse diagnostic unavailable",
			"trigger", trigger,
			"org_id", pgvalue.UUIDString(orgID),
			"run_id", pgvalue.UUIDString(runID),
			"worker_instance_id", pgvalue.UUIDString(workerInstanceID),
			"outcome", "run_workspace_scope_missing",
		)
		return
	}
	if err != nil {
		s.log.Warn("worker run workspace reuse diagnostic failed",
			"trigger", trigger,
			"org_id", pgvalue.UUIDString(orgID),
			"run_id", pgvalue.UUIDString(runID),
			"worker_instance_id", pgvalue.UUIDString(workerInstanceID),
			"error", err,
		)
		return
	}
	s.log.Info("worker run workspace reuse diagnostic",
		"trigger", trigger,
		"org_id", pgvalue.UUIDString(orgID),
		"run_id", pgvalue.UUIDString(runID),
		"workspace_id", pgvalue.UUIDString(diagnostics.WorkspaceID),
		"queued_workspace_mount_id", pgvalue.UUIDString(diagnostics.QueuedWorkspaceMountID),
		"resident_workspace_mount_id", pgvalue.UUIDString(diagnostics.ResidentWorkspaceMountID),
		"resident_workspace_mount_state", diagnostics.ResidentWorkspaceMountState.WorkspaceMountState,
		"resident_worker_instance_id", pgvalue.UUIDString(diagnostics.ResidentWorkerInstanceID),
		"dispatched_worker_instance_id", pgvalue.UUIDString(diagnostics.DispatchedWorkerInstanceID),
		"active_mount_count", diagnostics.ActiveMountCount,
		"mounted_mount_count", diagnostics.MountedMountCount,
		"mounting_mount_count", diagnostics.MountingMountCount,
		"active_write_lease_count", diagnostics.ActiveWriteLeaseCount,
		"worker_group_matches", diagnostics.WorkerGroupMatches,
		"worker_runtime_matches", diagnostics.WorkerRuntimeMatches,
		"worker_capacity_fits", diagnostics.WorkerCapacityFits,
		"outcome", diagnostics.Outcome,
	)
}

func (s *Server) workerStart(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request api.WorkerStartRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker start request JSON: %w", err)))
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
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	} else if err != nil {
		s.log.Error("worker queue lease lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("get queue lease"))
		return
	}
	status, err := s.db.StartRunLease(r.Context(), db.StartRunLeaseParams{
		OrgID:             pgvalue.UUID(leaseIDs.orgID),
		RunID:             pgvalue.UUID(leaseIDs.runID),
		RunLeaseID:        pgvalue.UUID(leaseIDs.runLeaseID),
		WorkerInstanceID:  pgvalue.UUID(worker.WorkerInstanceID),
		DispatchMessageID: leaseIDs.queueMessageID,
		DispatchLeaseID:   leaseIDs.queueLeaseID,
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	}
	if err != nil {
		s.log.Error("worker start failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("start run"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerStartResponse{RunID: request.Lease.RunID, Status: string(status.Status)})
}

func (s *Server) workerRenew(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	if s.dispatchQueue == nil {
		writeError(w, unavailable(errors.New("run dispatch queue is not configured")))
		return
	}
	var request api.WorkerRenewRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker renew request JSON: %w", err)))
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
	leaseRow, queueLease, err := s.workerExecutionLease(r.Context(), worker, leaseIDs)
	if err != nil {
		if isNoRows(err) {
			writeError(w, conflict(errors.New("worker run lease is stale")))
			return
		}
		s.log.Error("worker queue lease lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("get queue lease"))
		return
	}
	expiresAt := time.Now().Add(workerLeaseDuration)
	renewed, err := s.db.RenewRunLease(r.Context(), db.RenewRunLeaseParams{
		OrgID:             pgvalue.UUID(leaseIDs.orgID),
		RunID:             pgvalue.UUID(leaseIDs.runID),
		RunLeaseID:        pgvalue.UUID(leaseIDs.runLeaseID),
		WorkerInstanceID:  pgvalue.UUID(worker.WorkerInstanceID),
		DispatchMessageID: leaseRow.DispatchMessageID,
		DispatchLeaseID:   leaseRow.DispatchLeaseID,
		LeaseExpiresAt:    pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	}
	if err != nil {
		s.log.Error("worker renew failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("renew run lease"))
		return
	}
	if _, err := s.dispatchQueue.Renew(r.Context(), queueLease, expiresAt); err != nil {
		s.log.Warn("worker dispatch renew repair failed", "run_id", request.Lease.RunID, "error", err)
	}
	lease := api.WorkerRunLease{
		ID:                request.Lease.ID,
		OrgID:             request.Lease.OrgID,
		RunID:             request.Lease.RunID,
		WorkerInstanceID:  pgvalue.MustUUIDValue(renewed.WorkerInstanceID).String(),
		ProtocolVersion:   renewed.WorkerProtocolVersion,
		AttemptNumber:     renewed.AttemptNumber,
		DispatchMessageID: renewed.DispatchMessageID,
		DispatchLeaseID:   renewed.DispatchLeaseID,
		Trace: api.TraceContext{
			TraceID:     renewed.TraceID,
			SpanID:      renewed.SpanID,
			Traceparent: renewed.Traceparent,
		},
		ExpiresAt: pgvalue.Time(renewed.LeaseExpiresAt),
	}
	writeJSON(w, http.StatusOK, api.WorkerRenewResponse{Lease: lease})
}

func (s *Server) workerRelease(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	if s.dispatchQueue == nil {
		writeError(w, unavailable(errors.New("run dispatch queue is not configured")))
		return
	}
	var request api.WorkerReleaseRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker release request JSON: %w", err)))
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
	_, lease, err := s.workerExecutionLease(r.Context(), worker, leaseIDs)
	activeQueueLeaseFound := err == nil
	if err != nil {
		if !isNoRows(err) {
			s.log.Error("worker queue lease lookup failed", "run_id", request.Lease.RunID, "error", err)
			writeError(w, errors.New("get queue lease"))
			return
		}
	}
	status, exitCode, errorMessage, err := releaseFields(request.Result)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	output := releaseOutput(request.Result, status, exitCode)
	workspaceFields, err := releaseWorkspaceFields(request.Result.Workspace)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := s.verifyWorkerWorkspaceCommit(r.Context(), request.Result.Workspace); err != nil {
		writeError(w, conflict(err))
		return
	}
	_, terminalEventPayload, err := terminalRunEventForFields(status, exitCode, errorMessage, request.Result)
	if err != nil {
		writeError(w, errors.New("encode terminal run event"))
		return
	}
	var workspaceVersionPublicID string
	release := func() (db.ReleaseRunLeaseRow, error) {
		return s.db.ReleaseRunLease(r.Context(), db.ReleaseRunLeaseParams{
			OrgID:                       pgvalue.UUID(leaseIDs.orgID),
			RunID:                       pgvalue.UUID(leaseIDs.runID),
			RunLeaseID:                  pgvalue.UUID(leaseIDs.runLeaseID),
			WorkerInstanceID:            pgvalue.UUID(worker.WorkerInstanceID),
			DispatchMessageID:           leaseIDs.queueMessageID,
			DispatchLeaseID:             leaseIDs.queueLeaseID,
			RunStatus:                   status,
			WorkspaceLeaseID:            workspaceFields.leaseID,
			WorkspaceFencingToken:       workspaceFields.fencingToken,
			WorkspaceArtifactDigest:     workspaceFields.artifactDigest,
			WorkspaceArtifactSizeBytes:  workspaceFields.artifactSizeBytes,
			WorkspaceArtifactMediaType:  workspaceFields.artifactMediaType,
			WorkspaceArtifactEncoding:   workspaceFields.artifactEncoding,
			WorkspaceArtifactEntryCount: workspaceFields.artifactEntryCount,
			WorkspaceBaseVersionID:      workspaceFields.baseVersionID,
			ExitCode:                    exitCode,
			Output:                      output,
			ErrorMessage:                errorMessage,
			TerminalEventPayload:        terminalEventPayload,
			WorkspaceVersionPublicID:    pgvalue.Text(workspaceVersionPublicID),
		})
	}
	var run db.ReleaseRunLeaseRow
	if status == db.RunStatusSucceeded && workspaceFields.leaseID.Valid {
		run, err = createWithPublicID(r.Context(), []publicIDSlot{{prefix: publicid.WorkspaceVersion, value: &workspaceVersionPublicID}}, release)
	} else {
		run, err = release()
	}
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	}
	if err != nil {
		s.log.Error("worker release failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("release run"))
		return
	}
	if activeQueueLeaseFound {
		s.ackWorkerQueueLease(r.Context(), pgvalue.UUID(leaseIDs.runID), lease)
	}
	if run.SessionID.Valid && runStatusTerminal(run.Status) {
		s.sessionRunRequestWorkflow().reconcileAccepted(r.Context(), run.OrgID, run.ProjectID, run.EnvironmentID, run.SessionID)
	}
	writeJSON(w, http.StatusOK, api.WorkerReleaseResponse{RunID: request.Lease.RunID, Status: string(run.Status)})
}

func (s *Server) verifyWorkerWorkspaceCommit(ctx context.Context, workspace *api.WorkerWorkspace) error {
	if workspace == nil || workspace.Artifact == nil {
		return nil
	}
	if s.cas == nil {
		return errors.New("workspace CAS is not configured")
	}
	artifact := workspace.Artifact
	stat, err := s.cas.Stat(ctx, strings.TrimSpace(artifact.Digest))
	if err != nil {
		return fmt.Errorf("workspace artifact %s is missing from CAS: %w", artifact.Digest, err)
	}
	if stat.SizeBytes != artifact.SizeBytes {
		return fmt.Errorf("workspace artifact %s size mismatch", artifact.Digest)
	}
	if strings.TrimSpace(stat.MediaType) != strings.TrimSpace(artifact.MediaType) {
		return fmt.Errorf("workspace artifact %s media_type mismatch", artifact.Digest)
	}
	return nil
}

func (s *Server) appendWorkerEvent(w http.ResponseWriter, r *http.Request, lease api.WorkerRunLease, kind string, payload []byte) {
	worker, leaseIDs, ok := s.workerRunLeaseForWrite(w, r, lease)
	if !ok {
		return
	}
	_, err := s.db.AppendRunEventForExecution(r.Context(), db.AppendRunEventForExecutionParams{
		OrgID:            pgvalue.UUID(leaseIDs.orgID),
		WorkerGroupID:    worker.WorkerGroupID,
		RunID:            pgvalue.UUID(leaseIDs.runID),
		RunLeaseID:       pgvalue.UUID(leaseIDs.runLeaseID),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		Kind:             kind,
		Payload:          payload,
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	}
	if err != nil {
		s.log.Error("append worker event failed", "run_id", lease.RunID, "error", err)
		writeError(w, errors.New("append worker event"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerEventResponse{RunID: lease.RunID})
}

func (e terminalPayloadError) Error() string {
	return e.err.Error()
}

func (e terminalPayloadError) Unwrap() error {
	return e.err
}

func parseRequiredWorkspaceUUID(field string, raw string) (pgtype.UUID, error) {
	if strings.TrimSpace(raw) == "" {
		return pgtype.UUID{}, fmt.Errorf("%s is required", field)
	}
	return parseOptionalWorkspaceUUID(field, raw)
}

func parseOptionalWorkspaceUUID(field string, raw string) (pgtype.UUID, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return pgtype.UUID{}, nil
	}
	id, err := uuid.Parse(trimmed)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%s must be a UUID", field)
	}
	return pgvalue.UUID(id), nil
}

func requiredUUIDString(value pgtype.UUID, field string) (string, error) {
	if !value.Valid {
		return "", fmt.Errorf("%s is required", field)
	}
	return pgvalue.MustUUIDValue(value).String(), nil
}
