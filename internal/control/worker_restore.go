package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) tryLeaseCheckpointRestoreRun(ctx context.Context, worker workerActor) (dispatch.ClaimedRun, db.LeaseRunLeaseRow, bool, error) {
	expiresAt := time.Now().Add(workerLeaseDuration)
	entry, err := s.db.ReserveCheckpointRestoreRunForWorker(ctx, pgvalue.UUID(worker.WorkerInstanceID))
	if isNoRows(err) {
		return dispatch.ClaimedRun{}, db.LeaseRunLeaseRow{}, false, nil
	}
	if err != nil {
		return dispatch.ClaimedRun{}, db.LeaseRunLeaseRow{}, false, err
	}
	messageID := strings.TrimSpace(fmt.Sprint(entry.DispatchMessageID))
	dispatchLeaseID := "restore-source-" + uuid.Must(uuid.NewV7()).String()
	lease := dispatch.Lease{
		ID:               dispatchLeaseID,
		MessageID:        messageID,
		WorkerInstanceID: worker.WorkerInstanceID.String(),
		AttemptNumber:    1,
		ExpiresAt:        expiresAt,
		Message: dispatch.Message{
			RunID:              pgvalue.UUIDString(entry.RunID),
			OrgID:              pgvalue.UUIDString(entry.OrgID),
			WorkerGroupID:      entry.WorkerGroupID,
			QueueClass:         entry.QueueClass,
			QueueName:          entry.QueueName,
			DispatchGeneration: entry.DispatchGeneration,
			Priority:           entry.Priority,
			QueueTimestamp:     pgvalue.Time(entry.QueueTimestamp),
			QueuedExpiresAt:    pgvalue.Time(entry.QueuedExpiresAt),
		},
	}
	sessionSpanID, err := tracing.NewSpanID()
	if err != nil {
		if requeueErr := s.requeueCheckpointRestoreRunDispatch(ctx, worker, entry, messageID, "checkpoint restore trace span failed"); requeueErr != nil {
			err = errors.Join(err, requeueErr)
		}
		return dispatch.ClaimedRun{}, db.LeaseRunLeaseRow{}, false, err
	}
	leasedRun, err := s.db.LeaseRunLease(ctx, db.LeaseRunLeaseParams{
		OrgID:              entry.OrgID,
		RunID:              entry.RunID,
		DispatchGeneration: entry.DispatchGeneration,
		WorkerInstanceID:   pgvalue.UUID(worker.WorkerInstanceID),
		RunLeaseID:         pgvalue.UUID(uuid.Must(uuid.NewV7())),
		DispatchMessageID:  messageID,
		DispatchLeaseID:    dispatchLeaseID,
		DispatchAttempt:    1,
		LeaseExpiresAt:     pgtype.Timestamptz{Time: expiresAt, Valid: true},
		RunLeaseSpanID:     sessionSpanID,
	})
	if isNoRows(err) {
		s.logRunWorkspaceReuseDiagnostics(ctx, entry.OrgID, entry.RunID, pgvalue.UUID(worker.WorkerInstanceID), "checkpoint_restore_source_lease_no_rows")
		if requeueErr := s.requeueCheckpointRestoreRunDispatch(ctx, worker, entry, messageID, "checkpoint restore source lease conflict"); requeueErr != nil {
			return dispatch.ClaimedRun{}, db.LeaseRunLeaseRow{}, false, requeueErr
		}
		return dispatch.ClaimedRun{}, db.LeaseRunLeaseRow{}, false, nil
	}
	if err != nil {
		if requeueErr := s.requeueCheckpointRestoreRunDispatch(ctx, worker, entry, messageID, err.Error()); requeueErr != nil {
			err = errors.Join(err, requeueErr)
		}
		return dispatch.ClaimedRun{}, db.LeaseRunLeaseRow{}, false, err
	}
	if s.log != nil {
		s.log.Info("worker checkpoint restore source run lease acquired",
			"worker_instance_id", worker.WorkerInstanceID.String(),
			"run_id", pgvalue.UUIDString(leasedRun.ID),
			"workspace_id", pgvalue.UUIDString(leasedRun.WorkspaceID),
			"workspace_mount_id", pgvalue.UUIDString(leasedRun.WorkspaceMountID),
			"restore_runtime_checkpoint_id", pgvalue.UUIDString(leasedRun.RunLeaseRestoreRuntimeCheckpointID),
		)
	}
	return dispatch.ClaimedRun{Lease: lease, Entry: checkpointRestoreRun(entry)}, leasedRun, true, nil
}

func (s *Server) requeueCheckpointRestoreRunDispatch(ctx context.Context, worker workerActor, entry db.ReserveCheckpointRestoreRunForWorkerRow, messageID string, lastError string) error {
	return s.requeueRunDispatch(ctx, entry.OrgID, entry.WorkerGroupID, entry.QueueClass, entry.RunID, lastError)
}

func checkpointRestoreRun(row db.ReserveCheckpointRestoreRunForWorkerRow) db.Run {
	return db.Run{
		OrgID:              row.OrgID,
		WorkerGroupID:      row.WorkerGroupID,
		ID:                 row.RunID,
		QueueClass:         row.QueueClass,
		QueueName:          row.QueueName,
		Priority:           row.Priority,
		QueueTimestamp:     row.QueueTimestamp,
		QueuedExpiresAt:    row.QueuedExpiresAt,
		DispatchGeneration: row.DispatchGeneration,
		Status:             db.RunStatusQueued,
	}
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
	case db.WaitKindStream:
		if len(payload.WaitResult) > 0 {
			return "completed", json.RawMessage(payload.WaitResult), nil
		}
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
	case db.WaitKindToken:
		if len(payload.WaitResult) > 0 {
			return "completed", json.RawMessage(payload.WaitResult), nil
		}
		if payload.WaitState == db.WaitStateCancelled {
			return "cancelled", json.RawMessage(`null`), nil
		}
		if payload.WaitState == db.WaitStateExpired {
			return "timed_out", json.RawMessage(`null`), nil
		}
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
	case db.WaitKindTimer:
		return "completed", json.RawMessage(`null`), nil
	default:
		return "failed", json.RawMessage(`null`), nil
	}
}
