package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	workspacePtyListDefaultLimit = int32(50)
	workspacePtyListMaxLimit     = int32(200)
)

var (
	errWorkspacePtyTerminal = codedError{code: "workspace_pty_terminal"}
	errWorkspacePtyNotOpen  = codedError{code: "workspace_pty_not_open"}
)

func (s *Server) createWorkspacePty(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.loadWorkspaceForRequest(w, r, auth.PermissionPtyCreate)
	if !ok {
		return
	}
	actor := actorFromContext(r.Context())
	var request api.WorkspacePtyCreateRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace pty request JSON: %w", err)))
		return
	}
	cwd, err := normalizeExecCwd(request.Cwd)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	cols, rows, err := normalizePtySize(request.Cols, request.Rows)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	filesystemMode := db.WorkspaceFilesystemModeWrite
	fingerprint, err := ptyCreateFingerprint(cwd, cols, rows, filesystemMode)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	row, cached, err := s.createWorkspacePtyForRequest(r.Context(), actor, ws, cwd, cols, rows, filesystemMode, strings.TrimSpace(request.IdempotencyKey), fingerprint)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "create workspace pty", err)
		return
	}
	status := http.StatusCreated
	if cached {
		status = http.StatusOK
	}
	writeJSON(w, status, api.WorkspacePtyEnvelope{Pty: workspacePtyResponse(row), IsCached: cached})
}

func (s *Server) listWorkspacePtys(w http.ResponseWriter, r *http.Request) {
	workspace, ok := s.loadWorkspaceForRequest(w, r, auth.PermissionPtyRead)
	if !ok {
		return
	}
	limit, err := parseWorkspacePrimitiveLimit(r, workspacePtyListDefaultLimit, workspacePtyListMaxLimit)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	state := db.NullWorkspaceProcessState{}
	if raw := strings.TrimSpace(r.URL.Query().Get("state")); raw != "" {
		normalized, err := normalizeWorkspacePtyStateFilter(raw)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
		state = db.NullWorkspaceProcessState{WorkspaceProcessState: normalized, Valid: true}
	}
	rows, err := s.db.ListWorkspacePtySessions(r.Context(), db.ListWorkspacePtySessionsParams{
		OrgID:         workspace.OrgID,
		ProjectID:     workspace.ProjectID,
		EnvironmentID: workspace.EnvironmentID,
		WorkspaceID:   workspace.ID,
		State:         state,
		LimitCount:    limit,
	})
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "list workspace ptys", err)
		return
	}
	out := make([]api.WorkspacePtyResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, workspacePtyResponse(row))
	}
	writeJSON(w, http.StatusOK, api.ListWorkspacePtySessionsResponse{Ptys: out})
}

func normalizeWorkspacePtyStateFilter(raw string) (db.WorkspaceProcessState, error) {
	switch db.WorkspaceProcessState(strings.TrimSpace(raw)) {
	case db.WorkspaceProcessStateStarting,
		db.WorkspaceProcessStateRunning,
		db.WorkspaceProcessStateClosing,
		db.WorkspaceProcessStateExited,
		db.WorkspaceProcessStateLost,
		db.WorkspaceProcessStateFailed:
		return db.WorkspaceProcessState(strings.TrimSpace(raw)), nil
	default:
		return "", errors.New("state must be one of creating, open, resizing, closing, closed, lost, failed")
	}
}

func (s *Server) getWorkspacePty(w http.ResponseWriter, r *http.Request) {
	pty, ok := s.loadWorkspacePtyForRequest(w, r, auth.PermissionPtyRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspacePtyEnvelope{Pty: workspacePtyResponse(pty)})
}

func (s *Server) writeWorkspacePtyInput(w http.ResponseWriter, r *http.Request) {
	pty, ok := s.loadWorkspacePtyForRequest(w, r, auth.PermissionPtyManage)
	if !ok {
		return
	}
	var request api.WorkspacePtyInputWriteRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace pty input request JSON: %w", err)))
		return
	}
	chunk, err := s.appendWorkspacePtyStreamChunk(r.Context(), pty, workspaceStreamInput, request.Offset, request.Data)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "write workspace pty input", err)
		return
	}
	writeJSON(w, http.StatusOK, workspacePtyStreamChunkResponse(chunk))
}

