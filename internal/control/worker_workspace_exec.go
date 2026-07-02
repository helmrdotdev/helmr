package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerMarkWorkspaceExecStarted(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceExecStartedRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace exec started request JSON: %w", err)))
		return
	}
	mount, err := s.validateWorkerWorkspacePrimitiveScope(r.Context(), workerFromContext(r.Context()), request.WorkerWorkspacePrimitiveScope)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "validate workspace exec event scope", err)
		return
	}
	execID, err := parseWorkerPrimitiveUUID("exec_id", request.ExecID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	row, err := s.db.MarkWorkspaceExecStarted(r.Context(), db.MarkWorkspaceExecStartedParams{
		ProcessID:        strings.TrimSpace(request.ProcessID),
		OrgID:            mount.OrgID,
		ProjectID:        mount.ProjectID,
		EnvironmentID:    mount.EnvironmentID,
		WorkspaceID:      mount.WorkspaceID,
		ID:               execID,
		WorkspaceMountID: mount.ID,
	})
	if err != nil {
		if isNoRows(err) {
			existing, getErr := s.db.GetWorkspaceExec(r.Context(), db.GetWorkspaceExecParams{
				OrgID:         mount.OrgID,
				ProjectID:     mount.ProjectID,
				EnvironmentID: mount.EnvironmentID,
				WorkspaceID:   mount.WorkspaceID,
				ID:            execID,
			})
			if getErr == nil && workerPrimitiveWorkspaceMountMatches(existing.WorkspaceMountID, mount.ID) && execStateTerminal(existing.State) {
				writeJSON(w, http.StatusOK, api.WorkspaceExecEnvelope{Exec: workspaceExecResponse(existing)})
				return
			}
		}
		s.writeWorkspacePrimitiveError(w, "mark workspace exec started", err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspaceExecEnvelope{Exec: workspaceExecResponse(db.WorkspaceExec(row))})
}

func (s *Server) workerAppendWorkspaceExecOutput(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceExecOutputRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace exec output request JSON: %w", err)))
		return
	}
	mount, err := s.validateWorkerWorkspacePrimitiveScope(r.Context(), workerFromContext(r.Context()), request.WorkerWorkspacePrimitiveScope)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "validate workspace exec output scope", err)
		return
	}
	execID, err := parseWorkerPrimitiveUUID("exec_id", request.ExecID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	exec, err := s.db.GetWorkspaceExec(r.Context(), db.GetWorkspaceExecParams{
		OrgID:         mount.OrgID,
		ProjectID:     mount.ProjectID,
		EnvironmentID: mount.EnvironmentID,
		WorkspaceID:   mount.WorkspaceID,
		ID:            execID,
	})
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "get workspace exec", err)
		return
	}
	if !exec.WorkspaceMountID.Valid || pgvalue.MustUUIDValue(exec.WorkspaceMountID) != pgvalue.MustUUIDValue(mount.ID) {
		writeError(w, conflict(errors.New("workspace exec is not bound to this workspace mount")))
		return
	}
	out := make([]api.WorkspaceExecStreamChunkResponse, 0, len(request.Chunks))
	for _, input := range request.Chunks {
		stream := db.WorkspaceExecStream(strings.TrimSpace(input.Stream))
		if stream != db.WorkspaceExecStreamStdout && stream != db.WorkspaceExecStreamStderr {
			writeError(w, badRequest(errors.New("exec output stream must be stdout or stderr")))
			return
		}
		chunk, err := s.appendWorkspaceExecOutputStreamChunk(r.Context(), exec, stream, input.OffsetStart, input.Data)
		if err != nil {
			s.writeWorkspacePrimitiveError(w, "append workspace exec output", err)
			return
		}
		out = append(out, workspaceExecStreamChunkResponse(chunk))
	}
	writeJSON(w, http.StatusOK, api.ListWorkspaceExecStreamChunksResponse{Chunks: out})
}

