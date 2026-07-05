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
		if isNoRows(err) {
			writeError(w, forbidden(errors.New("worker instance conflicts with this cell or runtime")))
			return
		}
		writeError(w, errors.New("record worker heartbeat"))
		return
	}
	if err := s.db.EnsureRuntimeReleaseSelection(r.Context(), capabilities.RuntimeID); err != nil {
		s.log.Error("ensure runtime release selection failed", "worker_instance_id", worker.WorkerInstanceID.String(), "runtime_id", capabilities.RuntimeID, "error", err)
		writeError(w, errors.New("select runtime release"))
		return
	}
	if err := s.markStaleWorkspaceMountsLost(r.Context()); err != nil {
		s.log.Error("mark stale workspace mounts lost failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("reap stale workspace mounts"))
		return
	}
	queueLease, leasedRun, foundLease, err := s.tryLeaseResidentRun(r.Context(), worker)
	if err != nil {
		s.log.Error("worker resident run lease failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("lease resident run"))
		return
	}
	if !foundLease {
		queueLease, leasedRun, foundLease, err = s.tryLeaseCheckpointRestoreRun(r.Context(), worker)
		if err != nil {
			s.log.Error("worker checkpoint restore run lease failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
			writeError(w, errors.New("lease checkpoint restore run"))
			return
		}
	}
	if !foundLease {
		capacity, err := s.db.GetWorkerInstanceRunDispatchCapacity(r.Context(), db.GetWorkerInstanceRunDispatchCapacityParams{
			ID:     pgvalue.UUID(worker.WorkerInstanceID),
			CellID: worker.CellID,
		})
		if isNoRows(err) {
			s.requestCapacityPressureIdleWorkspaceStops(r.Context(), worker.WorkerInstanceID, "run_dispatch_capacity_missing")
			s.createCapacityPressureLiveRuntimeCheckpointWaitCommands(r.Context(), worker.WorkerInstanceID, "run_dispatch_capacity_missing")
			writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{})
			return
		}
		if err != nil {
			s.log.Error("worker capacity lookup failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
			writeError(w, errors.New("get worker capacity"))
			return
		}
		if capacity.AvailableExecutionSlots <= 0 || capacity.AvailableMilliCpu <= 0 || capacity.AvailableMemoryMib <= 0 {
			s.requestCapacityPressureIdleWorkspaceStops(r.Context(), worker.WorkerInstanceID, "run_dispatch_no_available_capacity")
			s.createCapacityPressureLiveRuntimeCheckpointWaitCommands(r.Context(), worker.WorkerInstanceID, "run_dispatch_no_available_capacity")
			if s.log != nil {
				s.log.Info("worker run lease capacity constrained",
					"worker_instance_id", worker.WorkerInstanceID.String(),
					"reason", "run_dispatch_no_available_capacity",
					"available_execution_slots", capacity.AvailableExecutionSlots,
					"available_milli_cpu", capacity.AvailableMilliCpu,
					"available_memory_mib", capacity.AvailableMemoryMib,
					"available_disk_mib", capacity.AvailableDiskMib,
				)
			}
			writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{})
			return
		}
		runClaimer, err := dispatch.NewClaimer(s.db, s.dispatchQueue)
		if err != nil {
			writeError(w, unavailable(errors.New("run queue item queue is not configured")))
			return
		}
		dequeueRequest := dispatch.DequeueRequest{
			CellID:           worker.CellID,
			WorkerInstanceID: worker.WorkerInstanceID.String(),
			Region:           capabilities.Region,
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
			Labels:      capabilities.Labels,
			MaxMessages: 1,
		}
		const scopePageSize int32 = 100
		scanSeed := int32(s.workerLeaseScanSeed.Add(1) & 0x7fffffff)
		scopeSelector := dispatch.RoundRobinQueueScopeSelector{}
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
					CellID:        row.CellID,
					ProjectID:     row.ProjectID,
					EnvironmentID: row.EnvironmentID,
					QueueClass:    row.QueueClass,
					QueueName:     row.QueueName,
				})
			}

			scopes = scopeSelector.Order(scopes)
			for _, scope := range scopes {
				orgID := pgvalue.MustUUIDValue(scope.OrgID)
				if err := dispatch.SweepExpiredForOrg(r.Context(), s.db, scope.CellID, scope.OrgID); err != nil {
					s.log.Warn("sweep expired run leases failed", "org_id", orgID.String(), "error", err)
				}
				dequeueRequest.OrgID = orgID.String()
				dequeueRequest.CellID = scope.CellID
				dequeueRequest.ProjectID = pgvalue.MustUUIDValue(scope.ProjectID).String()
				dequeueRequest.EnvironmentID = pgvalue.MustUUIDValue(scope.EnvironmentID).String()
				dequeueRequest.QueueClass = scope.QueueClass
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
						s.log.Info("worker run lease acquired",
							"worker_instance_id", worker.WorkerInstanceID.String(),
							"run_id", pgvalue.UUIDString(candidateRun.ID),
							"workspace_id", pgvalue.UUIDString(candidateRun.WorkspaceID),
							"workspace_mount_id", pgvalue.UUIDString(candidateRun.WorkspaceMountID),
							"restore_runtime_checkpoint_id", pgvalue.UUIDString(candidateRun.RunLeaseRestoreRuntimeCheckpointID),
						)
						queueLease = candidateLease
						leasedRun = candidateRun
						foundLease = true
						break
					}
					if isNoRows(err) {
						s.logRunWorkspaceReuseDiagnostics(r.Context(), candidateLease.Entry.OrgID, candidateLease.Entry.RunID, pgvalue.UUID(worker.WorkerInstanceID), "lease_no_rows")
						if ensureErr := s.ensureQueuedRunWorkspaceMountForLeaseConflict(r.Context(), candidateLease.Entry.OrgID, candidateLease.Entry.RunID); ensureErr != nil {
							s.log.Warn("ensure queued run workspace mount after lease conflict failed", "worker_instance_id", worker.WorkerInstanceID.String(), "run_id", pgvalue.UUIDString(candidateLease.Entry.RunID), "error", ensureErr)
						}
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
	}
	if !foundLease {
		writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{})
		return
	}

	lease := workerRunLeaseResponse(leasedRun)
	run, err := s.workerRunFromLease(r.Context(), leasedRun)
	if err != nil {
		if failure, ok := terminalPayloadFailure(err); ok {
			if failErr := s.failLeasedRunPayload(r.Context(), worker, leasedRun, queueLease.Lease, failure); failErr != nil {
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

func (s *Server) tryLeaseResidentRun(ctx context.Context, worker workerActor) (dispatch.ClaimedRun, db.LeaseRunLeaseRow, bool, error) {
	expiresAt := time.Now().Add(workerLeaseDuration)
	entry, err := s.db.ReserveResidentRunQueueItemForWorker(ctx, db.ReserveResidentRunQueueItemForWorkerParams{
		WorkerInstanceID:     pgvalue.UUID(worker.WorkerInstanceID),
		ReservationExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if isNoRows(err) {
		return dispatch.ClaimedRun{}, db.LeaseRunLeaseRow{}, false, nil
	}
	if err != nil {
		return dispatch.ClaimedRun{}, db.LeaseRunLeaseRow{}, false, err
	}
	messageID := strings.TrimSpace(entry.DispatchMessageID.String)
	dispatchLeaseID := "resident-" + uuid.Must(uuid.NewV7()).String()
	lease := dispatch.Lease{
		ID:               dispatchLeaseID,
		MessageID:        messageID,
		WorkerInstanceID: worker.WorkerInstanceID.String(),
		AttemptNumber:    1,
		ExpiresAt:        expiresAt,
		Message: dispatch.Message{
			RunID:           pgvalue.UUIDString(entry.RunID),
			OrgID:           pgvalue.UUIDString(entry.OrgID),
			CellID:          entry.CellID,
			RouteGeneration: entry.RouteGeneration,
			QueueClass:      entry.QueueClass,
			QueueName:       entry.QueueName,
			Priority:        entry.Priority,
			QueueTimestamp:  pgvalue.Time(entry.QueueTimestamp),
			QueuedExpiresAt: pgvalue.Time(entry.QueuedExpiresAt),
		},
	}
	sessionSpanID, err := tracing.NewSpanID()
	if err != nil {
		if requeueErr := s.requeueResidentRunQueueItem(ctx, worker, entry, messageID, "resident trace span failed"); requeueErr != nil {
			err = errors.Join(err, requeueErr)
		}
		return dispatch.ClaimedRun{}, db.LeaseRunLeaseRow{}, false, err
	}
	leasedRun, err := s.db.LeaseRunLease(ctx, db.LeaseRunLeaseParams{
		OrgID:             entry.OrgID,
		RunID:             entry.RunID,
		WorkerInstanceID:  pgvalue.UUID(worker.WorkerInstanceID),
		RunLeaseID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		DispatchMessageID: pgtype.Text{String: messageID, Valid: true},
		DispatchLeaseID:   dispatchLeaseID,
		DispatchAttempt:   1,
		LeaseExpiresAt:    pgtype.Timestamptz{Time: expiresAt, Valid: true},
		RunLeaseSpanID:    sessionSpanID,
	})
	if isNoRows(err) {
		s.logRunWorkspaceReuseDiagnostics(ctx, entry.OrgID, entry.RunID, pgvalue.UUID(worker.WorkerInstanceID), "resident_lease_no_rows")
		if requeueErr := s.requeueResidentRunQueueItem(ctx, worker, entry, messageID, "resident execution lease conflict"); requeueErr != nil {
			return dispatch.ClaimedRun{}, db.LeaseRunLeaseRow{}, false, requeueErr
		}
		return dispatch.ClaimedRun{}, db.LeaseRunLeaseRow{}, false, nil
	}
	if err != nil {
		if requeueErr := s.requeueResidentRunQueueItem(ctx, worker, entry, messageID, err.Error()); requeueErr != nil {
			err = errors.Join(err, requeueErr)
		}
		return dispatch.ClaimedRun{}, db.LeaseRunLeaseRow{}, false, err
	}
	if s.log != nil {
		s.log.Info("worker resident run lease acquired",
			"worker_instance_id", worker.WorkerInstanceID.String(),
			"run_id", pgvalue.UUIDString(leasedRun.ID),
			"workspace_id", pgvalue.UUIDString(leasedRun.WorkspaceID),
			"workspace_mount_id", pgvalue.UUIDString(leasedRun.WorkspaceMountID),
		)
	}
	return dispatch.ClaimedRun{Lease: lease, Entry: residentRunQueueItem(entry)}, leasedRun, true, nil
}

func (s *Server) requeueResidentRunQueueItem(ctx context.Context, worker workerActor, entry db.ReserveResidentRunQueueItemForWorkerRow, messageID string, lastError string) error {
	return s.requeueRunQueueItem(ctx, worker, residentRunQueueItem(entry), messageID, lastError)
}

func (s *Server) requeueRunQueueItem(ctx context.Context, worker workerActor, entry db.RunQueueItem, messageID string, lastError string) error {
	_, err := s.db.RequeueRunQueueItem(ctx, db.RequeueRunQueueItemParams{
		OrgID:             entry.OrgID,
		CellID:            entry.CellID,
		RouteGeneration:   entry.RouteGeneration,
		QueueClass:        entry.QueueClass,
		RunID:             entry.RunID,
		WorkerInstanceID:  pgvalue.UUID(worker.WorkerInstanceID),
		DispatchMessageID: pgtype.Text{String: strings.TrimSpace(messageID), Valid: true},
		LastError:         strings.TrimSpace(lastError),
	})
	return err
}

func residentRunQueueItem(row db.ReserveResidentRunQueueItemForWorkerRow) db.RunQueueItem {
	return db.RunQueueItem(row)
}

func (s *Server) ensureQueuedRunWorkspaceMountForLeaseConflict(ctx context.Context, orgID pgtype.UUID, runID pgtype.UUID) error {
	return s.inTx(ctx, func(work *txWork) error {
		_, err := ensureQueuedRunWorkspaceMount(ctx, work.q, orgID, runID, "run_lease_conflict", s.log)
		return err
	})
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
		CellID:            lease.Message.CellID,
		RouteGeneration:   lease.Message.RouteGeneration,
		QueueClass:        lease.Message.QueueClass,
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
		CellID:           worker.CellID,
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
			OrgID:           leaseIDs.orgID.String(),
			RunID:           pgvalue.MustUUIDValue(row.RunID).String(),
			CellID:          row.CellID,
			RouteGeneration: row.RouteGeneration,
			QueueClass:      row.QueueClass,
			QueueName:       row.QueueName,
		},
	}
	return row, lease, nil
}

func (s *Server) workerCurrentRunningLease(ctx context.Context, worker workerActor, leaseIDs workerRunLeaseIDs) (db.GetCurrentRunningRunLeaseRow, error) {
	row, err := s.db.GetCurrentRunningRunLease(ctx, db.GetCurrentRunningRunLeaseParams{
		OrgID:            pgvalue.UUID(leaseIDs.orgID),
		CellID:           worker.CellID,
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
	if err := s.requireRoutableRecordCellGeneration(ctx, s.db, pgvalue.MustUUIDValue(row.OrgID), row.ProjectID, row.EnvironmentID, row.CellID, row.RunLeaseRouteGeneration); err != nil {
		return api.WorkerRun{}, err
	}
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
		WorkspaceMountID:  pgvalue.MustUUIDValue(row.WorkspaceMountID).String(),
		FencingGeneration: row.WorkspaceMountFencingGeneration,
		WriteLeaseID:      pgvalue.MustUUIDValue(row.WorkspaceLeaseID).String(),
		WriteFencingToken: row.WorkspaceFencingToken,
		MountPath:         row.WorkspaceMountPath.String,
	}
	if row.WorkspaceSandboxImageArtifactDigest.Valid {
		workspace.SubstrateSource = &api.WorkerRuntimeSubstrateSource{
			DeploymentSandboxID: pgvalue.MustUUIDValue(row.WorkspaceDeploymentSandboxID).String(),
			SandboxImageArtifact: api.CASObject{
				Digest:    row.WorkspaceSandboxImageArtifactDigest.String,
				SizeBytes: row.WorkspaceSandboxImageArtifactSizeBytes.Int64,
				MediaType: row.WorkspaceSandboxImageArtifactMediaType.String,
			},
			SandboxImageArtifactFormat: row.WorkspaceSandboxImageArtifactFormat.String,
			RootfsDigest:               row.WorkspaceSandboxRootfsDigest.String,
			ImageDigest:                row.WorkspaceSandboxImageDigest.String,
			ImageFormat:                row.WorkspaceSandboxImageFormat.String,
			WorkspaceMountPath:         row.WorkspaceMountPath.String,
			RuntimeABI:                 row.WorkspaceRuntimeAbi.String,
			GuestdABI:                  row.WorkspaceGuestdAbi.String,
			AdapterABI:                 row.WorkspaceAdapterAbi.String,
		}
		if row.WorkspaceRuntimeSubstrateArtifactID.Valid {
			workspace.SubstrateSource.RuntimeSubstrateArtifact = &api.WorkerRuntimeSubstrateArtifact{
				ID:                  pgvalue.MustUUIDValue(row.WorkspaceRuntimeSubstrateArtifactID).String(),
				DeploymentSandboxID: pgvalue.MustUUIDValue(row.WorkspaceDeploymentSandboxID).String(),
				Artifact: api.CASObject{
					Digest:    row.WorkspaceRuntimeSubstrateArtifactDigest,
					SizeBytes: row.WorkspaceRuntimeSubstrateArtifactSizeBytes,
					MediaType: row.WorkspaceRuntimeSubstrateArtifactMediaType,
				},
				SubstrateDigest: row.WorkspaceRuntimeSubstrateDigest,
				Format:          row.WorkspaceRuntimeSubstrateFormat,
				BuilderABI:      row.WorkspaceRuntimeSubstrateBuilderAbi,
				LayoutABI:       row.WorkspaceRuntimeSubstrateLayoutAbi,
				SizeBytes:       row.WorkspaceRuntimeSubstrateSizeBytes,
			}
		}
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