func (s *Server) listWorkspacePtyOutput(w http.ResponseWriter, r *http.Request) {
	pty, ok := s.loadWorkspacePtyForRequest(w, r, auth.PermissionPtyRead)
	if !ok {
		return
	}
	cursor, err := parseWorkspaceStreamFollowCursor(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if workspaceStreamFollowRequested(r) {
		limit, err := parseWorkspacePrimitiveLimit(r, 100, 500)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
		s.followWorkspacePtyOutput(w, r, pty, cursor, limit)
		return
	}
	limit, err := parseWorkspacePrimitiveLimit(r, 100, 500)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	out, _, err := s.listWorkspacePtyTerminalOutput(r.Context(), pty, cursor, limit)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "list workspace pty output", err)
		return
	}
	writeJSON(w, http.StatusOK, api.ListWorkspacePtyStreamChunksResponse{Chunks: out})
}

func (s *Server) resizeWorkspacePty(w http.ResponseWriter, r *http.Request) {
	pty, ok := s.loadWorkspacePtyForRequest(w, r, auth.PermissionPtyManage)
	if !ok {
		return
	}
	var request api.WorkspacePtyResizeRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace pty resize request JSON: %w", err)))
		return
	}
	cols, rows, err := normalizePtySize(request.Cols, request.Rows)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	row, err := s.requestWorkspacePtyResize(r.Context(), pty, cols, rows)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "resize workspace pty", err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspacePtyEnvelope{Pty: workspacePtyResponse(row)})
}

func (s *Server) closeWorkspacePty(w http.ResponseWriter, r *http.Request) {
	s.requestWorkspacePtyClose(w, r)
}

func (s *Server) requestWorkspacePtyClose(w http.ResponseWriter, r *http.Request) {
	pty, ok := s.loadWorkspacePtyForRequest(w, r, auth.PermissionPtyManage)
	if !ok {
		return
	}
	row, err := s.requestWorkspacePtyCloseOperation(r.Context(), pty)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "close workspace pty", err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspacePtyEnvelope{Pty: workspacePtyResponse(row)})
}

func (s *Server) requestWorkspacePtyResize(ctx context.Context, pty db.WorkspaceProcess, cols int32, rows int32) (db.WorkspaceProcess, error) {
	if pty.State != db.WorkspaceProcessStateRunning {
		return db.WorkspaceProcess{}, conflict(errWorkspacePtyNotOpen)
	}
	var row db.WorkspaceProcess
	err := s.inTx(ctx, func(work *txWork) error {
		var err error
		row, err = work.q.ResizeWorkspacePtySession(ctx, db.ResizeWorkspacePtySessionParams{
			PtyCols:       pgtype.Int4{Int32: cols, Valid: true},
			PtyRows:       pgtype.Int4{Int32: rows, Valid: true},
			OrgID:         pty.OrgID,
			ProjectID:     pty.ProjectID,
			EnvironmentID: pty.EnvironmentID,
			WorkspaceID:   pty.WorkspaceID,
			ID:            pty.ID,
		})
		if err != nil {
			return err
		}
		request, err := ptyResizeOperationRequest(row)
		if err != nil {
			return err
		}
		return requestWorkspacePtyControlOperation(ctx, work.q, row, workspaceOperationKindResizePty, request)
	})
	if err != nil {
		return db.WorkspaceProcess{}, err
	}
	return row, nil
}

func (s *Server) requestWorkspacePtyCloseOperation(ctx context.Context, pty db.WorkspaceProcess) (db.WorkspaceProcess, error) {
	if pty.State != db.WorkspaceProcessStateRunning && pty.State != db.WorkspaceProcessStateClosing {
		return db.WorkspaceProcess{}, conflict(errWorkspacePtyNotOpen)
	}
	var row db.WorkspaceProcess
	err := s.inTx(ctx, func(work *txWork) error {
		var err error
		row, err = work.q.RequestWorkspacePtyClose(ctx, db.RequestWorkspacePtyCloseParams{
			OrgID:         pty.OrgID,
			ProjectID:     pty.ProjectID,
			EnvironmentID: pty.EnvironmentID,
			WorkspaceID:   pty.WorkspaceID,
			ID:            pty.ID,
		})
		if err != nil {
			return err
		}
		request, err := ptyCloseOperationRequest(row)
		if err != nil {
			return err
		}
		return requestWorkspacePtyControlOperation(ctx, work.q, row, workspaceOperationKindClosePty, request)
	})
	if err != nil {
		return db.WorkspaceProcess{}, err
	}
	return row, nil
}