func (s *Server) workerListWorkspaceExecInput(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceExecInputRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace exec input request JSON: %w", err)))
		return
	}
	mount, exec, ok := s.loadWorkerWorkspaceExecBoundToWorkspaceMount(w, r, request.WorkerWorkspacePrimitiveScope, request.ExecID)
	if !ok {
		_ = mount
		return
	}
	limit := request.Limit
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.db.ListWorkspaceExecStdinChunksAfterDelivered(r.Context(), db.ListWorkspaceExecStdinChunksAfterDeliveredParams{
		OrgID:         mount.OrgID,
		ProjectID:     mount.ProjectID,
		EnvironmentID: mount.EnvironmentID,
		WorkspaceID:   mount.WorkspaceID,
		ExecID:        exec.ID,
		LimitCount:    limit,
	})
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "list workspace exec input", err)
		return
	}
	out := make([]api.WorkspaceExecStreamChunkResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, workspaceExecStreamChunkResponse(row))
	}
	writeJSON(w, http.StatusOK, api.WorkerWorkspaceExecInputResponse{
		Chunks:               out,
		StdinClosedAt:        optionalWorkspaceTime(exec.StdinClosedAt),
		StdinCursor:          exec.StdinCursor,
		StdinDeliveredCursor: exec.StdinDeliveredCursor,
		State:                string(exec.State),
	})
}

