package control

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5/pgtype"
)

const workerLeaseDuration = 5 * time.Minute

func (s *Server) workerLease(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	if s.dispatchQueue == nil {
		writeError(w, unavailable(errors.New("run queue item queue is not configured")))
		return
	}
	var request api.WorkerRunLeaseRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker run lease request JSON: %w", err)))
		return
	}
	capabilities, err := normalizeWorkerCapabilities(request.Capabilities)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}

	worker := workerFromContext(r.Context())
	if _, err := s.db.UpsertWorkerInstanceHeartbeat(r.Context(), workerInstanceHeartbeatParams(worker, capabilities)); err != nil {
		s.log.Error("worker heartbeat failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("record worker heartbeat"))
		return
	}
	if err := s.db.EnsureRuntimeReleaseSelection(r.Context(), capabilities.RuntimeID); err != nil {
		s.log.Error("ensure runtime release selection failed", "worker_instance_id", worker.WorkerInstanceID.String(), "runtime_id", capabilities.RuntimeID, "error", err)
		writeError(w, errors.New("select runtime release"))
		return
	}
	if err := s.markStaleWorkspaceMaterializationsLost(r.Context()); err != nil {
		s.log.Error("mark stale workspace materializations lost failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("reap stale workspace materializations"))
		return
	}
	capacity, err := s.db.GetWorkerInstanceRunDispatchCapacity(r.Context(), pgvalue.UUID(worker.WorkerInstanceID))
	if isNoRows(err) {
		writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{})
		return
	}
	if err != nil {
		s.log.Error("worker capacity lookup failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("get worker capacity"))
		return
	}
	if capacity.AvailableExecutionSlots <= 0 || capacity.AvailableMilliCpu <= 0 || capacity.AvailableMemoryMib <= 0 {
		writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{})
		return
	}
	runClaimer, err := dispatch.NewClaimer(s.db, s.dispatchQueue)
	if err != nil {
		writeError(w, unavailable(errors.New("run queue item queue is not configured")))
		return
	}
	dequeueRequest := dispatch.DequeueRequest{
		WorkerInstanceID: worker.WorkerInstanceID.String(),
		Available: compute.ResourceVector{
			MilliCPU:  capacity.AvailableMilliCpu,
			MemoryMiB: capacity.AvailableMemoryMib,
			DiskMiB:   capacity.AvailableDiskMib,
			Slots:     capacity.AvailableExecutionSlots,
		},
		Runtime: compute.RuntimeSelector{
			ID:              capabilities.RuntimeID,
			Arch:            capabilities.RuntimeArch,
			ABI:             capabilities.RuntimeABI,
			KernelDigest:    capabilities.KernelDigest,
			InitramfsDigest: capabilities.InitramfsDigest,
			RootfsDigest:    capabilities.RootfsDigest,
			CNIProfile:      capabilities.CNIProfile,
		},
		Region:      capabilities.Region,
		Labels:      capabilities.Labels,
		MaxMessages: 1,
	}
	var queueLease dispatch.ClaimedRun
	var leasedRun db.LeaseRunLeaseRow
	const scopePageSize int32 = 100
	scanSeed := int32(s.workerLeaseScanSeed.Add(1) & 0x7fffffff)
	scopeSelector := dispatch.RoundRobinQueueScopeSelector{}
	foundLease := false
	for rowOffset := int32(0); !foundLease; rowOffset += scopePageSize {
		scopeRows, err := s.db.ListQueueScopes(r.Context(), db.ListQueueScopesParams{
			WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID),
			ScanSeed:      fmt.Sprint(scanSeed),
			RowOffset:     rowOffset,
			RowLimit:      scopePageSize,
		})
		if err != nil {
			s.log.Error("worker queue scope lookup failed", "error", err)
			writeError(w, errors.New("list worker queue scopes"))
			return
		}
		if len(scopeRows) == 0 {
			break
		}
		scopes := make([]dispatch.QueueScope, 0, len(scopeRows))
		for _, row := range scopeRows {
			scopes = append(scopes, dispatch.QueueScope{
				OrgID:         row.OrgID,
				ProjectID:     row.ProjectID,
				EnvironmentID: row.EnvironmentID,
				QueueName:     row.QueueName,
			})
		}
		// Worker leasing exits after one claim, so keep scope ordering page-bounded.
		scopes = scopeSelector.Order(scopes)
		for _, scope := range scopes {
			orgID := pgvalue.MustUUIDValue(scope.OrgID)
			if err := dispatch.SweepExpiredForOrg(r.Context(), s.db, scope.OrgID); err != nil {
				s.log.Warn("sweep expired run leases failed", "org_id", orgID.String(), "error", err)
			}
			dequeueRequest.OrgID = orgID.String()
			dequeueRequest.ProjectID = pgvalue.MustUUIDValue(scope.ProjectID).String()
			dequeueRequest.EnvironmentID = pgvalue.MustUUIDValue(scope.EnvironmentID).String()
			for _, queueName := range dispatch.QueueNamesForRuntime(scope.QueueName, dequeueRequest.Runtime) {
				dequeueRequest.QueueName = queueName
				candidateLease, err := runClaimer.Claim(r.Context(), dispatch.ClaimRequest{DequeueRequest: dequeueRequest})
				if errors.Is(err, dispatch.ErrNoClaim) {
					continue
				}
				if err != nil {
					s.log.Error("worker queue lease failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
					writeError(w, errors.New("lease run queue item"))
					return
				}
				if candidateLease.Lease.MessageID == "" {
					continue
				}
				sessionSpanID, err := tracing.NewSpanID()
				if err != nil {
					s.requeueWorkerQueueItem(r.Context(), worker, candidateLease.Entry.RunID, candidateLease.Lease, dispatch.NackReasonRetry, err.Error())
					s.log.Error("worker run trace span failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
					writeError(w, errors.New("lease run"))
					return
				}
				candidateRun, err := s.db.LeaseRunLease(r.Context(), db.LeaseRunLeaseParams{
					OrgID:             candidateLease.Entry.OrgID,
					RunID:             candidateLease.Entry.RunID,
					WorkerInstanceID:  pgvalue.UUID(worker.WorkerInstanceID),
					RunLeaseID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
					DispatchMessageID: pgtype.Text{String: candidateLease.Lease.MessageID, Valid: true},
					DispatchLeaseID:   candidateLease.Lease.ID,
					DispatchAttempt:   candidateLease.Lease.AttemptNumber,
					LeaseExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(workerLeaseDuration), Valid: true},
					RunLeaseSpanID:    sessionSpanID,
				})
				if err == nil {
					queueLease = candidateLease
					leasedRun = candidateRun
					foundLease = true
					break
				}
				if isNoRows(err) {
					s.requeueWorkerQueueItem(r.Context(), worker, candidateLease.Entry.RunID, candidateLease.Lease, dispatch.NackReasonLeaseConflict, "execution lease conflict")
					continue
				}
				s.requeueWorkerQueueItem(r.Context(), worker, candidateLease.Entry.RunID, candidateLease.Lease, dispatch.NackReasonRetry, err.Error())
				s.log.Error("worker run lease failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
				writeError(w, errors.New("lease run"))
				return
			}
			if foundLease {
				break
			}
		}
		if int32(len(scopeRows)) < scopePageSize {
			break
		}
	}
	if !foundLease {
		writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{})
		return
	}

	lease := workerRunLeaseResponse(leasedRun)
	run, err := s.workerRunFromLease(r.Context(), leasedRun)
	if err != nil {
		if failure, ok := terminalPayloadFailure(err); ok {
			if failErr := s.failLeasedRunPayload(r.Context(), leasedRun, queueLease.Lease, failure); failErr != nil {
				s.log.Error("fail worker run payload failed", "run_id", pgvalue.MustUUIDValue(leasedRun.ID).String(), "run_lease_id", pgvalue.MustUUIDValue(leasedRun.RunLeaseID).String(), "error", failErr)
				writeError(w, errors.New("fail worker run payload"))
				return
			}
			s.log.Warn("terminal worker run payload failed", "run_id", pgvalue.MustUUIDValue(leasedRun.ID).String(), "run_lease_id", pgvalue.MustUUIDValue(leasedRun.RunLeaseID).String(), "failure_kind", failure.kind, "error", err)
			writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{})
			return
		}
		if abandonErr := s.db.AbandonLeasedRunLease(r.Context(), db.AbandonLeasedRunLeaseParams{
			OrgID:            leasedRun.OrgID,
			RunID:            leasedRun.ID,
			RunLeaseID:       leasedRun.RunLeaseID,
			WorkerInstanceID: leasedRun.RunLeaseWorkerInstanceID,
		}); abandonErr != nil {
			s.log.Error("abandon worker run lease failed", "run_id", pgvalue.MustUUIDValue(leasedRun.ID).String(), "run_lease_id", pgvalue.MustUUIDValue(leasedRun.RunLeaseID).String(), "error", abandonErr)
		}
		s.requeueWorkerQueueItem(r.Context(), worker, leasedRun.ID, queueLease.Lease, dispatch.NackReasonRetry, err.Error())
		s.log.Error("build worker run payload failed", "run_id", pgvalue.MustUUIDValue(leasedRun.ID).String(), "run_lease_id", pgvalue.MustUUIDValue(leasedRun.RunLeaseID).String(), "error", err)
		writeError(w, badGateway(errors.New("build worker run payload")))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{Lease: &lease, Run: &run})
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
		OrgID:            pgvalue.UUID(leaseIDs.orgID),
		RunID:            pgvalue.UUID(leaseIDs.runID),
		RunLeaseID:       pgvalue.UUID(leaseIDs.runLeaseID),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
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
	writeJSON(w, http.StatusOK, api.WorkerStartResponse{RunID: request.Lease.RunID, Status: string(status)})
}

