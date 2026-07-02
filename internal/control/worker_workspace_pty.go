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

func (s *Server) workerMarkWorkspacePtyOpened(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspacePtyOpenedRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace pty opened request JSON: %w", err)))
		return
	}
	mount, err := s.validateWorkerWorkspacePrimitiveScope(r.Context(), workerFromContext(r.Context()), request.WorkerWorkspacePrimitiveScope)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "validate workspace pty event scope", err)
		return
	}
	ptyID, err := parseWorkerPrimitiveUUID("pty_id", request.PtyID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	row, err := s.db.MarkWorkspacePtyOpen(r.Context(), db.MarkWorkspacePtyOpenParams{
		ProcessID:        strings.TrimSpace(request.ProcessID),
		OrgID:            mount.OrgID,
		ProjectID:        mount.ProjectID,
		EnvironmentID:    mount.EnvironmentID,
		WorkspaceID:      mount.WorkspaceID,
		ID:               ptyID,
		WorkspaceMountID: mount.ID,
	})
	if err != nil {
		if isNoRows(err) {
			existing, getErr := s.db.GetWorkspacePtySession(r.Context(), db.GetWorkspacePtySessionParams{
				OrgID:         mount.OrgID,
				ProjectID:     mount.ProjectID,
				EnvironmentID: mount.EnvironmentID,
				WorkspaceID:   mount.WorkspaceID,
				ID:            ptyID,
			})
			if getErr == nil && workspacePtyTerminalEventMatches(existing, mount.ID, false, nil) {
				writeJSON(w, http.StatusOK, api.WorkspacePtyEnvelope{Pty: workspacePtyResponse(existing)})
				return
			}
		}
		s.writeWorkspacePrimitiveError(w, "mark workspace pty open", err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspacePtyEnvelope{Pty: workspacePtyResponse(db.WorkspacePtySession(row))})
}

func (s *Server) workerAppendWorkspacePtyOutput(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspacePtyOutputRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace pty output request JSON: %w", err)))
		return
	}
	mount, err := s.validateWorkerWorkspacePrimitiveScope(r.Context(), workerFromContext(r.Context()), request.WorkerWorkspacePrimitiveScope)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "validate workspace pty output scope", err)
		return
	}
	ptyID, err := parseWorkerPrimitiveUUID("pty_id", request.PtyID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	pty, err := s.db.GetWorkspacePtySession(r.Context(), db.GetWorkspacePtySessionParams{
		OrgID:         mount.OrgID,
		ProjectID:     mount.ProjectID,
		EnvironmentID: mount.EnvironmentID,
		WorkspaceID:   mount.WorkspaceID,
		ID:            ptyID,
	})
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "get workspace pty", err)
		return
	}
	if !pty.WorkspaceMountID.Valid || pgvalue.MustUUIDValue(pty.WorkspaceMountID) != pgvalue.MustUUIDValue(mount.ID) {
		writeError(w, conflict(errors.New("workspace pty is not bound to this workspace mount")))
		return
	}
	out := make([]api.WorkspacePtyStreamChunkResponse, 0, len(request.Chunks))
	for _, input := range request.Chunks {
		chunk, err := s.appendWorkspacePtyOutputStreamChunk(r.Context(), pty, input.OffsetStart, input.Data)
		if err != nil {
			s.writeWorkspacePrimitiveError(w, "append workspace pty output", err)
			return
		}
		out = append(out, workspacePtyStreamChunkResponse(chunk))
	}
	writeJSON(w, http.StatusOK, api.ListWorkspacePtyStreamChunksResponse{Chunks: out})
}

func (s *Server) workerListWorkspacePtyInput(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspacePtyInputRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace pty input request JSON: %w", err)))
		return
	}
	mount, pty, ok := s.loadWorkerWorkspacePtyBoundToWorkspaceMount(w, r, request.WorkerWorkspacePrimitiveScope, request.PtyID)
	if !ok {
		_ = mount
		return
	}
	limit := request.Limit
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	rows, err := s.db.ListWorkspacePtyInputChunksAfterDelivered(r.Context(), db.ListWorkspacePtyInputChunksAfterDeliveredParams{
		OrgID:         mount.OrgID,
		ProjectID:     mount.ProjectID,
		EnvironmentID: mount.EnvironmentID,
		WorkspaceID:   mount.WorkspaceID,
		PtySessionID:  pty.ID,
		LimitCount:    limit,
	})
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "list workspace pty input", err)
		return
	}
	out := make([]api.WorkspacePtyStreamChunkResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, workspacePtyStreamChunkResponse(row))
	}
	writeJSON(w, http.StatusOK, api.WorkerWorkspacePtyInputResponse{
		Chunks:               out,
		InputCursor:          pty.InputCursor,
		InputDeliveredCursor: pty.InputDeliveredCursor,
		State:                string(pty.State),
	})
}

