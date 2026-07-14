package control

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func (s *Server) workerRestorePayload(ctx context.Context, row db.ClaimAssignedRunLeaseRow) (*api.WorkerRestore, error) {
	if !row.RunLeaseRestoreRunCheckpointID.Valid {
		return nil, nil
	}
	if !row.RunLeaseRestoreResumeRequestVersion.Valid || row.RunLeaseRestoreResumeRequestVersion.Int64 <= 0 {
		return nil, fmt.Errorf("restore run checkpoint %s has no resume request", pgvalue.MustUUIDValue(row.RunLeaseRestoreRunCheckpointID).String())
	}
	payload, err := s.db.GetClaimedRunRestorePayload(ctx, db.GetClaimedRunRestorePayloadParams{
		OrgID:                row.OrgID,
		RunID:                row.ID,
		RunLeaseID:           row.RunLeaseID,
		WorkerInstanceID:     row.RunLeaseWorkerInstanceID,
		WorkerEpoch:          row.RunLeaseWorkerEpoch,
		RunCheckpointID:      row.RunLeaseRestoreRunCheckpointID,
		ResumeRequestVersion: row.RunLeaseRestoreResumeRequestVersion.Int64,
	})
	if isNoRows(err) {
		return nil, fmt.Errorf("restore run checkpoint %s is unavailable", pgvalue.MustUUIDValue(row.RunLeaseRestoreRunCheckpointID).String())
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

func workerRestoreRunWait(payload db.GetClaimedRunRestorePayloadRow) (api.WorkerRestoreRunWait, error) {
	resumeKind, resumePayload, err := workerRestoreRunWaitDecision(payload)
	if err != nil {
		return api.WorkerRestoreRunWait{}, err
	}
	return api.WorkerRestoreRunWait{
		ID:                   pgvalue.UUIDString(payload.RunWaitID),
		ResumeRequestVersion: payload.ResumeRequestVersion,
		Kind:                 string(payload.RunWaitKind),
		ResumeKind:           resumeKind,
		ResumePayloadJSON:    resumePayload,
	}, nil
}

func workerRestoreRunWaitDecision(payload db.GetClaimedRunRestorePayloadRow) (string, json.RawMessage, error) {
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