func (s *Server) workerRenew(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	if s.dispatchQueue == nil {
		writeError(w, unavailable(errors.New("run queue item queue is not configured")))
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
	if _, err := s.db.RenewRunQueueReservation(r.Context(), db.RenewRunQueueReservationParams{
		OrgID:                pgvalue.UUID(leaseIDs.orgID),
		RunID:                pgvalue.UUID(leaseIDs.runID),
		WorkerInstanceID:     pgvalue.UUID(worker.WorkerInstanceID),
		DispatchMessageID:    pgtype.Text{String: leaseRow.DispatchMessageID, Valid: true},
		ReservationExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	} else if err != nil {
		s.log.Error("worker queue lease renewal failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("renew queue lease"))
		return
	}
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
		writeError(w, unavailable(errors.New("run queue item queue is not configured")))
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
	terminalEventKind, terminalEventPayload, err := terminalRunEventForFields(status, exitCode, errorMessage, request.Result)
	if err != nil {
		writeError(w, errors.New("encode terminal run event"))
		return
	}
	run, err := s.db.ReleaseRunLease(r.Context(), db.ReleaseRunLeaseParams{
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
		WorkspaceMountPath:          workspaceFields.mountPath,
		WorkspaceBaseVersionID:      workspaceFields.baseVersionID,
		AttemptStatus:               db.RunAttemptStatus(status),
		ExitCode:                    exitCode,
		Output:                      output,
		ErrorMessage:                errorMessage,
		TerminalEventKind:           terminalEventKind,
		TerminalEventPayload:        terminalEventPayload,
	})
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
		s.reconcileAcceptedSessionRunRequests(r.Context(), run.OrgID, run.ProjectID, run.EnvironmentID, run.SessionID)
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

func (s *Server) ackWorkerQueueLease(ctx context.Context, runID pgtype.UUID, lease dispatch.Lease) {
	if err := s.dispatchQueue.Ack(ctx, lease); err != nil {
		s.log.Warn("complete queue lease failed", "run_id", pgvalue.MustUUIDValue(runID).String(), "error", err)
	}
}

func (s *Server) requeueWorkerQueueItem(ctx context.Context, worker workerActor, runID pgtype.UUID, lease dispatch.Lease, reason dispatch.NackReason, lastError string) {
	orgID, err := uuid.Parse(lease.Message.OrgID)
	if err != nil {
		s.log.Warn("requeue run queue item failed", "run_id", pgvalue.MustUUIDValue(runID).String(), "reason", reason, "error", err)
		if nackErr := s.dispatchQueue.Nack(ctx, lease, dispatch.NackReasonInvalid); nackErr != nil {
			s.log.Warn("requeue queue lease failed", "run_id", pgvalue.MustUUIDValue(runID).String(), "reason", dispatch.NackReasonInvalid, "error", nackErr)
		}
		return
	}
	if _, err := s.db.RequeueRunQueueItem(ctx, db.RequeueRunQueueItemParams{
		OrgID:             pgvalue.UUID(orgID),
		RunID:             runID,
		WorkerInstanceID:  pgvalue.UUID(worker.WorkerInstanceID),
		DispatchMessageID: pgtype.Text{String: lease.MessageID, Valid: true},
		LastError:         strings.TrimSpace(lastError),
	}); err != nil {
		s.log.Warn("requeue run queue item failed", "run_id", pgvalue.MustUUIDValue(runID).String(), "reason", reason, "error", err)
		nackReason := reason
		if isNoRows(err) {
			nackReason = dispatch.NackReasonInvalid
		}
		if nackErr := s.dispatchQueue.Nack(ctx, lease, nackReason); nackErr != nil {
			s.log.Warn("requeue queue lease failed", "run_id", pgvalue.MustUUIDValue(runID).String(), "reason", nackReason, "error", nackErr)
		}
		return
	}
	if err := s.dispatchQueue.Nack(ctx, lease, dispatch.NackReasonInvalid); err != nil {
		s.log.Warn("discard stale queue lease failed", "run_id", pgvalue.MustUUIDValue(runID).String(), "reason", reason, "error", err)
	}
}

func (s *Server) workerAppendLogs(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerAppendLogRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker log request JSON: %w", err)))
		return
	}
	content, err := base64.StdEncoding.DecodeString(request.ContentBase64)
	if err != nil {
		writeError(w, badRequest(errors.New("log content is not valid base64")))
		return
	}
	kind := "log.stdout"
	switch request.Stream {
	case api.WorkerLogStreamStdout:
	case api.WorkerLogStreamStderr:
		kind = "log.stderr"
	default:
		writeError(w, badRequest(errors.New("stream must be stdout or stderr")))
		return
	}
	if request.ObservedSeq > uint64(^uint64(0)>>1) {
		writeError(w, badRequest(errors.New("observed_seq is too large")))
		return
	}
	worker, leaseIDs, ok := s.workerRunLeaseForWrite(w, r, request.Lease)
	if !ok {
		return
	}
	payload, err := json.Marshal(workerLogChunkPayload{
		RunID:       request.Lease.RunID,
		Stream:      request.Stream,
		ObservedSeq: request.ObservedSeq,
		Bytes:       len(content),
	})
	if err != nil {
		writeError(w, errors.New("encode worker log event"))
		return
	}
	_, err = s.db.AppendRunLogChunk(r.Context(), db.AppendRunLogChunkParams{
		OrgID:            pgvalue.UUID(leaseIDs.orgID),
		RunID:            pgvalue.UUID(leaseIDs.runID),
		RunLeaseID:       pgvalue.UUID(leaseIDs.runLeaseID),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		Stream:           db.RunLogStream(request.Stream),
		ObservedSeq:      int64(request.ObservedSeq),
		Content:          content,
		Kind:             kind,
		Payload:          payload,
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	}
	if err != nil {
		s.log.Error("append worker logs failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("append worker logs"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerEventResponse{RunID: request.Lease.RunID})
}

func (s *Server) workerRecordLogEntry(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerRecordLogEntryRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker log entry request JSON: %w", err)))
		return
	}
	payload, err := json.Marshal(workerMessagePayload{Message: request.Entry})
	if err != nil {
		writeError(w, errors.New("encode worker log entry"))
		return
	}
	s.appendWorkerEvent(w, r, request.Lease, "log", payload)
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

func (s *Server) workerRunLeaseForWrite(w http.ResponseWriter, r *http.Request, lease api.WorkerRunLease) (workerActor, workerRunLeaseIDs, bool) {
	leaseIDs, err := parseWorkerRunLease(lease)
	if err != nil {
		writeError(w, badRequest(err))
		return workerActor{}, workerRunLeaseIDs{}, false
	}
	worker := workerFromContext(r.Context())
	if lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, forbidden(errors.New("worker run lease belongs to another worker")))
		return workerActor{}, workerRunLeaseIDs{}, false
	}
	if _, _, err := s.workerExecutionLease(r.Context(), worker, leaseIDs); isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return workerActor{}, workerRunLeaseIDs{}, false
	} else if err != nil {
		s.log.Error("worker queue lease lookup failed", "run_id", lease.RunID, "error", err)
		writeError(w, errors.New("get queue lease"))
		return workerActor{}, workerRunLeaseIDs{}, false
	}
	return worker, leaseIDs, true
}

type payloadFailure struct {
	kind    string
	message string
}

type workerMessagePayload struct {
	Message string `json:"message"`
}

type workerLogChunkPayload struct {
	Bytes       int                 `json:"bytes"`
	ObservedSeq uint64              `json:"observed_seq"`
	RunID       string              `json:"run_id"`
	Stream      api.WorkerLogStream `json:"stream"`
}

type workerHeartbeatPayload struct {
	CNIProfile      string `json:"cni_profile"`
	InitramfsDigest string `json:"initramfs_digest"`
	KernelDigest    string `json:"kernel_digest"`
	RootfsDigest    string `json:"rootfs_digest"`
	RuntimeABI      string `json:"runtime_abi"`
	RuntimeArch     string `json:"runtime_arch"`
	RuntimeID       string `json:"runtime_id"`
}

type runCompletedPayload struct {
	ExitCode int32 `json:"exit_code"`
}

type runFailurePayload struct {
	Detail      any    `json:"detail"`
	FailureKind string `json:"failure_kind"`
}

type taskFailedDetailPayload struct {
	ExitCode int32 `json:"exit_code"`
}

type workerFailureDetailPayload struct {
	LimitSeconds *int32 `json:"limit_seconds,omitempty"`
	Message      string `json:"message"`
}

type runCancelledPayload struct {
	Reason string `json:"reason"`
}

type terminalPayloadError struct {
	kind string
	err  error
}

func (e terminalPayloadError) Error() string {
	return e.err.Error()
}

func (e terminalPayloadError) Unwrap() error {
	return e.err
}

func terminalPayload(kind string, err error) error {
	return terminalPayloadError{kind: kind, err: err}
}

func terminalPayloadFailure(err error) (payloadFailure, bool) {
	var terminal terminalPayloadError
	if !errors.As(err, &terminal) {
		return payloadFailure{}, false
	}
	return payloadFailure{kind: terminal.kind, message: terminal.err.Error()}, true
}

func (s *Server) failLeasedRunPayload(ctx context.Context, row db.LeaseRunLeaseRow, lease dispatch.Lease, failure payloadFailure) error {
	kind, payload, err := payloadFailureRunEvent(failure)
	if err != nil {
		return err
	}
	_, err = s.db.ReleaseRunLease(ctx, db.ReleaseRunLeaseParams{
		OrgID:                row.OrgID,
		RunID:                row.ID,
		RunLeaseID:           row.RunLeaseID,
		WorkerInstanceID:     row.RunLeaseWorkerInstanceID,
		DispatchMessageID:    row.RunLeaseDispatchMessageID,
		DispatchLeaseID:      row.RunLeaseDispatchLeaseID,
		RunStatus:            db.RunStatusFailed,
		AttemptStatus:        db.RunAttemptStatusFailed,
		ExitCode:             pgtype.Int4{},
		ErrorMessage:         pgtype.Text{String: failure.message, Valid: true},
		TerminalEventKind:    kind,
		TerminalEventPayload: payload,
	})
	if err != nil {
		return err
	}
	s.ackWorkerQueueLease(ctx, row.ID, lease)
	return nil
}

func payloadFailureRunEvent(failure payloadFailure) (string, []byte, error) {
	payload, err := json.Marshal(runFailurePayload{
		FailureKind: failure.kind,
		Detail:      workerMessagePayload{Message: failure.message},
	})
	if err != nil {
		return "", nil, err
	}
	return "run.failed", payload, nil
}

type workerRunLeaseIDs struct {
	orgID           uuid.UUID
	runLeaseID      uuid.UUID
	runID           uuid.UUID
	protocolVersion string
	attemptNumber   int32
	queueMessageID  string
	queueLeaseID    string
}

func parseWorkerRunLease(lease api.WorkerRunLease) (workerRunLeaseIDs, error) {
	if strings.TrimSpace(lease.WorkerInstanceID) == "" {
		return workerRunLeaseIDs{}, errors.New("lease.worker_instance_id is required")
	}
	orgID, err := uuid.Parse(lease.OrgID)
	if err != nil {
		return workerRunLeaseIDs{}, errors.New("lease.org_id must be a UUID")
	}
	runLeaseID, err := uuid.Parse(lease.ID)
	if err != nil {
		return workerRunLeaseIDs{}, errors.New("lease.id must be a UUID")
	}
	runID, err := uuid.Parse(lease.RunID)
	if err != nil {
		return workerRunLeaseIDs{}, errors.New("lease.run_id must be a UUID")
	}
	if lease.AttemptNumber <= 0 {
		return workerRunLeaseIDs{}, errors.New("lease.attempt_number must be positive")
	}
	protocolVersion := strings.TrimSpace(lease.ProtocolVersion)
	if protocolVersion == "" {
		return workerRunLeaseIDs{}, errors.New("lease.protocol_version is required")
	}
	queueMessageID := strings.TrimSpace(lease.DispatchMessageID)
	if queueMessageID == "" {
		return workerRunLeaseIDs{}, errors.New("lease.dispatch_message_id is required")
	}
	queueLeaseID := strings.TrimSpace(lease.DispatchLeaseID)
	if queueLeaseID == "" {
		return workerRunLeaseIDs{}, errors.New("lease.dispatch_lease_id is required")
	}
	return workerRunLeaseIDs{
		orgID:           orgID,
		runLeaseID:      runLeaseID,
		runID:           runID,
		protocolVersion: protocolVersion,
		attemptNumber:   lease.AttemptNumber,
		queueMessageID:  queueMessageID,
		queueLeaseID:    queueLeaseID,
	}, nil
}

func (s *Server) workerExecutionLease(ctx context.Context, worker workerActor, leaseIDs workerRunLeaseIDs) (db.GetRunLeaseQueueLeaseRow, dispatch.Lease, error) {
	row, err := s.db.GetRunLeaseQueueLease(ctx, db.GetRunLeaseQueueLeaseParams{
		OrgID:            pgvalue.UUID(leaseIDs.orgID),
		RunID:            pgvalue.UUID(leaseIDs.runID),
		RunLeaseID:       pgvalue.UUID(leaseIDs.runLeaseID),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
	})
	if err != nil {
		return db.GetRunLeaseQueueLeaseRow{}, dispatch.Lease{}, err
	}
	if row.WorkerProtocolVersion != leaseIDs.protocolVersion || row.DispatchMessageID != leaseIDs.queueMessageID || row.DispatchLeaseID != leaseIDs.queueLeaseID || row.AttemptNumber != leaseIDs.attemptNumber {
		return db.GetRunLeaseQueueLeaseRow{}, dispatch.Lease{}, errRecordNotFound
	}
	lease := dispatch.Lease{
		ID:               row.DispatchLeaseID,
		MessageID:        row.DispatchMessageID,
		WorkerInstanceID: worker.WorkerInstanceID.String(),
		AttemptNumber:    row.DispatchAttempt,
		ExpiresAt:        pgvalue.Time(row.LeaseExpiresAt),
		Message: dispatch.Message{
			OrgID:     leaseIDs.orgID.String(),
			RunID:     pgvalue.MustUUIDValue(row.RunID).String(),
			QueueName: row.QueueName,
		},
	}
	return row, lease, nil
}

func (s *Server) workerCurrentRunningLease(ctx context.Context, worker workerActor, leaseIDs workerRunLeaseIDs) (db.GetCurrentRunningRunLeaseRow, error) {
	row, err := s.db.GetCurrentRunningRunLease(ctx, db.GetCurrentRunningRunLeaseParams{
		OrgID:            pgvalue.UUID(leaseIDs.orgID),
		RunID:            pgvalue.UUID(leaseIDs.runID),
		RunLeaseID:       pgvalue.UUID(leaseIDs.runLeaseID),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
	})
	if err != nil {
		return db.GetCurrentRunningRunLeaseRow{}, err
	}
	if row.WorkerProtocolVersion != leaseIDs.protocolVersion || row.DispatchMessageID != leaseIDs.queueMessageID || row.DispatchLeaseID != leaseIDs.queueLeaseID || row.AttemptNumber != leaseIDs.attemptNumber {
		return db.GetCurrentRunningRunLeaseRow{}, errRecordNotFound
	}
	return row, nil
}

func releaseFields(result api.WorkerReleaseResult) (db.RunStatus, pgtype.Int4, pgtype.Text, error) {
	switch result.Kind {
	case "completed":
		if result.ExitCode == nil {
			return "", pgtype.Int4{}, pgtype.Text{}, errors.New("result.exit_code is required for completed releases")
		}
		status := db.RunStatusSucceeded
		if *result.ExitCode != 0 {
			status = db.RunStatusFailed
		}
		return status, pgtype.Int4{Int32: *result.ExitCode, Valid: true}, pgtype.Text{}, nil
	case "failed":
		message := "worker execution failed"
		if result.Error != nil && *result.Error != "" {
			message = *result.Error
		}
		return db.RunStatusFailed, pgtype.Int4{}, pgtype.Text{String: message, Valid: true}, nil
	case "cancelled":
		message := "worker execution cancelled"
		if result.Error != nil && *result.Error != "" {
			message = *result.Error
		}
		return db.RunStatusCancelled, pgtype.Int4{}, pgtype.Text{String: message, Valid: true}, nil
	default:
		return "", pgtype.Int4{}, pgtype.Text{}, fmt.Errorf("unsupported release result kind %q", result.Kind)
	}
}

func releaseOutput(result api.WorkerReleaseResult, status db.RunStatus, exitCode pgtype.Int4) []byte {
	if status != db.RunStatusSucceeded || !exitCode.Valid || exitCode.Int32 != 0 || len(result.Output) == 0 {
		return nil
	}
	return append([]byte(nil), result.Output...)
}

type releaseWorkspaceCommitFields struct {
	leaseID            pgtype.UUID
	fencingToken       pgtype.Text
	baseVersionID      pgtype.UUID
	artifactDigest     pgtype.Text
	artifactSizeBytes  pgtype.Int8
	artifactMediaType  pgtype.Text
	artifactEncoding   pgtype.Text
	artifactEntryCount pgtype.Int4
	mountPath          pgtype.Text
}

func releaseWorkspaceFields(workspace *api.WorkerWorkspace) (releaseWorkspaceCommitFields, error) {
	if workspace == nil {
		return releaseWorkspaceCommitFields{}, nil
	}
	leaseID, err := parseRequiredWorkspaceUUID("workspace.write_lease_id", workspace.WriteLeaseID)
	if err != nil {
		return releaseWorkspaceCommitFields{}, err
	}
	fencingToken := strings.TrimSpace(workspace.WriteFencingToken)
	if fencingToken == "" {
		return releaseWorkspaceCommitFields{}, errors.New("workspace.write_fencing_token is required")
	}
	baseVersionID, err := parseOptionalWorkspaceUUID("workspace.base_version_id", workspace.BaseVersionID)
	if err != nil {
		return releaseWorkspaceCommitFields{}, err
	}
	if workspace.Artifact == nil {
		return releaseWorkspaceCommitFields{}, errors.New("workspace.artifact is required")
	}
	artifact := workspace.Artifact
	digest := strings.TrimSpace(artifact.Digest)
	if digest == "" {
		return releaseWorkspaceCommitFields{}, errors.New("workspace.artifact.digest is required")
	}
	mediaType := strings.TrimSpace(artifact.MediaType)
	if mediaType == "" {
		return releaseWorkspaceCommitFields{}, errors.New("workspace.artifact.media_type is required")
	}
	encoding := strings.TrimSpace(artifact.Encoding)
	if encoding == "" {
		return releaseWorkspaceCommitFields{}, errors.New("workspace.artifact.encoding is required")
	}
	if artifact.SizeBytes <= 0 {
		return releaseWorkspaceCommitFields{}, errors.New("workspace.artifact.size_bytes must be positive")
	}
	if artifact.EntryCount < 0 {
		return releaseWorkspaceCommitFields{}, errors.New("workspace.artifact.entry_count must be non-negative")
	}
	mountPath := strings.TrimSpace(workspace.MountPath)
	if mountPath == "" {
		return releaseWorkspaceCommitFields{}, errors.New("workspace.mount_path is required")
	}
	return releaseWorkspaceCommitFields{
		leaseID:            leaseID,
		fencingToken:       pgvalue.Text(fencingToken),
		baseVersionID:      baseVersionID,
		artifactDigest:     pgvalue.Text(digest),
		artifactSizeBytes:  pgtype.Int8{Int64: artifact.SizeBytes, Valid: true},
		artifactMediaType:  pgvalue.Text(mediaType),
		artifactEncoding:   pgvalue.Text(encoding),
		artifactEntryCount: pgtype.Int4{Int32: artifact.EntryCount, Valid: true},
		mountPath:          pgvalue.Text(mountPath),
	}, nil
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

func terminalRunEventForFields(status db.RunStatus, exitCode pgtype.Int4, errorMessage pgtype.Text, result api.WorkerReleaseResult) (string, []byte, error) {
	switch status {
	case db.RunStatusSucceeded:
		code := int32(0)
		if exitCode.Valid {
			code = exitCode.Int32
		}
		payload, err := json.Marshal(runCompletedPayload{ExitCode: code})
		return "run.completed", payload, err
	case db.RunStatusFailed:
		if exitCode.Valid {
			payload, err := json.Marshal(runFailurePayload{
				FailureKind: "task_failed",
				Detail:      taskFailedDetailPayload{ExitCode: exitCode.Int32},
			})
			return "run.failed", payload, err
		}
		message := "worker execution failed"
		if errorMessage.Valid && strings.TrimSpace(errorMessage.String) != "" {
			message = errorMessage.String
		}
		if failureKind, ok := trustedWorkerFailureKind(result); ok {
			payload, err := json.Marshal(runFailurePayload{
				FailureKind: failureKind,
				Detail: workerFailureDetailPayload{
					Message:      message,
					LimitSeconds: result.LimitSeconds,
				},
			})
			return "run.failed", payload, err
		}
		payload, err := json.Marshal(runFailurePayload{
			FailureKind: "worker_failed",
			Detail:      workerMessagePayload{Message: message},
		})
		return "run.failed", payload, err
	case db.RunStatusCancelled:
		reason := "worker execution cancelled"
		if errorMessage.Valid && strings.TrimSpace(errorMessage.String) != "" {
			reason = errorMessage.String
		}
		payload, err := json.Marshal(runCancelledPayload{Reason: reason})
		return "run.cancelled", payload, err
	default:
		return "", nil, fmt.Errorf("run status %q is not terminal", status)
	}
}

func trustedWorkerFailureKind(result api.WorkerReleaseResult) (string, bool) {
	if result.FailureKind == nil {
		return "", false
	}
	switch *result.FailureKind {
	case "max_duration", "task_not_found", "duplicate_task_id", "missing_config", "task_parse_failed":
		return *result.FailureKind, true
	default:
		return "", false
	}
}

func workerRunLeaseResponse(row db.LeaseRunLeaseRow) api.WorkerRunLease {
	return api.WorkerRunLease{
		ID:                pgvalue.MustUUIDValue(row.RunLeaseID).String(),
		OrgID:             pgvalue.MustUUIDValue(row.OrgID).String(),
		RunID:             pgvalue.MustUUIDValue(row.ID).String(),
		WorkerInstanceID:  pgvalue.MustUUIDValue(row.RunLeaseWorkerInstanceID).String(),
		ProtocolVersion:   row.RunLeaseWorkerProtocolVersion,
		AttemptNumber:     row.RunLeaseAttemptNumber,
		DispatchMessageID: row.RunLeaseDispatchMessageID,
		DispatchLeaseID:   row.RunLeaseDispatchLeaseID,
		Trace: api.TraceContext{
			TraceID:     row.RunLeaseTraceID,
			SpanID:      row.RunLeaseSpanID,
			Traceparent: row.RunLeaseTraceparent,
		},
		ExpiresAt: pgvalue.Time(row.RunLeaseExpiresAt),
	}
}

func (s *Server) workerRunFromLease(ctx context.Context, row db.LeaseRunLeaseRow) (api.WorkerRun, error) {
	restore, err := s.workerRestorePayload(ctx, row)
	if err != nil {
		return api.WorkerRun{}, err
	}
	secretNames, err := deploymentTaskSecretNames(row.DeploymentTaskSecretDeclarations)
	if err != nil {
		return api.WorkerRun{}, terminalPayload("secret_unavailable", err)
	}
	var resolvedSecrets api.ResolvedSecrets
	if len(secretNames) > 0 && restore == nil {
		if s.secrets == nil {
			return api.WorkerRun{}, errors.New("secret store is not configured")
		}
		resolvedSecrets, err = s.secrets.ResolveScopedNames(ctx, pgvalue.MustUUIDValue(row.OrgID), pgvalue.MustUUIDValue(row.ProjectID), pgvalue.MustUUIDValue(row.EnvironmentID), secretNames)
		if err != nil {
			if secret.IsUnavailable(err) || isNoRows(err) {
				return api.WorkerRun{}, terminalPayload("secret_unavailable", err)
			}
			return api.WorkerRun{}, err
		}
	}
	requirements, err := workerRunRequirementsFromLease(row)
	if err != nil {
		return api.WorkerRun{}, err
	}
	sessionID, err := requiredUUIDString(row.SessionID, "session_id")
	if err != nil {
		return api.WorkerRun{}, err
	}
	run := api.WorkerRun{
		ID:                    pgvalue.MustUUIDValue(row.ID).String(),
		Version:               row.RunDeploymentVersion,
		DeploymentVersion:     row.RunDeploymentVersion,
		APIVersion:            row.RunApiVersion,
		SDKVersion:            row.RunSdkVersion,
		CLIVersion:            row.RunCliVersion,
		WorkerProtocolVersion: row.RunLeaseWorkerProtocolVersion,
		AttemptNumber:         row.RunLeaseAttemptNumber,
		AttemptID:             pgvalue.MustUUIDValue(row.CurrentAttemptID).String(),
		RunLeaseID:            pgvalue.MustUUIDValue(row.RunLeaseID).String(),
		SnapshotVersion:       row.StateVersion,
		SessionID:             sessionID,
		TaskID:                row.TaskID,
		Payload:               json.RawMessage(row.Payload),
		Secrets:               resolvedSecrets,
		DeploymentSource: api.DeploymentSourceArtifact{
			Digest:    row.DeploymentSourceDigest,
			MediaType: api.DeploymentSourceArtifactMediaType,
		},
		DeploymentTask: api.WorkerDeploymentTask{
			ID:                  pgvalue.MustUUIDValue(row.DeploymentTaskID).String(),
			FilePath:            row.DeploymentTaskFilePath,
			ExportName:          row.DeploymentTaskExportName,
			HandlerEntrypoint:   row.DeploymentTaskHandlerEntrypoint,
			BundleDigest:        row.DeploymentTaskBundleDigest,
			BundleFormatVersion: row.DeploymentTaskBundleFormatVersion,
		},
		Workspace:          workerWorkspaceFromLease(row),
		Requirements:       requirements,
		MaxDurationSeconds: activeDurationMsToSeconds(row.MaxActiveDurationMs),
		ActiveDurationMs:   row.ActiveDurationMs,
		Trace: api.TraceContext{
			TraceID:     row.RunLeaseTraceID,
			SpanID:      row.RunLeaseSpanID,
			Traceparent: row.RunLeaseTraceparent,
		},
		Restore: restore,
	}
	return run, nil
}

func workerWorkspaceFromLease(row db.LeaseRunLeaseRow) api.WorkerWorkspace {
	if !row.WorkspaceID.Valid {
		return api.WorkerWorkspace{}
	}
	workspace := api.WorkerWorkspace{
		ID:                pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		MaterializationID: pgvalue.MustUUIDValue(row.WorkspaceMaterializationID).String(),
		WriteLeaseID:      pgvalue.MustUUIDValue(row.WorkspaceLeaseID).String(),
		WriteFencingToken: row.WorkspaceFencingToken.String,
		MountPath:         row.WorkspaceMountPath.String,
	}
	if row.WorkspaceBaseVersionID.Valid {
		workspace.BaseVersionID = pgvalue.MustUUIDValue(row.WorkspaceBaseVersionID).String()
	}
	if row.WorkspaceArtifactDigest.Valid {
		workspace.Artifact = &api.WorkerWorkspaceArtifact{
			Digest:     row.WorkspaceArtifactDigest.String,
			MediaType:  row.WorkspaceArtifactMediaType.String,
			Encoding:   row.WorkspaceArtifactEncoding.String,
			SizeBytes:  row.WorkspaceArtifactSizeBytes.Int64,
			EntryCount: row.WorkspaceArtifactEntryCount.Int32,
		}
	}
	return workspace
}

func requiredUUIDString(value pgtype.UUID, field string) (string, error) {
	if !value.Valid {
		return "", fmt.Errorf("%s is required", field)
	}
	return pgvalue.MustUUIDValue(value).String(), nil
}

func workerRunRequirementsFromLease(row db.LeaseRunLeaseRow) (compute.RunRuntimeRequirements, error) {
	return compute.RunRuntimeRequirementsFromFields(compute.RunRuntimeRequirementFields{
		RequestedMilliCPU:       row.RequestedMilliCpu,
		RequestedMemoryMiB:      row.RequestedMemoryMib,
		RequestedDiskMiB:        row.RequestedDiskMib,
		RequestedExecutionSlots: row.RequestedExecutionSlots,
		RuntimeID:               row.RequirementsRuntimeID,
		RuntimeArch:             row.RequirementsRuntimeArch,
		RuntimeABI:              row.RequirementsRuntimeAbi,
		KernelDigest:            row.RequirementsKernelDigest,
		InitramfsDigest:         row.RequirementsInitramfsDigest,
		RootfsDigest:            row.RequirementsRootfsDigest,
		CNIProfile:              row.RequirementsCniProfile,
		NetworkPolicyJSON:       row.RequirementsNetworkPolicy,
		NetworkPolicyLabel:      "worker run network policy",
		PlacementJSON:           row.RequirementsPlacement,
		PlacementLabel:          "worker run placement",
	})
}

func (s *Server) workerRestorePayload(ctx context.Context, row db.LeaseRunLeaseRow) (*api.WorkerRestore, error) {
	payload, err := s.db.GetRunRestorePayload(ctx, db.GetRunRestorePayloadParams{
		OrgID:            row.OrgID,
		RunID:            row.ID,
		RunLeaseID:       row.RunLeaseID,
		WorkerInstanceID: row.RunLeaseWorkerInstanceID,
	})
	if isNoRows(err) {
		if row.RunLeaseRestoreRuntimeCheckpointID.Valid {
			return nil, fmt.Errorf("restore runtime checkpoint %s is unavailable", pgvalue.MustUUIDValue(row.RunLeaseRestoreRuntimeCheckpointID).String())
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var manifest api.WorkerCheckpointManifest
	if err := json.Unmarshal(payload.Manifest, &manifest); err != nil {
		return nil, fmt.Errorf("decode checkpoint manifest: %w", err)
	}
	runWait, err := workerRestoreRunWait(payload)
	if err != nil {
		return nil, err
	}
	return &api.WorkerRestore{
		CheckpointID: pgvalue.MustUUIDValue(payload.RuntimeCheckpointID).String(),
		Checkpoint:   manifest,
		RunWait:      runWait,
	}, nil
}

func workerRestoreRunWait(payload db.GetRunRestorePayloadRow) (api.WorkerRestoreRunWait, error) {
	resumeKind, resumePayload, err := workerRestoreRunWaitDecision(payload)
	if err != nil {
		return api.WorkerRestoreRunWait{}, err
	}
	return api.WorkerRestoreRunWait{
		ID:                pgvalue.UUIDString(payload.RunWaitID),
		Kind:              string(payload.RunWaitKind),
		ResumeKind:        resumeKind,
		ResumePayloadJSON: resumePayload,
	}, nil
}

func workerRestoreRunWaitDecision(payload db.GetRunRestorePayloadRow) (string, json.RawMessage, error) {
	switch payload.RunWaitKind {
	case db.RunWaitKindStream:
		if payload.StreamRecordSequence.Valid {
			data := json.RawMessage(payload.StreamRecordData)
			if len(data) == 0 {
				data = json.RawMessage(`null`)
			}
			envelope, err := json.Marshal(map[string]any{
				"stream":   payload.StreamName.String,
				"sequence": payload.StreamRecordSequence.Int64,
				"data":     data,
			})
			if err == nil {
				return "completed", envelope, nil
			}
			return "", nil, fmt.Errorf("encode stream wait resume payload: %w", err)
		}
		return "timed_out", json.RawMessage(`null`), nil
	case db.RunWaitKindToken:
		if payload.TokenState.Valid {
			switch payload.TokenState.TokenState {
			case db.TokenStateCompleted:
				data := json.RawMessage(payload.TokenCompletionData)
				if len(data) == 0 {
					data = json.RawMessage(`null`)
				}
				return "completed", data, nil
			case db.TokenStateCancelled:
				return "cancelled", json.RawMessage(`null`), nil
			case db.TokenStateExpired:
				return "timed_out", json.RawMessage(`null`), nil
			}
		}
		return "timed_out", json.RawMessage(`null`), nil
	case db.RunWaitKindTimer:
		return "timed_out", json.RawMessage(`null`), nil
	default:
		return "failed", json.RawMessage(`null`), nil
	}
}