func (s *Server) workerAdvanceWorkspacePtyInputDelivered(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspacePtyInputDeliveredRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace pty input delivered request JSON: %w", err)))
		return
	}
	mount, pty, ok := s.loadWorkerWorkspacePtyBoundToWorkspaceMount(w, r, request.WorkerWorkspacePrimitiveScope, request.PtyID)
	if !ok {
		_ = mount
		return
	}
	var row db.WorkspacePtySession
	if err := s.inTx(r.Context(), func(work *txWork) error {
		deliveredChunk, err := work.q.GetWorkspacePtyStreamChunkAtOffset(r.Context(), db.GetWorkspacePtyStreamChunkAtOffsetParams{
			OrgID:         mount.OrgID,
			ProjectID:     mount.ProjectID,
			EnvironmentID: mount.EnvironmentID,
			WorkspaceID:   mount.WorkspaceID,
			PtySessionID:  pty.ID,
			Stream:        db.WorkspacePtyStreamInput,
			OffsetStart:   request.OffsetStart,
		})
		if err != nil {
			if isNoRows(err) {
				current, getErr := work.q.GetWorkspacePtySession(r.Context(), db.GetWorkspacePtySessionParams{
					OrgID:         mount.OrgID,
					ProjectID:     mount.ProjectID,
					EnvironmentID: mount.EnvironmentID,
					WorkspaceID:   mount.WorkspaceID,
					ID:            pty.ID,
				})
				if getErr == nil && current.InputDeliveredCursor >= request.OffsetEnd {
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
		if _, err := work.q.InsertWorkspacePtyStreamChunkReceipt(r.Context(), db.InsertWorkspacePtyStreamChunkReceiptParams{
			OrgID:         mount.OrgID,
			ProjectID:     mount.ProjectID,
			EnvironmentID: mount.EnvironmentID,
			WorkspaceID:   mount.WorkspaceID,
			PtySessionID:  pty.ID,
			Stream:        db.WorkspacePtyStreamInput,
			OffsetStart:   deliveredChunk.OffsetStart,
			OffsetEnd:     deliveredChunk.OffsetEnd,
			DataSha256:    deliveredDigest,
			DataSize:      int32(len(deliveredChunk.Data)),
			ObservedAt:    nil,
		}); err != nil {
			if isNoRows(err) {
				receipt, getErr := work.q.GetWorkspacePtyStreamChunkReceiptAtOffset(r.Context(), db.GetWorkspacePtyStreamChunkReceiptAtOffsetParams{
					OrgID:         mount.OrgID,
					ProjectID:     mount.ProjectID,
					EnvironmentID: mount.EnvironmentID,
					WorkspaceID:   mount.WorkspaceID,
					PtySessionID:  pty.ID,
					Stream:        db.WorkspacePtyStreamInput,
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
		row, err = work.q.AdvanceWorkspacePtyInputDeliveredCursor(r.Context(), db.AdvanceWorkspacePtyInputDeliveredCursorParams{
			OffsetEnd:     request.OffsetEnd,
			OrgID:         mount.OrgID,
			ProjectID:     mount.ProjectID,
			EnvironmentID: mount.EnvironmentID,
			WorkspaceID:   mount.WorkspaceID,
			PtySessionID:  pty.ID,
			OffsetStart:   request.OffsetStart,
		})
		if err != nil {
			if isNoRows(err) {
				current, getErr := work.q.GetWorkspacePtySession(r.Context(), db.GetWorkspacePtySessionParams{
					OrgID:         mount.OrgID,
					ProjectID:     mount.ProjectID,
					EnvironmentID: mount.EnvironmentID,
					WorkspaceID:   mount.WorkspaceID,
					ID:            pty.ID,
				})
				if getErr == nil && current.InputDeliveredCursor >= request.OffsetEnd {
					row = current
					return nil
				}
			}
			return err
		}
		if err := work.q.DeleteWorkspacePtyStreamChunksBefore(r.Context(), db.DeleteWorkspacePtyStreamChunksBeforeParams{
			OrgID:             mount.OrgID,
			ProjectID:         mount.ProjectID,
			EnvironmentID:     mount.EnvironmentID,
			WorkspaceID:       mount.WorkspaceID,
			PtySessionID:      pty.ID,
			Stream:            db.WorkspacePtyStreamInput,
			RetainAfterOffset: request.OffsetEnd,
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		s.writeWorkspacePrimitiveError(w, "advance workspace pty input delivered", err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspacePtyEnvelope{Pty: workspacePtyResponse(row)})
}

func (s *Server) workerMarkWorkspacePtyResizeApplied(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspacePtyResizeAppliedRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace pty resize request JSON: %w", err)))
		return
	}
	mount, err := s.validateWorkerWorkspacePrimitiveScope(r.Context(), workerFromContext(r.Context()), request.WorkerWorkspacePrimitiveScope)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "validate workspace pty resize scope", err)
		return
	}
	ptyID, err := parseWorkerPrimitiveUUID("pty_id", request.PtyID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	row, err := s.db.MarkWorkspacePtyResizeApplied(r.Context(), db.MarkWorkspacePtyResizeAppliedParams{
		OrgID:            mount.OrgID,
		ProjectID:        mount.ProjectID,
		EnvironmentID:    mount.EnvironmentID,
		WorkspaceID:      mount.WorkspaceID,
		ID:               ptyID,
		WorkspaceMountID: mount.ID,
		Cols:             pgtype.Int4{Int32: request.Cols, Valid: true},
		Rows:             pgtype.Int4{Int32: request.Rows, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			existing, getErr := s.db.GetWorkspacePtySession(r.Context(), db.GetWorkspacePtySessionParams{
				OrgID:         mount.OrgID,
				ProjectID:     mount.ProjectID,
				EnvironmentID: mount.EnvironmentID,
				WorkspaceID:   mount.WorkspaceID,
				ID:            ptyID,
			})
			if getErr == nil && workspacePtyResizeAppliedEventMatches(existing, mount.ID, request.Cols, request.Rows) {
				writeJSON(w, http.StatusOK, api.WorkspacePtyEnvelope{Pty: workspacePtyResponse(existing)})
				return
			}
			if getErr == nil {
				writeError(w, conflict(errWorkspaceLifecycleEventConflict))
				return
			}
		}
		s.writeWorkspacePrimitiveError(w, "mark workspace pty resize applied", err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspacePtyEnvelope{Pty: workspacePtyResponse(row)})
}

func (s *Server) loadWorkerWorkspacePtyBoundToWorkspaceMount(w http.ResponseWriter, r *http.Request, scope api.WorkerWorkspacePrimitiveScope, rawPtyID string) (db.WorkspaceMount, db.WorkspacePtySession, bool) {
	mount, err := s.validateWorkerWorkspacePrimitiveScope(r.Context(), workerFromContext(r.Context()), scope)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "validate workspace pty scope", err)
		return db.WorkspaceMount{}, db.WorkspacePtySession{}, false
	}
	ptyID, err := parseWorkerPrimitiveUUID("pty_id", rawPtyID)
	if err != nil {
		writeError(w, badRequest(err))
		return db.WorkspaceMount{}, db.WorkspacePtySession{}, false
	}
	pty, err := s.db.GetWorkspacePtySession(r.Context(), db.GetWorkspacePtySessionParams{
		OrgID:         mount.OrgID,
		ProjectID:     mount.ProjectID,
		EnvironmentID: mount.EnvironmentID,
		WorkspaceID:   mount.WorkspaceID,
		ID:            ptyID,
	})
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "get workspace pty", err)
		return db.WorkspaceMount{}, db.WorkspacePtySession{}, false
	}
	if !pty.WorkspaceMountID.Valid || pgvalue.MustUUIDValue(pty.WorkspaceMountID) != pgvalue.MustUUIDValue(mount.ID) {
		writeError(w, conflict(errors.New("workspace pty is not bound to this workspace mount")))
		return db.WorkspaceMount{}, db.WorkspacePtySession{}, false
	}
	return mount, pty, true
}

func (s *Server) workerMarkWorkspacePtyClosed(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspacePtyClosedRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace pty closed request JSON: %w", err)))
		return
	}
	mount, err := s.validateWorkerWorkspacePrimitiveScope(r.Context(), workerFromContext(r.Context()), request.WorkerWorkspacePrimitiveScope)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "validate workspace pty terminal scope", err)
		return
	}
	ptyID, err := parseWorkerPrimitiveUUID("pty_id", request.PtyID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	errorJSON, err := normalizedOptionalJSONObject(request.Error, "error")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var row db.WorkspacePtySession
	if len(errorJSON) > 0 {
		failed, markErr := s.db.MarkWorkspacePtyFailed(r.Context(), db.MarkWorkspacePtyFailedParams{
			Error:            errorJSON,
			OrgID:            mount.OrgID,
			ProjectID:        mount.ProjectID,
			EnvironmentID:    mount.EnvironmentID,
			WorkspaceID:      mount.WorkspaceID,
			ID:               ptyID,
			WorkspaceMountID: mount.ID,
		})
		err = markErr
		row = workspacePtyFromFailedRow(failed)
	} else {
		closed, markErr := s.db.MarkWorkspacePtyClosed(r.Context(), db.MarkWorkspacePtyClosedParams{
			OrgID:            mount.OrgID,
			ProjectID:        mount.ProjectID,
			EnvironmentID:    mount.EnvironmentID,
			WorkspaceID:      mount.WorkspaceID,
			ID:               ptyID,
			WorkspaceMountID: mount.ID,
		})
		err = markErr
		row = workspacePtyFromClosedRow(closed)
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			existing, getErr := s.db.GetWorkspacePtySession(r.Context(), db.GetWorkspacePtySessionParams{
				OrgID:         mount.OrgID,
				ProjectID:     mount.ProjectID,
				EnvironmentID: mount.EnvironmentID,
				WorkspaceID:   mount.WorkspaceID,
				ID:            ptyID,
			})
			if getErr == nil && workspacePtyTerminalEventMatches(existing, mount.ID, len(errorJSON) > 0, errorJSON) {
				writeJSON(w, http.StatusOK, api.WorkspacePtyEnvelope{Pty: workspacePtyResponse(existing)})
				return
			}
			if getErr == nil {
				writeError(w, conflict(errWorkspaceLifecycleEventConflict))
				return
			}
		}
		s.writeWorkspacePrimitiveError(w, "mark workspace pty terminal", err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspacePtyEnvelope{Pty: workspacePtyResponse(row)})
}

func workspacePtyResizeAppliedEventMatches(row db.WorkspacePtySession, workspaceMountID pgtype.UUID, cols int32, rows int32) bool {
	return workerPrimitiveWorkspaceMountMatches(row.WorkspaceMountID, workspaceMountID) &&
		(row.State == db.WorkspacePtyStateOpen || row.State == db.WorkspacePtyStateClosing || row.State == db.WorkspacePtyStateClosed) &&
		row.Cols == cols &&
		row.Rows == rows
}

func workspacePtyTerminalEventMatches(row db.WorkspacePtySession, workspaceMountID pgtype.UUID, failed bool, errorJSON []byte) bool {
	if !workerPrimitiveWorkspaceMountMatches(row.WorkspaceMountID, workspaceMountID) {
		return false
	}
	if row.State == db.WorkspacePtyStateLost {
		return true
	}
	if failed {
		return row.State == db.WorkspacePtyStateFailed && workerPrimitiveJSONEqual(row.Error, errorJSON)
	}
	return row.State == db.WorkspacePtyStateClosed
}

func (s *Server) appendWorkspacePtyOutputStreamChunk(ctx context.Context, pty db.WorkspacePtySession, requestedOffset *int64, data []byte) (db.WorkspacePtyStreamChunk, error) {
	if requestedOffset == nil {
		return db.WorkspacePtyStreamChunk{}, badRequest(errors.New("offset_start is required"))
	}
	if *requestedOffset < 0 {
		return db.WorkspacePtyStreamChunk{}, badRequest(errors.New("offset must be non-negative"))
	}
	if len(data) == 0 {
		return db.WorkspacePtyStreamChunk{}, badRequest(errors.New("data is required"))
	}
	if len(data) > workspaceStreamChunkMaxBytes {
		return db.WorkspacePtyStreamChunk{}, tooLarge(fmt.Errorf("stream chunk is %d bytes, exceeds max %d", len(data), workspaceStreamChunkMaxBytes))
	}
	var chunk db.WorkspacePtyStreamChunk
	err := s.inTx(ctx, func(work *txWork) error {
		locked, err := work.q.LockWorkspacePtyForStreamAppend(ctx, db.LockWorkspacePtyForStreamAppendParams{
			OrgID:         pty.OrgID,
			ProjectID:     pty.ProjectID,
			EnvironmentID: pty.EnvironmentID,
			WorkspaceID:   pty.WorkspaceID,
			PtySessionID:  pty.ID,
		})
		if err != nil {
			return err
		}
		tail := ptyStreamCursor(locked, db.WorkspacePtyStreamOutput)
		offset := *requestedOffset
		if offset != tail {
			existing, getErr := work.q.GetWorkspacePtyStreamChunkAtOffset(ctx, db.GetWorkspacePtyStreamChunkAtOffsetParams{
				OrgID:         pty.OrgID,
				ProjectID:     pty.ProjectID,
				EnvironmentID: pty.EnvironmentID,
				WorkspaceID:   pty.WorkspaceID,
				PtySessionID:  pty.ID,
				Stream:        db.WorkspacePtyStreamOutput,
				OffsetStart:   offset,
			})
			if getErr == nil && existing.OffsetEnd == offset+int64(len(data)) && bytes.Equal(existing.Data, data) {
				chunk = existing
				return nil
			}
			return conflict(errWorkspaceStreamOffsetConflict)
		}
		chunk, err = work.q.InsertWorkspacePtyOutputStreamChunk(ctx, db.InsertWorkspacePtyOutputStreamChunkParams{
			OrgID:         pty.OrgID,
			ProjectID:     pty.ProjectID,
			EnvironmentID: pty.EnvironmentID,
			WorkspaceID:   pty.WorkspaceID,
			PtySessionID:  pty.ID,
			Stream:        db.WorkspacePtyStreamOutput,
			OffsetStart:   offset,
			OffsetEnd:     offset + int64(len(data)),
			Data:          data,
			ObservedAt:    nil,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			existing, getErr := work.q.GetWorkspacePtyStreamChunkAtOffset(ctx, db.GetWorkspacePtyStreamChunkAtOffsetParams{
				OrgID:         pty.OrgID,
				ProjectID:     pty.ProjectID,
				EnvironmentID: pty.EnvironmentID,
				WorkspaceID:   pty.WorkspaceID,
				PtySessionID:  pty.ID,
				Stream:        db.WorkspacePtyStreamOutput,
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
		row, err := work.q.AdvanceWorkspacePtyOutputCursor(ctx, db.AdvanceWorkspacePtyOutputCursorParams{
			Stream:        db.WorkspacePtyStreamOutput,
			OrgID:         pty.OrgID,
			ProjectID:     pty.ProjectID,
			EnvironmentID: pty.EnvironmentID,
			WorkspaceID:   pty.WorkspaceID,
			PtySessionID:  pty.ID,
		})
		if err != nil {
			return err
		}
		retainAfter := row.OutputCursor - workspaceStreamRetainedMaxBytes
		if retainAfter > 0 {
			if err := work.q.DeleteWorkspacePtyStreamChunksBefore(ctx, db.DeleteWorkspacePtyStreamChunksBeforeParams{
				OrgID:             pty.OrgID,
				ProjectID:         pty.ProjectID,
				EnvironmentID:     pty.EnvironmentID,
				WorkspaceID:       pty.WorkspaceID,
				PtySessionID:      pty.ID,
				Stream:            db.WorkspacePtyStreamOutput,
				RetainAfterOffset: retainAfter,
			}); err != nil {
				return err
			}
		}
		if _, err := work.q.CreateWorkspaceStreamWakeup(ctx, db.CreateWorkspaceStreamWakeupParams{
			OrgID:            pty.OrgID,
			ProjectID:        pty.ProjectID,
			EnvironmentID:    pty.EnvironmentID,
			WorkspaceID:      pty.WorkspaceID,
			ResourceKind:     db.WorkspaceResourceKindWorkspacePty,
			ResourceID:       pty.ID,
			Stream:           string(db.WorkspacePtyStreamOutput),
			CursorOffset:     chunk.OffsetEnd,
			NotificationKind: db.WorkspaceStreamNotificationKindChunk,
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return db.WorkspacePtyStreamChunk{}, err
	}
	return chunk, nil
}
