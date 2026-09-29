package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const workerRuntimeReconcileLimit int32 = 64

func (s *Server) workerNextRuntimeReconcileTarget(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RuntimeReconcileRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid runtime reconcile request JSON: %w", err))
		return
	}
	worker := workerFromContext(r.Context())
	rows, err := s.db.ListComputerInstanceReconcileTargets(r.Context(), db.ListComputerInstanceReconcileTargetsParams{
		WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerHostID: pgvalue.UUID(worker.WorkerHostID), WorkerEpoch: worker.WorkerEpoch,
		RowLimit: workerRuntimeReconcileLimit,
	})
	if err != nil {
		writeError(w, errors.New("list runtime reconcile targets"))
		return
	}
	items := make([]workerapi.RuntimeReconcileTarget, 0, len(rows))
	for _, row := range rows {
		action := computerInstanceReconcileAction(row)
		var capture *workerapi.RuntimeCapture
		if action == workerapi.RuntimeReconcileCapture {
			capture, err = loadComputerInstanceCapture(r.Context(), s.db, row)
			if err != nil {
				writeError(w, err)
				return
			}
		}
		source := computerInstanceSourceMetadata(row)
		if action == workerapi.RuntimeReconcilePrepare {
			source, err = projectComputerInstancePreparation(r.Context(), s.platformStore, row)
			if err != nil {
				writeError(w, err)
				return
			}
			if row.AdmissionState == "restoring" {
				if err := populateRuntimeRestoreSource(r.Context(), s.db, &source, row); err != nil {
					writeError(w, err)
					return
				}
			}
		}
		items = append(items, workerapi.RuntimeReconcileTarget{
			ID: pgvalue.UUIDString(row.ID), WorkerEpoch: row.WorkerEpoch,
			DesiredVersion: row.DesiredVersion, ObservedVersion: row.ObservedVersion,
			Action: action, Source: source, Capture: capture, PreparationExpiresAt: row.PreparationExpiresAt.Time,
		})
	}
	writeJSON(w, http.StatusOK, workerapi.RuntimeReconcileResponse{Items: items})
}

func (s *Server) workerMarkComputerInstanceReady(w http.ResponseWriter, r *http.Request) {
	s.workerMarkComputerInstance(w, r, "ready")
}
func (s *Server) workerMarkComputerInstanceClosed(w http.ResponseWriter, r *http.Request) {
	s.workerMarkComputerInstance(w, r, "closed")
}
func (s *Server) workerMarkComputerInstanceFailed(w http.ResponseWriter, r *http.Request) {
	s.workerMarkComputerInstance(w, r, "failed")
}

