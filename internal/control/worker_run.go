package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	started, err := s.db.GetStartedRunLease(r.Context(), db.GetStartedRunLeaseParams{
		OrgID: pgvalue.UUID(leaseIDs.orgID), RunID: pgvalue.UUID(leaseIDs.runID), RunLeaseID: pgvalue.UUID(leaseIDs.runLeaseID),
		LeaseSequence: leaseIDs.leaseSequence, TaskAttemptNumber: leaseIDs.attemptNumber,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
		RuntimeInstanceID: pgvalue.UUID(leaseIDs.runtimeInstanceID), NetworkSlotID: pgvalue.UUID(leaseIDs.networkSlotID),
		NetworkSlotGeneration: leaseIDs.networkSlotGeneration, WorkerProtocolVersion: leaseIDs.protocolVersion,
		ExpectedRunStateVersion: leaseIDs.snapshotVersion,
	})
	if err == nil {
		startedLease := request.Lease
		startedLease.SnapshotVersion++
		startedLease.ExpiresAt = pgvalue.Time(started.ExpiresAt)
		writeJSON(w, http.StatusOK, api.WorkerStartResponse{RunID: request.Lease.RunID, Status: string(started.State), Lease: startedLease})
		return
	}
	if !isNoRows(err) {
		s.log.Error("worker started run lease lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("get started run lease"))
		return
	}
	if _, err := s.workerExecutionLease(r.Context(), worker, leaseIDs); isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	} else if err != nil {
		s.log.Error("worker run lease lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("get run lease"))
		return
	}
	expiresAt := time.Now().Add(workerLeaseDuration)
	status, err := s.db.StartRunLease(r.Context(), db.StartRunLeaseParams{
		ExpiresAt: pgvalue.Timestamptz(expiresAt), OrgID: pgvalue.UUID(leaseIDs.orgID),
		RunID: pgvalue.UUID(leaseIDs.runID), RunLeaseID: pgvalue.UUID(leaseIDs.runLeaseID),
		LeaseSequence: leaseIDs.leaseSequence, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: leaseIDs.workerEpoch, RuntimeInstanceID: pgvalue.UUID(leaseIDs.runtimeInstanceID),
		NetworkSlotID: pgvalue.UUID(leaseIDs.networkSlotID), NetworkSlotGeneration: leaseIDs.networkSlotGeneration,
		ExpectedRunStateVersion: leaseIDs.snapshotVersion,
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
	startedLease := request.Lease
	startedLease.SnapshotVersion++
	startedLease.ExpiresAt = pgvalue.Time(status.ExpiresAt)
	writeJSON(w, http.StatusOK, api.WorkerStartResponse{RunID: request.Lease.RunID, Status: string(status.State), Lease: startedLease})
}

func (s *Server) workerRenew(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
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
	err = s.workerCurrentRunningLease(r.Context(), worker, leaseIDs)
	if err != nil {
		if isNoRows(err) {
			writeError(w, conflict(errors.New("worker run lease is stale")))
			return
		}
		s.log.Error("worker run lease lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("get run lease"))
		return
	}
	expiresAt := time.Now().Add(workerLeaseDuration)
	renewed, err := s.db.RenewRunLease(r.Context(), db.RenewRunLeaseParams{
		ExpiresAt: pgvalue.Timestamptz(expiresAt), OrgID: pgvalue.UUID(leaseIDs.orgID),
		RunID: pgvalue.UUID(leaseIDs.runID), RunLeaseID: pgvalue.UUID(leaseIDs.runLeaseID),
		LeaseSequence: leaseIDs.leaseSequence, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: leaseIDs.workerEpoch, RuntimeInstanceID: pgvalue.UUID(leaseIDs.runtimeInstanceID),
		NetworkSlotID: pgvalue.UUID(leaseIDs.networkSlotID), NetworkSlotGeneration: leaseIDs.networkSlotGeneration,
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
	lease := request.Lease
	lease.ExpiresAt = pgvalue.Time(renewed.ExpiresAt)
	writeJSON(w, http.StatusOK, api.WorkerRenewResponse{Lease: lease})
}

func (s *Server) workerRelease(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
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
	fingerprint, err := terminalRequestFingerprint("run.release", request.Result)
	if err != nil {
		writeError(w, errors.New("fingerprint worker release"))
		return
	}
	terminal, err := s.db.GetRunLeaseTerminalResult(r.Context(), db.GetRunLeaseTerminalResultParams{
		OrgID: pgvalue.UUID(leaseIDs.orgID), RunID: pgvalue.UUID(leaseIDs.runID), RunLeaseID: pgvalue.UUID(leaseIDs.runLeaseID),
		LeaseSequence: leaseIDs.leaseSequence, TaskAttemptNumber: leaseIDs.attemptNumber,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
		RuntimeInstanceID: pgvalue.UUID(leaseIDs.runtimeInstanceID), NetworkSlotID: pgvalue.UUID(leaseIDs.networkSlotID),
		NetworkSlotGeneration: leaseIDs.networkSlotGeneration, WorkerProtocolVersion: leaseIDs.protocolVersion,
	})
	if err == nil {
		if !terminal.TerminalRequestFingerprint.Valid || terminal.TerminalRequestFingerprint.String != fingerprint {
			writeError(w, conflict(errors.New("worker run lease already has a different terminal result")))
			return
		}
		writeJSON(w, http.StatusOK, api.WorkerReleaseResponse{RunID: request.Lease.RunID, Status: string(terminal.RunStatus)})
		return
	}
	if !isNoRows(err) {
		writeError(w, errors.New("get terminal run lease"))
		return
	}
	err = s.workerCurrentRunningLease(r.Context(), worker, leaseIDs)
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("get running run lease"))
		return
	}
	status, exitCode, errorMessage, err := releaseFields(request.Result)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	output := releaseOutput(request.Result, status, exitCode)
	if request.Result.ActiveDurationMs < 0 {
		writeError(w, badRequest(errors.New("result.active_duration_ms must be non-negative")))
		return
	}
	workspaceFields, err := releaseWorkspaceFields(request.Result.Workspace)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := s.verifyWorkerWorkspaceCommit(r.Context(), request.Result.Workspace); err != nil {
		writeError(w, conflict(err))
		return
	}
	currentRun, err := s.db.GetRun(r.Context(), db.GetRunParams{OrgID: pgvalue.UUID(leaseIDs.orgID), ID: pgvalue.UUID(leaseIDs.runID)})
	if err != nil {
		writeError(w, conflict(errors.New("run state changed before release")))
		return
	}
	leaseState := db.RunLeaseStateCompleted
	reasonCode := "completed"
	terminalOutcome := db.RunTerminalOutcomeSucceeded
	var terminalError []byte
	if status == db.RunStatusFailed {
		leaseState, reasonCode, terminalOutcome = db.RunLeaseStateFailed, "failed", db.RunTerminalOutcomeFailed
	} else if status == db.RunStatusCancelled {
		leaseState, reasonCode, terminalOutcome = db.RunLeaseStateCancelled, "cancelled", db.RunTerminalOutcomeCancelled
	}
	if errorMessage.Valid {
		terminalError, _ = json.Marshal(map[string]string{"message": errorMessage.String})
	}
	var workspaceVersionPublicID string
	release := func() (db.ReleaseRunLeaseRow, error) {
		return s.db.ReleaseRunLease(r.Context(), db.ReleaseRunLeaseParams{
			OrgID: pgvalue.UUID(leaseIDs.orgID), RunID: pgvalue.UUID(leaseIDs.runID),
			WorkspaceLeaseID: workspaceFields.leaseID, WorkspaceFencingToken: workspaceFields.fencingToken,
			WorkspaceFencingGeneration: workspaceFields.fencingGeneration,
			State:                      leaseState, ReasonCode: pgvalue.Text(reasonCode), Error: terminalError,
			TerminalRequestFingerprint: fingerprint,
			RunLeaseID:                 pgvalue.UUID(leaseIDs.runLeaseID), LeaseSequence: leaseIDs.leaseSequence,
			WorkerGroupID:    request.Lease.WorkerGroupID,
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: leaseIDs.workerEpoch,
			WorkerProtocolVersion: request.Lease.ProtocolVersion,
			RuntimeInstanceID:     pgvalue.UUID(leaseIDs.runtimeInstanceID), NetworkSlotID: pgvalue.UUID(leaseIDs.networkSlotID),
			NetworkSlotGeneration: leaseIDs.networkSlotGeneration, RunStatus: status,
			WorkspaceArtifactDigest:     workspaceFields.artifactDigest,
			WorkspaceArtifactSizeBytes:  workspaceFields.artifactSizeBytes,
			WorkspaceArtifactMediaType:  workspaceFields.artifactMediaType,
			WorkspaceArtifactEncoding:   workspaceFields.artifactEncoding,
			WorkspaceArtifactEntryCount: workspaceFields.artifactEntryCount,
			WorkspaceBaseVersionID:      workspaceFields.baseVersionID,
			ExitCode:                    exitCode,
			Output:                      output,
			ErrorMessage:                errorMessage,
			WorkspaceVersionPublicID:    pgvalue.Text(workspaceVersionPublicID),
			TerminalOutcome:             db.NullRunTerminalOutcome{RunTerminalOutcome: terminalOutcome, Valid: true},
			ExpectedRunStateVersion:     currentRun.StateVersion,
			ActiveDurationMs:            request.Result.ActiveDurationMs,
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
	if run.SessionID.Valid && runStatusTerminal(run.Status) {
		s.sessionContinuationRequestWorkflow().reconcileAccepted(r.Context(), run.OrgID, run.ProjectID, run.EnvironmentID, run.SessionID)
	}
	writeJSON(w, http.StatusOK, api.WorkerReleaseResponse{RunID: request.Lease.RunID, Status: string(run.Status)})
}

func terminalRequestFingerprint(scope string, payload any) (string, error) {
	body, err := json.Marshal(struct {
		Scope   string `json:"scope"`
		Payload any    `json:"payload"`
	}{Scope: scope, Payload: payload})
	if err != nil {
		return "", err
	}
	canonical, err := canonicalJSON(body)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
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
