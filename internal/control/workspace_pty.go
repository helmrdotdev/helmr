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
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	workspacePtyCreateOperationKind = db.WorkspaceOperationIdempotencyKindWorkspacePtyCreate
	workspacePtyListDefaultLimit    = int32(50)
	workspacePtyListMaxLimit        = int32(200)
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
	cwd, err := workspace.NormalizeExecCwd(request.Cwd)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	cols, rows, err := workspace.NormalizePtySize(request.Cols, request.Rows)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	filesystemMode := db.WorkspaceFilesystemModeWrite
	fingerprint, err := workspace.PtyCreateFingerprint(cwd, cols, rows, filesystemMode)
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
	state := db.NullWorkspacePtyState{}
	if raw := strings.TrimSpace(r.URL.Query().Get("state")); raw != "" {
		normalized, err := normalizeWorkspacePtyStateFilter(raw)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
		state = db.NullWorkspacePtyState{WorkspacePtyState: normalized, Valid: true}
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

func normalizeWorkspacePtyStateFilter(raw string) (db.WorkspacePtyState, error) {
	switch db.WorkspacePtyState(strings.TrimSpace(raw)) {
	case db.WorkspacePtyStateCreating,
		db.WorkspacePtyStateOpen,
		db.WorkspacePtyStateResizing,
		db.WorkspacePtyStateClosing,
		db.WorkspacePtyStateClosed,
		db.WorkspacePtyStateLost,
		db.WorkspacePtyStateFailed:
		return db.WorkspacePtyState(strings.TrimSpace(raw)), nil
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
	chunk, err := s.appendWorkspacePtyStreamChunk(r.Context(), pty, db.WorkspacePtyStreamInput, request.Offset, request.Data)
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
	if err := s.ensureWorkspacePtyCursorAvailable(r.Context(), pty, db.WorkspacePtyStreamOutput, cursor); err != nil {
		writeError(w, err)
		return
	}
	rows, err := s.db.ListWorkspacePtyStreamChunksAfter(r.Context(), db.ListWorkspacePtyStreamChunksAfterParams{
		OrgID:         pty.OrgID,
		ProjectID:     pty.ProjectID,
		EnvironmentID: pty.EnvironmentID,
		WorkspaceID:   pty.WorkspaceID,
		PtySessionID:  pty.ID,
		Stream:        db.WorkspacePtyStreamOutput,
		CursorOffset:  cursor,
		LimitCount:    limit,
	})
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "list workspace pty output", err)
		return
	}
	out := make([]api.WorkspacePtyStreamChunkResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, workspacePtyStreamChunkResponse(row))
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
	cols, rows, err := workspace.NormalizePtySize(request.Cols, request.Rows)
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

func (s *Server) requestWorkspacePtyResize(ctx context.Context, pty db.WorkspacePtySession, cols int32, rows int32) (db.WorkspacePtySession, error) {
	if pty.State != db.WorkspacePtyStateOpen && pty.State != db.WorkspacePtyStateResizing {
		return db.WorkspacePtySession{}, conflict(errWorkspacePtyNotOpen)
	}
	if s.tx == nil {
		return db.WorkspacePtySession{}, errors.New("workspace pty resize requires transactional store")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return db.WorkspacePtySession{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	store := db.New(tx)
	row, err := store.ResizeWorkspacePtySession(ctx, db.ResizeWorkspacePtySessionParams{
		Cols:          pgtype.Int4{Int32: cols, Valid: true},
		Rows:          pgtype.Int4{Int32: rows, Valid: true},
		OrgID:         pty.OrgID,
		ProjectID:     pty.ProjectID,
		EnvironmentID: pty.EnvironmentID,
		WorkspaceID:   pty.WorkspaceID,
		ID:            pty.ID,
	})
	if err != nil {
		return db.WorkspacePtySession{}, err
	}
	request, err := workspace.PtyResizeOperationRequest(row)
	if err != nil {
		return db.WorkspacePtySession{}, err
	}
	if err := requestWorkspacePtyControlOperation(ctx, store, row, workspaceOperationKindResizePty, request); err != nil {
		return db.WorkspacePtySession{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.WorkspacePtySession{}, err
	}
	return row, nil
}

func (s *Server) requestWorkspacePtyCloseOperation(ctx context.Context, pty db.WorkspacePtySession) (db.WorkspacePtySession, error) {
	if pty.State != db.WorkspacePtyStateOpen && pty.State != db.WorkspacePtyStateResizing && pty.State != db.WorkspacePtyStateClosing {
		return db.WorkspacePtySession{}, conflict(errWorkspacePtyNotOpen)
	}
	if s.tx == nil {
		return db.WorkspacePtySession{}, errors.New("workspace pty close requires transactional store")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return db.WorkspacePtySession{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	store := db.New(tx)
	row, err := store.RequestWorkspacePtyClose(ctx, db.RequestWorkspacePtyCloseParams{
		OrgID:         pty.OrgID,
		ProjectID:     pty.ProjectID,
		EnvironmentID: pty.EnvironmentID,
		WorkspaceID:   pty.WorkspaceID,
		ID:            pty.ID,
	})
	if err != nil {
		return db.WorkspacePtySession{}, err
	}
	request, err := workspace.PtyCloseOperationRequest(row)
	if err != nil {
		return db.WorkspacePtySession{}, err
	}
	if err := requestWorkspacePtyControlOperation(ctx, store, row, workspaceOperationKindClosePty, request); err != nil {
		return db.WorkspacePtySession{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.WorkspacePtySession{}, err
	}
	return row, nil
}

func (s *Server) createWorkspacePtyForRequest(ctx context.Context, actor auth.Actor, ws db.Workspace, cwd string, cols int32, rows int32, filesystemMode db.WorkspaceFilesystemMode, idempotencyKey string, fingerprint string) (db.WorkspacePtySession, bool, error) {
	if s.tx == nil {
		return db.WorkspacePtySession{}, false, errors.New("transactional workspace storage is not configured")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return db.WorkspacePtySession{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	store := db.New(tx)
	if idempotencyKey != "" {
		idempotency, err := ensureWorkspaceOperationIdempotency(ctx, store, db.EnsureWorkspaceOperationIdempotencyParams{
			ID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:                pgvalue.UUID(actor.OrgID),
			ProjectID:            ws.ProjectID,
			EnvironmentID:        ws.EnvironmentID,
			WorkspaceID:          ws.ID,
			OperationKind:        workspacePtyCreateOperationKind,
			IdempotencyKey:       idempotencyKey,
			RequestFingerprint:   fingerprint,
			ResponseResourceType: "",
			ResponseResourceID:   pgtype.UUID{},
			ResponseBody:         []byte(`{}`),
			ExpiresAt:            pgvalue.Timestamptz(time.Now().Add(workspaceExecIdempotencyTTL)),
		})
		if err != nil {
			return db.WorkspacePtySession{}, false, err
		}
		if !idempotency.Inserted {
			if idempotency.RequestFingerprint != fingerprint {
				return db.WorkspacePtySession{}, false, errWorkspaceOperationIdempotencyUsed
			}
			if !idempotency.ResponseResourceID.Valid {
				return db.WorkspacePtySession{}, false, errWorkspaceOperationPending
			}
			row, getPtyErr := s.db.GetWorkspacePtySession(ctx, db.GetWorkspacePtySessionParams{
				OrgID:         pgvalue.UUID(actor.OrgID),
				ProjectID:     ws.ProjectID,
				EnvironmentID: ws.EnvironmentID,
				WorkspaceID:   ws.ID,
				ID:            idempotency.ResponseResourceID,
			})
			return row, true, getPtyErr
		}
	}
	if err := ensureWorkspacePrimitiveWriterAvailable(ctx, store, pgvalue.UUID(actor.OrgID), ws.ProjectID, ws.EnvironmentID, ws.ID); err != nil {
		return db.WorkspacePtySession{}, false, err
	}
	row, err := store.CreateWorkspacePtySession(ctx, db.CreateWorkspacePtySessionParams{
		ID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Cwd:                  cwd,
		Cols:                 cols,
		Rows:                 rows,
		FilesystemMode:       filesystemMode,
		State:                db.WorkspacePtyStateCreating,
		CreatedBySubjectType: string(actor.Kind),
		CreatedBySubjectID:   actorSubjectID(actor),
		OrgID:                pgvalue.UUID(actor.OrgID),
		ProjectID:            ws.ProjectID,
		EnvironmentID:        ws.EnvironmentID,
		WorkspaceID:          ws.ID,
	})
	if err != nil {
		return db.WorkspacePtySession{}, false, err
	}
	materialization, err := store.EnsureWorkspaceMaterializationRequested(ctx, db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     ws.ProjectID,
		EnvironmentID: ws.EnvironmentID,
		WorkspaceID:   ws.ID,
		Priority:      0,
		Request:       []byte(`{"reason":"workspace_pty"}`),
	})
	if err != nil {
		return db.WorkspacePtySession{}, false, err
	}
	row, err = store.BindWorkspacePtyMaterialization(ctx, db.BindWorkspacePtyMaterializationParams{
		MaterializationID: materialization.ID,
		InstanceLeaseID:   pgtype.UUID{},
		WriteLeaseID:      pgtype.UUID{},
		State:             db.WorkspacePtyStateCreating,
		OrgID:             pgvalue.UUID(actor.OrgID),
		ProjectID:         ws.ProjectID,
		EnvironmentID:     ws.EnvironmentID,
		WorkspaceID:       ws.ID,
		ID:                row.ID,
	})
	if err != nil {
		return db.WorkspacePtySession{}, false, err
	}
	if materialization.State == db.WorkspaceMaterializationStateRunning {
		var lease workspacePrimitiveOperationLease
		row, lease, err = ensureWorkspacePtyWriteLease(ctx, store, materializationFromEnsureRow(materialization), row)
		if err != nil {
			return db.WorkspacePtySession{}, false, err
		}
		request, err := workspace.PtyCreateOperationRequest(row)
		if err != nil {
			return db.WorkspacePtySession{}, false, err
		}
		if _, err := requestWorkspacePrimitiveOperation(ctx, store, materializationFromEnsureRow(materialization), workspaceOperationKindCreatePty, workspaceOperationResourcePty, row.ID, request, lease, 0); err != nil {
			return db.WorkspacePtySession{}, false, err
		}
	}
	if idempotencyKey != "" {
		_, err = store.CompleteWorkspaceScopedOperationIdempotency(ctx, db.CompleteWorkspaceScopedOperationIdempotencyParams{
			OrgID:                pgvalue.UUID(actor.OrgID),
			ProjectID:            ws.ProjectID,
			EnvironmentID:        ws.EnvironmentID,
			OperationKind:        workspacePtyCreateOperationKind,
			WorkspaceID:          ws.ID,
			IdempotencyKey:       idempotencyKey,
			RequestFingerprint:   fingerprint,
			ResponseResourceType: "workspace_pty",
			ResponseResourceID:   row.ID,
			ResponseBody:         []byte(`{}`),
		})
		if err != nil {
			return db.WorkspacePtySession{}, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return db.WorkspacePtySession{}, false, err
	}
	return row, false, nil
}

func (s *Server) appendWorkspacePtyStreamChunk(ctx context.Context, pty db.WorkspacePtySession, stream db.WorkspacePtyStream, offset int64, data []byte) (db.WorkspacePtyStreamChunk, error) {
	if s.tx == nil {
		return db.WorkspacePtyStreamChunk{}, errors.New("transactional workspace storage is not configured")
	}
	if offset < 0 {
		return db.WorkspacePtyStreamChunk{}, badRequest(errors.New("offset must be non-negative"))
	}
	if len(data) == 0 {
		return db.WorkspacePtyStreamChunk{}, badRequest(errors.New("data is required"))
	}
	if len(data) > workspaceStreamChunkMaxBytes {
		return db.WorkspacePtyStreamChunk{}, tooLarge(fmt.Errorf("stream chunk is %d bytes, exceeds max %d", len(data), workspaceStreamChunkMaxBytes))
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return db.WorkspacePtyStreamChunk{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	store := db.New(tx)
	locked, err := store.LockWorkspacePtyForStreamAppend(ctx, db.LockWorkspacePtyForStreamAppendParams{
		OrgID:         pty.OrgID,
		ProjectID:     pty.ProjectID,
		EnvironmentID: pty.EnvironmentID,
		WorkspaceID:   pty.WorkspaceID,
		PtySessionID:  pty.ID,
	})
	if err != nil {
		return db.WorkspacePtyStreamChunk{}, err
	}
	if workspace.PtyStateTerminal(locked.State) {
		return db.WorkspacePtyStreamChunk{}, conflict(errWorkspacePtyTerminal)
	}
	if stream == db.WorkspacePtyStreamInput && locked.State != db.WorkspacePtyStateOpen && locked.State != db.WorkspacePtyStateResizing {
		return db.WorkspacePtyStreamChunk{}, conflict(errWorkspacePtyNotOpen)
	}
	want := workspace.PtyStreamCursor(locked, stream)
	if offset != want {
		existing, getErr := store.GetWorkspacePtyStreamChunkAtOffset(ctx, db.GetWorkspacePtyStreamChunkAtOffsetParams{
			OrgID:         pty.OrgID,
			ProjectID:     pty.ProjectID,
			EnvironmentID: pty.EnvironmentID,
			WorkspaceID:   pty.WorkspaceID,
			PtySessionID:  pty.ID,
			Stream:        stream,
			OffsetStart:   offset,
		})
		if getErr == nil && existing.OffsetEnd == offset+int64(len(data)) && bytes.Equal(existing.Data, data) {
			return existing, nil
		}
		receipt, receiptErr := store.GetWorkspacePtyStreamChunkReceiptAtOffset(ctx, db.GetWorkspacePtyStreamChunkReceiptAtOffsetParams{
			OrgID:         pty.OrgID,
			ProjectID:     pty.ProjectID,
			EnvironmentID: pty.EnvironmentID,
			WorkspaceID:   pty.WorkspaceID,
			PtySessionID:  pty.ID,
			Stream:        stream,
			OffsetStart:   offset,
		})
		if receiptErr == nil && receipt.OffsetEnd == offset+int64(len(data)) && receipt.DataSize == int32(len(data)) && bytes.Equal(receipt.DataSha256, workspace.StreamDataSHA256(data)) {
			return workspace.PtyChunkFromReceipt(receipt, data), nil
		}
		return db.WorkspacePtyStreamChunk{}, conflict(errWorkspaceStreamOffsetConflict)
	}
	chunk, err := store.InsertWorkspacePtyStreamChunk(ctx, db.InsertWorkspacePtyStreamChunkParams{
		OrgID:         pty.OrgID,
		ProjectID:     pty.ProjectID,
		EnvironmentID: pty.EnvironmentID,
		WorkspaceID:   pty.WorkspaceID,
		PtySessionID:  pty.ID,
		Stream:        stream,
		OffsetStart:   offset,
		OffsetEnd:     offset + int64(len(data)),
		Data:          data,
		ObservedAt:    nil,
	})
	if err != nil {
		if isUniqueViolation(err) || isExclusionViolation(err) {
			return db.WorkspacePtyStreamChunk{}, conflict(errWorkspaceStreamOffsetConflict)
		}
		return db.WorkspacePtyStreamChunk{}, err
	}
	if _, err := store.AdvanceWorkspacePtyStreamCursor(ctx, db.AdvanceWorkspacePtyStreamCursorParams{
		Stream:        stream,
		OffsetEnd:     chunk.OffsetEnd,
		OrgID:         pty.OrgID,
		ProjectID:     pty.ProjectID,
		EnvironmentID: pty.EnvironmentID,
		WorkspaceID:   pty.WorkspaceID,
		PtySessionID:  pty.ID,
		OffsetStart:   offset,
	}); err != nil {
		return db.WorkspacePtyStreamChunk{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.WorkspacePtyStreamChunk{}, err
	}
	return chunk, nil
}

func (s *Server) ensureWorkspacePtyCursorAvailable(ctx context.Context, pty db.WorkspacePtySession, stream db.WorkspacePtyStream, cursor int64) error {
	bounds, err := s.db.GetWorkspacePtyStreamBounds(ctx, db.GetWorkspacePtyStreamBoundsParams{
		OrgID:         pty.OrgID,
		ProjectID:     pty.ProjectID,
		EnvironmentID: pty.EnvironmentID,
		WorkspaceID:   pty.WorkspaceID,
		PtySessionID:  pty.ID,
		Stream:        stream,
	})
	if err != nil {
		return err
	}
	if cursor < bounds.EarliestOffset && bounds.EarliestOffset > 0 {
		return gone(codedError{code: errWorkspaceStreamCursorExpired.code, message: fmt.Sprintf("workspace stream cursor expired; earliest available cursor is %d", bounds.EarliestOffset)})
	}
	return nil
}

func (s *Server) loadWorkspacePtyForRequest(w http.ResponseWriter, r *http.Request, permission auth.Permission) (db.WorkspacePtySession, bool) {
	workspace, ok := s.loadWorkspaceForRequest(w, r, permission)
	if !ok {
		return db.WorkspacePtySession{}, false
	}
	ptyID, err := parseRequiredWorkspaceUUID("pty_id", chi.URLParam(r, "ptyID"))
	if err != nil {
		writeError(w, badRequest(err))
		return db.WorkspacePtySession{}, false
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
		return db.WorkspacePtySession{}, false
	}
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "get workspace pty", err)
		return db.WorkspacePtySession{}, false
	}
	return row, true
}

func workspacePtyResponse(row db.WorkspacePtySession) api.WorkspacePtyResponse {
	return api.WorkspacePtyResponse{
		ID:                pgvalue.MustUUIDValue(row.ID).String(),
		WorkspaceID:       pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		MaterializationID: optionalUUIDString(row.MaterializationID),
		Cwd:               row.Cwd,
		Cols:              row.Cols,
		Rows:              row.Rows,
		FilesystemMode:    string(row.FilesystemMode),
		State:             string(row.State),
		ProcessID:         row.ProcessID,
		OutputCursor:      row.OutputCursor,
		InputCursor:       row.InputCursor,
		Error:             json.RawMessage(row.Error),
		CreatedAt:         pgvalue.Time(row.CreatedAt),
		StartedAt:         pgvalue.TimePtr(row.StartedAt),
		ClosedAt:          pgvalue.TimePtr(row.ClosedAt),
		UpdatedAt:         pgvalue.Time(row.UpdatedAt),
	}
}

func workspacePtyFromClosedRow(row db.MarkWorkspacePtyClosedRow) db.WorkspacePtySession {
	return db.WorkspacePtySession{
		ID:                   row.ID,
		OrgID:                row.OrgID,
		ProjectID:            row.ProjectID,
		EnvironmentID:        row.EnvironmentID,
		WorkspaceID:          row.WorkspaceID,
		MaterializationID:    row.MaterializationID,
		InstanceLeaseID:      row.InstanceLeaseID,
		WriteLeaseID:         row.WriteLeaseID,
		Cwd:                  row.Cwd,
		Cols:                 row.Cols,
		Rows:                 row.Rows,
		ResizeCols:           row.ResizeCols,
		ResizeRows:           row.ResizeRows,
		FilesystemMode:       row.FilesystemMode,
		State:                row.State,
		ProcessID:            row.ProcessID,
		OutputCursor:         row.OutputCursor,
		InputCursor:          row.InputCursor,
		InputDeliveredCursor: row.InputDeliveredCursor,
		CreatedBySubjectType: row.CreatedBySubjectType,
		CreatedBySubjectID:   row.CreatedBySubjectID,
		CreatedAt:            row.CreatedAt,
		StartedAt:            row.StartedAt,
		ClosedAt:             row.ClosedAt,
		UpdatedAt:            row.UpdatedAt,
		Error:                row.Error,
	}
}

func workspacePtyFromFailedRow(row db.MarkWorkspacePtyFailedRow) db.WorkspacePtySession {
	return db.WorkspacePtySession{
		ID:                   row.ID,
		OrgID:                row.OrgID,
		ProjectID:            row.ProjectID,
		EnvironmentID:        row.EnvironmentID,
		WorkspaceID:          row.WorkspaceID,
		MaterializationID:    row.MaterializationID,
		InstanceLeaseID:      row.InstanceLeaseID,
		WriteLeaseID:         row.WriteLeaseID,
		Cwd:                  row.Cwd,
		Cols:                 row.Cols,
		Rows:                 row.Rows,
		ResizeCols:           row.ResizeCols,
		ResizeRows:           row.ResizeRows,
		FilesystemMode:       row.FilesystemMode,
		State:                row.State,
		ProcessID:            row.ProcessID,
		OutputCursor:         row.OutputCursor,
		InputCursor:          row.InputCursor,
		InputDeliveredCursor: row.InputDeliveredCursor,
		CreatedBySubjectType: row.CreatedBySubjectType,
		CreatedBySubjectID:   row.CreatedBySubjectID,
		CreatedAt:            row.CreatedAt,
		StartedAt:            row.StartedAt,
		ClosedAt:             row.ClosedAt,
		UpdatedAt:            row.UpdatedAt,
		Error:                row.Error,
	}
}

func workspacePtyStreamChunkResponse(row db.WorkspacePtyStreamChunk) api.WorkspacePtyStreamChunkResponse {
	return api.WorkspacePtyStreamChunkResponse{
		ID:          pgvalue.MustUUIDValue(row.ID).String(),
		Stream:      string(row.Stream),
		OffsetStart: row.OffsetStart,
		OffsetEnd:   row.OffsetEnd,
		Data:        row.Data,
		ObservedAt:  pgvalue.Time(row.ObservedAt),
		CreatedAt:   pgvalue.Time(row.CreatedAt),
	}
}