func (s *Server) workerAdvanceWorkspaceExecInputDelivered(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceExecInputDeliveredRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace exec input delivered request JSON: %w", err)))
		return
	}
	mount, exec, ok := s.loadWorkerWorkspaceExecBoundToWorkspaceMount(w, r, request.WorkerWorkspacePrimitiveScope, request.ExecID)
	if !ok {
		_ = mount
		return
	}
	var row db.WorkspaceExec
	if err := s.inTx(r.Context(), func(work *txWork) error {
		deliveredChunk, err := work.q.GetWorkspaceExecStreamChunkAtOffset(r.Context(), db.GetWorkspaceExecStreamChunkAtOffsetParams{
			OrgID:         mount.OrgID,
			ProjectID:     mount.ProjectID,
			EnvironmentID: mount.EnvironmentID,
			WorkspaceID:   mount.WorkspaceID,
			ExecID:        exec.ID,
			Stream:        db.WorkspaceExecStreamStdin,
			OffsetStart:   request.OffsetStart,
		})
		if err != nil {
			if isNoRows(err) {
				current, getErr := work.q.GetWorkspaceExec(r.Context(), db.GetWorkspaceExecParams{
					OrgID:         mount.OrgID,
					ProjectID:     mount.ProjectID,
					EnvironmentID: mount.EnvironmentID,
					WorkspaceID:   mount.WorkspaceID,
					ID:            exec.ID,
				})
				if getErr == nil && current.StdinDeliveredCursor >= request.OffsetEnd {
					row = current
					return nil
				}
			}
			return err
		}
		if deliveredChunk.OffsetEnd != request.OffsetEnd {
			return conflict(errWorkspaceStreamOffsetConflict)
		}
		deliveredDigest := streamDataSHA256(deliveredChunk.Data)
		if _, err := work.q.InsertWorkspaceExecStreamChunkReceipt(r.Context(), db.InsertWorkspaceExecStreamChunkReceiptParams{
			OrgID:         mount.OrgID,
			ProjectID:     mount.ProjectID,
			EnvironmentID: mount.EnvironmentID,
			WorkspaceID:   mount.WorkspaceID,
			ExecID:        exec.ID,
			Stream:        db.WorkspaceExecStreamStdin,
			OffsetStart:   deliveredChunk.OffsetStart,
			OffsetEnd:     deliveredChunk.OffsetEnd,
			DataSha256:    deliveredDigest,
			DataSize:      int32(len(deliveredChunk.Data)),
			ObservedAt:    nil,
		}); err != nil {
			if isNoRows(err) {
				receipt, getErr := work.q.GetWorkspaceExecStreamChunkReceiptAtOffset(r.Context(), db.GetWorkspaceExecStreamChunkReceiptAtOffsetParams{
					OrgID:         mount.OrgID,
					ProjectID:     mount.ProjectID,
					EnvironmentID: mount.EnvironmentID,
					WorkspaceID:   mount.WorkspaceID,
					ExecID:        exec.ID,
					Stream:        db.WorkspaceExecStreamStdin,
					OffsetStart:   deliveredChunk.OffsetStart,
				})
				if getErr == nil && receipt.OffsetEnd == deliveredChunk.OffsetEnd && receipt.DataSize == int32(len(deliveredChunk.Data)) && bytes.Equal(receipt.DataSha256, deliveredDigest) {
					err = nil
				} else {
					return conflict(errWorkspaceStreamOffsetConflict)
				}
			}
			if err != nil {
				return err
			}
		}
		row, err = work.q.AdvanceWorkspaceExecStdinDeliveredCursor(r.Context(), db.AdvanceWorkspaceExecStdinDeliveredCursorParams{
			OffsetEnd:     request.OffsetEnd,
			OrgID:         mount.OrgID,
			ProjectID:     mount.ProjectID,
			EnvironmentID: mount.EnvironmentID,
			WorkspaceID:   mount.WorkspaceID,
			ExecID:        exec.ID,
			OffsetStart:   request.OffsetStart,
		})
		if err != nil {
			if isNoRows(err) {
				current, getErr := work.q.GetWorkspaceExec(r.Context(), db.GetWorkspaceExecParams{
					OrgID:         mount.OrgID,
					ProjectID:     mount.ProjectID,
					EnvironmentID: mount.EnvironmentID,
					WorkspaceID:   mount.WorkspaceID,
					ID:            exec.ID,
				})
				if getErr == nil && current.StdinDeliveredCursor >= request.OffsetEnd {
					row = current
					return nil
				}
			}
			return err
		}
		if err := work.q.DeleteWorkspaceExecStreamChunksBefore(r.Context(), db.DeleteWorkspaceExecStreamChunksBeforeParams{
			OrgID:             mount.OrgID,
			ProjectID:         mount.ProjectID,
			EnvironmentID:     mount.EnvironmentID,
			WorkspaceID:       mount.WorkspaceID,
			ExecID:            exec.ID,
			Stream:            db.WorkspaceExecStreamStdin,
			RetainAfterOffset: request.OffsetEnd,
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		s.writeWorkspacePrimitiveError(w, "advance workspace exec input delivered", err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspaceExecEnvelope{Exec: workspaceExecResponse(row)})
}

func (s *Server) workerMarkWorkspaceExecExited(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceExecExitedRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace exec exited request JSON: %w", err)))
		return
	}
	mount, err := s.validateWorkerWorkspacePrimitiveScope(r.Context(), workerFromContext(r.Context()), request.WorkerWorkspacePrimitiveScope)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "validate workspace exec terminal scope", err)
		return
	}
	execID, err := parseWorkerPrimitiveUUID("exec_id", request.ExecID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	state, err := normalizeWorkerWorkspaceExecTerminalState(request.State)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	exitCode := pgtype.Int4{}
	if request.ExitCode != nil {
		exitCode = pgvalue.Int4Ptr(request.ExitCode)
	}
	errJSON, err := normalizedOptionalJSONObject(request.Error, "error")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if len(errJSON) == 0 {
		errJSON = []byte(`{}`)
	}
	row, err := s.db.MarkWorkspaceExecExited(r.Context(), db.MarkWorkspaceExecExitedParams{
		State:            state,
		ExitCode:         exitCode,
		Signal:           strings.TrimSpace(request.Signal),
		Error:            errJSON,
		OrgID:            mount.OrgID,
		ProjectID:        mount.ProjectID,
		EnvironmentID:    mount.EnvironmentID,
		WorkspaceID:      mount.WorkspaceID,
		ID:               execID,
		WorkspaceMountID: mount.ID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			existing, getErr := s.db.GetWorkspaceExec(r.Context(), db.GetWorkspaceExecParams{
				OrgID:         mount.OrgID,
				ProjectID:     mount.ProjectID,
				EnvironmentID: mount.EnvironmentID,
				WorkspaceID:   mount.WorkspaceID,
				ID:            execID,
			})
			if getErr == nil && workspaceExecTerminalEventMatches(existing, mount.ID, state, exitCode, strings.TrimSpace(request.Signal), errJSON) {
				writeJSON(w, http.StatusOK, api.WorkspaceExecEnvelope{Exec: workspaceExecResponse(existing)})
				return
			}
			if getErr == nil {
				writeError(w, conflict(errWorkspaceLifecycleEventConflict))
				return
			}
		}
		s.writeWorkspacePrimitiveError(w, "mark workspace exec terminal", err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspaceExecEnvelope{Exec: workspaceExecResponse(workspaceExecFromExitedRow(row))})
}

func workspaceExecTerminalEventMatches(row db.WorkspaceExec, workspaceMountID pgtype.UUID, state db.WorkspaceExecState, exitCode pgtype.Int4, signal string, errorJSON []byte) bool {
	if !workerPrimitiveWorkspaceMountMatches(row.WorkspaceMountID, workspaceMountID) {
		return false
	}
	if row.State == db.WorkspaceExecStateLost || row.State == db.WorkspaceExecStateTerminated {
		return true
	}
	if row.State != state {
		return false
	}
	if !workerPrimitiveInt4Equal(row.ExitCode, exitCode) {
		return false
	}
	if row.Signal != signal {
		return false
	}
	return workerPrimitiveJSONEqual(row.Error, errorJSON)
}

func (s *Server) loadWorkerWorkspaceExecBoundToWorkspaceMount(w http.ResponseWriter, r *http.Request, scope api.WorkerWorkspacePrimitiveScope, rawExecID string) (db.WorkspaceMount, db.WorkspaceExec, bool) {
	mount, err := s.validateWorkerWorkspacePrimitiveScope(r.Context(), workerFromContext(r.Context()), scope)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "validate workspace exec scope", err)
		return db.WorkspaceMount{}, db.WorkspaceExec{}, false
	}
	execID, err := parseWorkerPrimitiveUUID("exec_id", rawExecID)
	if err != nil {
		writeError(w, badRequest(err))
		return db.WorkspaceMount{}, db.WorkspaceExec{}, false
	}
	exec, err := s.db.GetWorkspaceExec(r.Context(), db.GetWorkspaceExecParams{
		OrgID:         mount.OrgID,
		ProjectID:     mount.ProjectID,
		EnvironmentID: mount.EnvironmentID,
		WorkspaceID:   mount.WorkspaceID,
		ID:            execID,
	})
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "get workspace exec", err)
		return db.WorkspaceMount{}, db.WorkspaceExec{}, false
	}
	if !exec.WorkspaceMountID.Valid || pgvalue.MustUUIDValue(exec.WorkspaceMountID) != pgvalue.MustUUIDValue(mount.ID) {
		writeError(w, conflict(errors.New("workspace exec is not bound to this workspace mount")))
		return db.WorkspaceMount{}, db.WorkspaceExec{}, false
	}
	return mount, exec, true
}

