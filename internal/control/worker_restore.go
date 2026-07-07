package control

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func (s *Server) workerRestorePayload(ctx context.Context, row db.LeaseRunLeaseRow) (*api.WorkerRestore, error) {
	payload, err := s.db.GetRunRestorePayload(ctx, db.GetRunRestorePayloadParams{
		OrgID:            row.OrgID,
		RunID:            row.ID,
		RunLeaseID:       row.RunLeaseID,
		WorkerInstanceID: row.RunLeaseWorkerInstanceID,
	})
	if isNoRows(err) {
		if row.RunLeaseRestoreRunCheckpointID.Valid {
			return nil, fmt.Errorf("restore run checkpoint %s is unavailable", pgvalue.MustUUIDValue(row.RunLeaseRestoreRunCheckpointID).String())
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
		CheckpointID: pgvalue.MustUUIDValue(payload.RunCheckpointID).String(),
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