func (s *Server) workerMarkComputerInstance(w http.ResponseWriter, r *http.Request, state string) {
	var request workerapi.ComputerInstanceStateRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker runtime instance %s request JSON: %w", state, err))
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
	var row db.ComputerInstance
	switch state {
	case "ready":
		if request.VMVCPUCount <= 0 {
			writeError(w, badRequest(errors.New("vm_vcpu_count must be positive")))
			return
		}
		if !sha256sum.ValidDigest(request.CPUConfigDigest) {
			writeError(w, badRequest(errors.New("cpu_config_digest must be a canonical SHA-256 digest")))
			return
		}
		row, err = s.markComputerInstanceReady(r.Context(), pgvalue.UUID(worker.WorkerGroupID), db.MarkComputerInstanceReadyParams{
			DesiredVersion: request.DesiredVersion, ID: pgvalue.UUID(id), WorkerHostID: pgvalue.UUID(worker.WorkerHostID),
			WorkerEpoch:             worker.WorkerEpoch,
			ExpectedObservedVersion: request.ExpectedObservedVersion,
			VMVCPUCount:             request.VMVCPUCount, CPUConfigDigest: request.CPUConfigDigest,
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
		row, err = s.reclaimComputerInstance(r.Context(), worker.WorkerGroupID, db.ReclaimComputerInstanceParams{
			ID: pgvalue.UUID(id), WorkerHostID: pgvalue.UUID(worker.WorkerHostID), WorkerEpoch: worker.WorkerEpoch,
			DesiredVersion: request.DesiredVersion, ExpectedObservedVersion: request.ExpectedObservedVersion,
			Reason: pgvalue.Text(reason), Evidence: proof,
		})
	case "failed":
		reason := strings.TrimSpace(request.ReasonCode)
		if reason == "" {
			reason = "runtime_reconcile_failed"
		}
		if reason == workerapi.RuntimeFailureWorkerInvalid {
			if err = s.fenceInvalidWorkerEpoch(r.Context(), worker.WorkerGroupID, pgvalue.UUID(worker.WorkerHostID), worker.WorkerEpoch); err != nil {
				writeError(w, err)
				return
			}
		}
		if request.CleanupProof != nil {
			if proofErr := validateRuntimeCleanupProof(*request.CleanupProof, time.Now()); proofErr != nil {
				writeError(w, badRequest(proofErr))
				return
			}
			proof, proofErr := json.Marshal(request.CleanupProof)
			if proofErr != nil {
				writeError(w, badRequest(proofErr))
				return
			}
			row, err = s.reclaimComputerInstance(r.Context(), worker.WorkerGroupID, db.ReclaimComputerInstanceParams{RequireFailure: true,
				ID: pgvalue.UUID(id), WorkerHostID: pgvalue.UUID(worker.WorkerHostID), WorkerEpoch: worker.WorkerEpoch,
				DesiredVersion: request.DesiredVersion, ExpectedObservedVersion: request.ExpectedObservedVersion, Reason: pgvalue.Text(reason), Evidence: proof,
			})
			if err == nil {
				writeJSON(w, http.StatusOK, computerInstanceResponse(row))
				return
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				writeError(w, errors.New("reclaim failed Computer Instance"))
				return
			}
		}
		row, err = s.markComputerInstanceFailed(r.Context(), worker.WorkerGroupID, db.MarkComputerInstanceFailedParams{
			ReasonCode: pgvalue.Text(reason), Error: normalizedJSONRawMessage(request.Error),
			ID: pgvalue.UUID(id), WorkerHostID: pgvalue.UUID(worker.WorkerHostID), WorkerEpoch: worker.WorkerEpoch,
			DesiredVersion:          request.DesiredVersion,
			ExpectedObservedVersion: request.ExpectedObservedVersion,
		})
		if err == nil && request.CleanupProof != nil {
			proof, _ := json.Marshal(request.CleanupProof)
			row, err = s.reclaimComputerInstance(r.Context(), worker.WorkerGroupID, db.ReclaimComputerInstanceParams{RequireFailure: true,
				ID: row.ID, WorkerHostID: row.WorkerHostID, WorkerEpoch: row.WorkerEpoch, DesiredVersion: row.DesiredVersion,
				ExpectedObservedVersion: row.ObservedVersion, Reason: row.TerminalReasonCode, Evidence: proof,
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
	writeJSON(w, http.StatusOK, computerInstanceResponse(row))
}

func (s *Server) markComputerInstanceFailed(ctx context.Context, workerGroupID uuid.UUID, params db.MarkComputerInstanceFailedParams) (db.ComputerInstance, error) {
	var row db.ComputerInstance
	err := s.inTx(ctx, func(work *txWork) error {
		tx := work.tx
		var err error
		row, err = dispatch.RecordComputerInstanceFailure(ctx, tx, workerGroupID, params)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) && params.ReasonCode.String == workerapi.RuntimeFailureWorkerInvalid {
		if fenceErr := s.fenceInvalidWorkerEpoch(ctx, workerGroupID, params.WorkerHostID, params.WorkerEpoch); fenceErr != nil {
			return row, fenceErr
		}
	}
	return row, err
}

func (s *Server) reclaimComputerInstance(ctx context.Context, groupID uuid.UUID, params db.ReclaimComputerInstanceParams) (db.ComputerInstance, error) {
	var row db.ComputerInstance
	err := s.inTx(ctx, func(work *txWork) error {
		tx := work.tx
		var err error
		row, err = dispatch.RecordComputerInstanceReclaim(ctx, tx, groupID, params)
		return err
	})
	return row, err
}

func (s *Server) fenceInvalidWorkerEpoch(
	ctx context.Context,
	workerGroupID uuid.UUID,
	workerHostID pgtype.UUID,
	workerEpoch int64,
) error {
	if s.tx == nil {
		return errors.New("invalid Worker epoch transaction authority is unavailable")
	}
	return s.inTx(ctx, func(work *txWork) error {
		tx := work.tx
		queries := db.New(tx)
		poolID, err := queries.GetWorkerHostPoolID(ctx, db.GetWorkerHostPoolIDParams{
			WorkerHostID:  workerHostID,
			WorkerGroupID: pgvalue.UUID(workerGroupID),
			WorkerEpoch:   pgtype.Int8{Int64: workerEpoch, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("resolve invalid Worker epoch Pool: %w", err)
		}
		group, err := queries.LockWorkerGroupForPoolMutation(ctx, pgvalue.UUID(workerGroupID))
		if err != nil {
			return fmt.Errorf("lock invalid Worker epoch Group: %w", err)
		}
		if group.Status != db.WorkerGroupStatusActive && group.Status != db.WorkerGroupStatusPaused &&
			group.Status != db.WorkerGroupStatusDraining {
			return errors.New("invalid Worker epoch Group is inactive")
		}
		pool, err := queries.LockWorkerPool(ctx, db.LockWorkerPoolParams{
			WorkerGroupID: pgvalue.UUID(workerGroupID),
			WorkerPoolID:  poolID,
		})
		if err != nil {
			return fmt.Errorf("lock invalid Worker epoch Pool: %w", err)
		}
		if pool.Status != "active" && pool.Status != "draining" {
			return errors.New("invalid Worker epoch Pool is inactive")
		}
		worker, err := queries.LockWorkerHostForActivation(ctx, db.LockWorkerHostForActivationParams{
			WorkerHostID:  workerHostID,
			WorkerGroupID: pgvalue.UUID(workerGroupID),
			WorkerPoolID:  poolID,
			WorkerEpoch:   pgtype.Int8{Int64: workerEpoch, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("lock invalid Worker epoch: %w", err)
		}
		if worker.Status != db.WorkerHostStatusActive && worker.Status != db.WorkerHostStatusDraining {
			return errors.New("invalid Worker epoch is inactive")
		}
		if worker.Status == db.WorkerHostStatusActive {
			if _, err := queries.DrainWorkerHost(ctx, db.DrainWorkerHostParams{
				ID:                   workerHostID,
				WorkerGroupID:        pgvalue.UUID(workerGroupID),
				ExpectedEpoch:        pgtype.Int8{Int64: workerEpoch, Valid: true},
				ExpectedClaimVersion: worker.ClaimVersion,
			}); err != nil {
				return fmt.Errorf("fence invalid Worker epoch: %w", err)
			}
		}
		return nil
	})
}

func validateRuntimeCleanupProof(proof workerapi.RuntimeCleanupProof, now time.Time) error {
	switch proof.Method {
	case workerapi.RuntimeCleanupSessionClosed, workerapi.RuntimeCleanupHostReconciled, workerapi.RuntimeCleanupNotMaterialized:
	default:
		return errors.New("runtime cleanup proof method is unsupported")
	}
	if proof.CompletedAt.IsZero() || proof.CompletedAt.After(now.Add(time.Minute)) {
		return errors.New("runtime cleanup proof completed_at is required and cannot be in the future")
	}
	return nil
}

func validateRuntimeClosedCleanupProof(proof workerapi.RuntimeCleanupProof, now time.Time) error {
	if proof.Method != workerapi.RuntimeCleanupSessionClosed && proof.Method != workerapi.RuntimeCleanupHostReconciled {
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

func computerInstanceResponse(row db.ComputerInstance) workerapi.ComputerInstance {
	return workerapi.ComputerInstance{
		ID:                     pgvalue.UUIDString(row.ID),
		OrgID:                  pgvalue.UUIDString(row.OrgID),
		ProjectID:              pgvalue.UUIDString(row.ProjectID),
		EnvironmentID:          pgvalue.UUIDString(row.EnvironmentID),
		WorkerHostID:           pgvalue.UUIDString(row.WorkerHostID),
		RuntimeEpoch:           row.WorkerEpoch,
		RuntimeID:              row.VMPlatformID,
		VMVCPUCount:            row.VMVCPUCount,
		CPUConfigDigest:        row.CPUConfigDigest,
		ComputerSpecID:         pgvalue.UUIDString(row.ComputerSpecID),
		Status:                 string(row.ObservedState),
		ReservedCPUMillis:      int32(row.ReservedCPUMillis),
		ReservedMemoryMiB:      int32(row.ReservedMemoryBytes / 1048576),
		ReservedDiskMiB:        row.ReservedGuestEphemeralDiskBytes / 1048576,
		ReservedExecutionSlots: row.ReservedExecutionSlots,
	}
}