func (s *Server) appendWorkspaceExecOutputStreamChunk(ctx context.Context, exec db.WorkspaceExec, stream db.WorkspaceExecStream, requestedOffset *int64, data []byte) (db.WorkspaceExecStreamChunk, error) {
	if requestedOffset == nil {
		return db.WorkspaceExecStreamChunk{}, badRequest(errors.New("offset_start is required"))
	}
	if *requestedOffset < 0 {
		return db.WorkspaceExecStreamChunk{}, badRequest(errors.New("offset must be non-negative"))
	}
	if len(data) == 0 {
		return db.WorkspaceExecStreamChunk{}, badRequest(errors.New("data is required"))
	}
	if len(data) > workspaceStreamChunkMaxBytes {
		return db.WorkspaceExecStreamChunk{}, tooLarge(fmt.Errorf("stream chunk is %d bytes, exceeds max %d", len(data), workspaceStreamChunkMaxBytes))
	}
	var chunk db.WorkspaceExecStreamChunk
	err := s.inTx(ctx, func(work *txWork) error {
		locked, err := work.q.LockWorkspaceExecForStreamAppend(ctx, db.LockWorkspaceExecForStreamAppendParams{
			OrgID:         exec.OrgID,
			ProjectID:     exec.ProjectID,
			EnvironmentID: exec.EnvironmentID,
			WorkspaceID:   exec.WorkspaceID,
			ExecID:        exec.ID,
		})
		if err != nil {
			return err
		}
		tail := execStreamCursor(locked, stream)
		offset := *requestedOffset
		if offset != tail {
			existing, getErr := work.q.GetWorkspaceExecStreamChunkAtOffset(ctx, db.GetWorkspaceExecStreamChunkAtOffsetParams{
				OrgID:         exec.OrgID,
				ProjectID:     exec.ProjectID,
				EnvironmentID: exec.EnvironmentID,
				WorkspaceID:   exec.WorkspaceID,
				ExecID:        exec.ID,
				Stream:        stream,
				OffsetStart:   offset,
			})
			if getErr == nil && existing.OffsetEnd == offset+int64(len(data)) && bytes.Equal(existing.Data, data) {
				chunk = existing
				return nil
			}
			return conflict(errWorkspaceStreamOffsetConflict)
		}
		chunk, err = work.q.InsertWorkspaceExecOutputStreamChunk(ctx, db.InsertWorkspaceExecOutputStreamChunkParams{
			OrgID:         exec.OrgID,
			ProjectID:     exec.ProjectID,
			EnvironmentID: exec.EnvironmentID,
			WorkspaceID:   exec.WorkspaceID,
			ExecID:        exec.ID,
			Stream:        stream,
			OffsetStart:   offset,
			OffsetEnd:     offset + int64(len(data)),
			Data:          data,
			ObservedAt:    nil,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			existing, getErr := work.q.GetWorkspaceExecStreamChunkAtOffset(ctx, db.GetWorkspaceExecStreamChunkAtOffsetParams{
				OrgID:         exec.OrgID,
				ProjectID:     exec.ProjectID,
				EnvironmentID: exec.EnvironmentID,
				WorkspaceID:   exec.WorkspaceID,
				ExecID:        exec.ID,
				Stream:        stream,
				OffsetStart:   offset,
			})
			if getErr == nil && existing.OffsetEnd == offset+int64(len(data)) && bytes.Equal(existing.Data, data) {
				chunk = existing
				return nil
			}
			return conflict(errWorkspaceStreamOffsetConflict)
		}
		if err != nil {
			if isUniqueViolation(err) || isExclusionViolation(err) {
				return conflict(errWorkspaceStreamOffsetConflict)
			}
			return err
		}
		row, err := work.q.AdvanceWorkspaceExecOutputCursor(ctx, db.AdvanceWorkspaceExecOutputCursorParams{
			Stream:        stream,
			OrgID:         exec.OrgID,
			ProjectID:     exec.ProjectID,
			EnvironmentID: exec.EnvironmentID,
			WorkspaceID:   exec.WorkspaceID,
			ExecID:        exec.ID,
		})
		if err != nil {
			return err
		}
		retainAfter := execStreamCursorFromRow(row, stream) - workspaceStreamRetainedMaxBytes
		if retainAfter > 0 {
			if err := work.q.DeleteWorkspaceExecStreamChunksBefore(ctx, db.DeleteWorkspaceExecStreamChunksBeforeParams{
				OrgID:             exec.OrgID,
				ProjectID:         exec.ProjectID,
				EnvironmentID:     exec.EnvironmentID,
				WorkspaceID:       exec.WorkspaceID,
				ExecID:            exec.ID,
				Stream:            stream,
				RetainAfterOffset: retainAfter,
			}); err != nil {
				return err
			}
		}
		if _, err := work.q.CreateWorkspaceStreamWakeup(ctx, db.CreateWorkspaceStreamWakeupParams{
			OrgID:            exec.OrgID,
			ProjectID:        exec.ProjectID,
			EnvironmentID:    exec.EnvironmentID,
			WorkspaceID:      exec.WorkspaceID,
			ResourceKind:     db.WorkspaceResourceKindWorkspaceExec,
			ResourceID:       exec.ID,
			Stream:           string(stream),
			CursorOffset:     chunk.OffsetEnd,
			NotificationKind: db.WorkspaceStreamNotificationKindChunk,
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return db.WorkspaceExecStreamChunk{}, err
	}
	return chunk, nil
}

func normalizeWorkerWorkspaceExecTerminalState(raw string) (db.WorkspaceExecState, error) {
	switch db.WorkspaceExecState(strings.TrimSpace(raw)) {
	case db.WorkspaceExecStateExited:
		return db.WorkspaceExecStateExited, nil
	case db.WorkspaceExecStateFailed:
		return db.WorkspaceExecStateFailed, nil
	default:
		return "", errors.New("exec terminal state must be exited or failed")
	}
}