func (s *Server) createWorkspacePtyForRequest(ctx context.Context, actor auth.Actor, ws db.Workspace, cwd string, cols int32, rows int32, filesystemMode db.WorkspaceFilesystemMode, idempotencyKey string, fingerprint string) (db.WorkspaceProcess, bool, error) {
	var row db.WorkspaceProcess
	var existing bool
	idempotencyExpiresAt := pgtype.Timestamptz{}
	if idempotencyKey != "" {
		idempotencyExpiresAt = pgvalue.Timestamptz(time.Now().Add(workspaceExecIdempotencyTTL))
	}
	err := s.inTx(ctx, func(work *txWork) error {
		if idempotencyKey != "" {
			if err := work.q.ClearExpiredWorkspacePtyIdempotency(ctx, db.ClearExpiredWorkspacePtyIdempotencyParams{
				OrgID:          pgvalue.UUID(actor.OrgID),
				ProjectID:      ws.ProjectID,
				EnvironmentID:  ws.EnvironmentID,
				WorkspaceID:    ws.ID,
				IdempotencyKey: idempotencyKey,
			}); err != nil {
				return err
			}
			existingPty, err := work.q.GetWorkspacePtySessionByIdempotency(ctx, db.GetWorkspacePtySessionByIdempotencyParams{
				OrgID:          pgvalue.UUID(actor.OrgID),
				ProjectID:      ws.ProjectID,
				EnvironmentID:  ws.EnvironmentID,
				WorkspaceID:    ws.ID,
				IdempotencyKey: idempotencyKey,
			})
			if err == nil {
				if existingPty.RequestFingerprint != fingerprint {
					return errWorkspaceOperationIdempotencyUsed
				}
				row = existingPty
				existing = true
				return nil
			}
			if !isNoRows(err) {
				return err
			}
		}
		if err := ensureWorkspacePrimitiveWriterAvailable(ctx, work.q, pgvalue.UUID(actor.OrgID), ws.ProjectID, ws.EnvironmentID, ws.ID); err != nil {
			return err
		}
		var err error
		row, err = work.q.CreateWorkspacePtySession(ctx, db.CreateWorkspacePtySessionParams{
			ID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
			Cwd:                  cwd,
			PtyCols:              pgtype.Int4{Int32: cols, Valid: true},
			PtyRows:              pgtype.Int4{Int32: rows, Valid: true},
			FilesystemMode:       filesystemMode,
			State:                db.WorkspaceProcessStateStarting,
			IdempotencyKey:       idempotencyKey,
			IdempotencyExpiresAt: idempotencyExpiresAt,
			RequestFingerprint:   fingerprint,
			CreatedBySubjectType: string(actor.Kind),
			CreatedBySubjectID:   actorSubjectID(actor),
			OrgID:                pgvalue.UUID(actor.OrgID),
			ProjectID:            ws.ProjectID,
			EnvironmentID:        ws.EnvironmentID,
			WorkspaceID:          ws.ID,
		})
		if err != nil {
			return err
		}
		mount, err := work.q.EnsureWorkspaceMountRequested(ctx, db.EnsureWorkspaceMountRequestedParams{
			ID: pgvalue.UUID(uuid.Must(uuid.NewV7())), OrgID: pgvalue.UUID(actor.OrgID),
			WorkspaceID: ws.ID, Priority: 0, Request: []byte(`{"reason":"workspace_pty"}`),
		})
		if err != nil {
			return err
		}
		row, err = work.q.BindWorkspacePtyWorkspaceMount(ctx, db.BindWorkspacePtyWorkspaceMountParams{
			WorkspaceMountID: mount.ID,
			InstanceLeaseID:  pgtype.UUID{},
			WriteLeaseID:     pgtype.UUID{},
			State:            db.WorkspaceProcessStateStarting,
			OrgID:            pgvalue.UUID(actor.OrgID),
			ProjectID:        ws.ProjectID,
			EnvironmentID:    ws.EnvironmentID,
			WorkspaceID:      ws.ID,
			ID:               row.ID,
		})
		if err != nil {
			return err
		}
		if mount.State == db.WorkspaceMountStateMounted {
			var lease workspacePrimitiveOperationLease
			row, lease, err = ensureWorkspacePtyWriteLease(ctx, work.q, workspaceMountFromEnsureRow(mount), row)
			if err != nil {
				return err
			}
			request, err := ptyCreateOperationRequest(row)
			if err != nil {
				return err
			}
			if err := requestWorkspacePrimitiveOperation(ctx, work.q, workspaceMountFromEnsureRow(mount), workspaceOperationKindCreatePty, row.ID, request, lease); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if idempotencyKey != "" && isUniqueViolation(err) {
			existingPty, getErr := s.db.GetWorkspacePtySessionByIdempotency(ctx, db.GetWorkspacePtySessionByIdempotencyParams{
				OrgID:          pgvalue.UUID(actor.OrgID),
				ProjectID:      ws.ProjectID,
				EnvironmentID:  ws.EnvironmentID,
				WorkspaceID:    ws.ID,
				IdempotencyKey: idempotencyKey,
			})
			if getErr == nil {
				if existingPty.RequestFingerprint != fingerprint {
					return db.WorkspaceProcess{}, false, errWorkspaceOperationIdempotencyUsed
				}
				return existingPty, true, nil
			}
		}
		return db.WorkspaceProcess{}, false, err
	}
	return row, existing, nil
}

func (s *Server) appendWorkspacePtyStreamChunk(ctx context.Context, pty db.WorkspaceProcess, stream string, offset int64, data []byte) (db.WorkspaceProcessStreamChunk, error) {
	if offset < 0 {
		return db.WorkspaceProcessStreamChunk{}, badRequest(errors.New("offset must be non-negative"))
	}
	if len(data) == 0 {
		return db.WorkspaceProcessStreamChunk{}, badRequest(errors.New("data is required"))
	}
	if len(data) > workspaceStreamChunkMaxBytes {
		return db.WorkspaceProcessStreamChunk{}, tooLarge(fmt.Errorf("stream chunk is %d bytes, exceeds max %d", len(data), workspaceStreamChunkMaxBytes))
	}
	var chunk db.WorkspaceProcessStreamChunk
	err := s.inTx(ctx, func(work *txWork) error {
		locked, err := work.q.LockWorkspacePtyForStreamAppend(ctx, db.LockWorkspacePtyForStreamAppendParams{
			OrgID:         pty.OrgID,
			ProjectID:     pty.ProjectID,
			EnvironmentID: pty.EnvironmentID,
			WorkspaceID:   pty.WorkspaceID,
			ProcessID:     pty.ID,
		})
		if err != nil {
			return err
		}
		if ptyStateTerminal(locked.State) {
			return conflict(errWorkspacePtyTerminal)
		}
		if stream == workspaceStreamInput && locked.State != db.WorkspaceProcessStateRunning {
			return conflict(errWorkspacePtyNotOpen)
		}
		want := ptyStreamCursor(locked, stream)
		if offset != want {
			existing, getErr := work.q.GetWorkspacePtyStreamChunkAtOffset(ctx, db.GetWorkspacePtyStreamChunkAtOffsetParams{
				OrgID:         pty.OrgID,
				ProjectID:     pty.ProjectID,
				EnvironmentID: pty.EnvironmentID,
				WorkspaceID:   pty.WorkspaceID,
				ProcessID:     pty.ID,
				StreamName:    stream,
				OffsetStart:   offset,
			})
			if getErr == nil && existing.OffsetEnd == offset+int64(len(data)) && bytes.Equal(existing.Data, data) {
				chunk = existing
				return nil
			}
			receipt, receiptErr := work.q.GetWorkspacePtyStreamChunkReceiptAtOffset(ctx, db.GetWorkspacePtyStreamChunkReceiptAtOffsetParams{
				OrgID:         pty.OrgID,
				ProjectID:     pty.ProjectID,
				EnvironmentID: pty.EnvironmentID,
				WorkspaceID:   pty.WorkspaceID,
				ProcessID:     pty.ID,
				StreamName:    stream,
				OffsetStart:   offset,
			})
			if receiptErr == nil && receipt.OffsetEnd == offset+int64(len(data)) && receipt.DataSize == int32(len(data)) && bytes.Equal(receipt.DataSha256, streamDataSHA256(data)) {
				chunk = ptyChunkFromReceipt(receipt, data)
				return nil
			}
			return conflict(errWorkspaceStreamOffsetConflict)
		}
		chunk, err = work.q.InsertWorkspacePtyStreamChunk(ctx, db.InsertWorkspacePtyStreamChunkParams{
			OrgID:         pty.OrgID,
			ProjectID:     pty.ProjectID,
			EnvironmentID: pty.EnvironmentID,
			WorkspaceID:   pty.WorkspaceID,
			ProcessID:     pty.ID,
			StreamName:    stream,
			OffsetStart:   offset,
			OffsetEnd:     offset + int64(len(data)),
			Data:          data,
			ObservedAt:    nil,
		})
		if err != nil {
			if isUniqueViolation(err) || isExclusionViolation(err) {
				return conflict(errWorkspaceStreamOffsetConflict)
			}
			return err
		}
		if _, err := work.q.AdvanceWorkspacePtyStreamCursor(ctx, db.AdvanceWorkspacePtyStreamCursorParams{
			StreamName:    stream,
			OffsetEnd:     chunk.OffsetEnd,
			OrgID:         pty.OrgID,
			ProjectID:     pty.ProjectID,
			EnvironmentID: pty.EnvironmentID,
			WorkspaceID:   pty.WorkspaceID,
			ProcessID:     pty.ID,
			OffsetStart:   offset,
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return db.WorkspaceProcessStreamChunk{}, err
	}
	return chunk, nil
}

func (s *Server) loadWorkspacePtyForRequest(w http.ResponseWriter, r *http.Request, permission auth.Permission) (db.WorkspaceProcess, bool) {
	workspace, ok := s.loadWorkspaceForRequest(w, r, permission)
	if !ok {
		return db.WorkspaceProcess{}, false
	}
	ptyID, err := parseRequiredWorkspaceUUID("pty_id", chi.URLParam(r, "ptyID"))
	if err != nil {
		writeError(w, badRequest(err))
		return db.WorkspaceProcess{}, false
	}
	row, err := s.db.GetWorkspacePtySession(r.Context(), db.GetWorkspacePtySessionParams{
		OrgID:         workspace.OrgID,
		ProjectID:     workspace.ProjectID,
		EnvironmentID: workspace.EnvironmentID,
		WorkspaceID:   workspace.ID,
		ID:            ptyID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("workspace pty not found")))
		return db.WorkspaceProcess{}, false
	}
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "get workspace pty", err)
		return db.WorkspaceProcess{}, false
	}
	return row, true
}

func workspacePtyResponse(row db.WorkspaceProcess) api.WorkspacePtyResponse {
	return api.WorkspacePtyResponse{
		ID:               pgvalue.MustUUIDValue(row.ID).String(),
		WorkspaceID:      pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		WorkspaceMountID: optionalUUIDString(row.WorkspaceMountID),
		Cwd:              row.Cwd,
		Cols:             row.PtyCols.Int32,
		Rows:             row.PtyRows.Int32,
		FilesystemMode:   string(row.FilesystemMode),
		State:            string(row.State),
		ProcessID:        row.RuntimeProcessID,
		OutputCursor:     row.OutputCursor,
		InputCursor:      row.InputCursor,
		Error:            json.RawMessage(row.TerminalError),
		CreatedAt:        pgvalue.Time(row.CreatedAt),
		StartedAt:        pgvalue.TimePtr(row.StartedAt),
		ClosedAt:         pgvalue.TimePtr(row.ExitedAt),
		UpdatedAt:        pgvalue.Time(row.UpdatedAt),
	}
}

func workspacePtyStreamChunkResponse(row db.WorkspaceProcessStreamChunk) api.WorkspacePtyStreamChunkResponse {
	return api.WorkspacePtyStreamChunkResponse{
		ID:          pgvalue.MustUUIDValue(row.ID).String(),
		Stream:      row.StreamName,
		OffsetStart: row.OffsetStart,
		OffsetEnd:   row.OffsetEnd,
		Data:        row.Data,
		ObservedAt:  pgvalue.Time(row.ObservedAt),
		CreatedAt:   pgvalue.Time(row.CreatedAt),
	}
}
