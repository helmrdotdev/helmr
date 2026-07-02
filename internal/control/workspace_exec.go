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
	workspaceExecCreateOperationKind = db.WorkspaceOperationIdempotencyKindWorkspaceExecCreate
	workspaceExecIdempotencyTTL      = 24 * time.Hour
	workspaceExecListDefaultLimit    = int32(50)
	workspaceExecListMaxLimit        = int32(200)
	workspaceStreamChunkMaxBytes     = 1024 * 1024
	workspaceStreamRetainedMaxBytes  = 64 * 1024 * 1024
)

var (
	errWorkspaceStreamOffsetConflict     = codedError{code: "workspace_stream_offset_conflict"}
	errWorkspaceStreamCursorExpired      = codedError{code: "workspace_stream_cursor_expired"}
	errWorkspaceReadOnlyUnsupported      = codedError{code: "workspace_read_only_unsupported"}
	errWorkspaceExecTerminal             = codedError{code: "workspace_exec_terminal"}
	errWorkspaceExecStdinClosed          = codedError{code: "workspace_exec_stdin_closed"}
	errWorkspaceWriterActive             = codedError{code: "workspace_writer_active", message: "workspace already has an active write primitive"}
	errWorkspaceNotActive                = codedError{code: "workspace_not_active", message: "workspace is not active"}
	errWorkspaceLifecycleEventConflict   = codedError{code: "workspace_lifecycle_event_conflict"}
	errWorkspaceOperationIdempotencyUsed = codedError{code: "idempotency_fingerprint_mismatch", message: "idempotency_key was already used with different parameters"}
)

func (s *Server) createWorkspaceExec(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.loadWorkspaceForRequest(w, r, auth.PermissionExecCreate)
	if !ok {
		return
	}
	actor := actorFromContext(r.Context())
	var request api.WorkspaceExecCreateRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace exec request JSON: %w", err)))
		return
	}
	command, err := normalizeExecCommand(request.Command)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	cwd, err := normalizeExecCwd(request.Cwd)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	envShape, err := execEnvShape(request.Env)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	filesystemMode := db.WorkspaceFilesystemModeWrite
	fingerprint, err := execCreateFingerprint(command, cwd, envShape, request.Detached, filesystemMode)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	row, cached, err := s.createWorkspaceExecForRequest(r.Context(), actor, ws, command, cwd, envShape, request.Detached, filesystemMode, strings.TrimSpace(request.IdempotencyKey), fingerprint)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "create workspace exec", err)
		return
	}
	status := http.StatusCreated
	if cached {
		status = http.StatusOK
	}
	writeJSON(w, status, api.WorkspaceExecEnvelope{Exec: workspaceExecResponse(row), IsCached: cached})
}

func (s *Server) listWorkspaceExecs(w http.ResponseWriter, r *http.Request) {
	workspace, ok := s.loadWorkspaceForRequest(w, r, auth.PermissionExecRead)
	if !ok {
		return
	}
	limit, err := parseWorkspacePrimitiveLimit(r, workspaceExecListDefaultLimit, workspaceExecListMaxLimit)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	state := pgtype.Text{}
	if raw := strings.TrimSpace(r.URL.Query().Get("state")); raw != "" {
		normalized, err := normalizeWorkspaceExecStateFilter(raw)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
		state = pgvalue.Text(string(normalized))
	}
	rows, err := s.db.ListWorkspaceExecs(r.Context(), db.ListWorkspaceExecsParams{
		OrgID:         workspace.OrgID,
		ProjectID:     workspace.ProjectID,
		EnvironmentID: workspace.EnvironmentID,
		WorkspaceID:   workspace.ID,
		State:         db.NullWorkspaceExecState{WorkspaceExecState: db.WorkspaceExecState(state.String), Valid: state.Valid},
		LimitCount:    limit,
	})
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "list workspace execs", err)
		return
	}
	out := make([]api.WorkspaceExecResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, workspaceExecResponse(row))
	}
	writeJSON(w, http.StatusOK, api.ListWorkspaceExecsResponse{Execs: out})
}

func normalizeWorkspaceExecStateFilter(raw string) (db.WorkspaceExecState, error) {
	switch db.WorkspaceExecState(strings.TrimSpace(raw)) {
	case db.WorkspaceExecStateQueued,
		db.WorkspaceExecStateMaterializing,
		db.WorkspaceExecStateRunning,
		db.WorkspaceExecStateExited,
		db.WorkspaceExecStateTerminated,
		db.WorkspaceExecStateLost,
		db.WorkspaceExecStateFailed:
		return db.WorkspaceExecState(strings.TrimSpace(raw)), nil
	default:
		return "", errors.New("state must be one of queued, materializing, running, exited, terminated, lost, failed")
	}
}

func (s *Server) getWorkspaceExec(w http.ResponseWriter, r *http.Request) {
	exec, ok := s.loadWorkspaceExecForRequest(w, r, auth.PermissionExecRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspaceExecEnvelope{Exec: workspaceExecResponse(exec)})
}

func (s *Server) closeWorkspaceExecStdin(w http.ResponseWriter, r *http.Request) {
	exec, ok := s.loadWorkspaceExecForRequest(w, r, auth.PermissionExecManage)
	if !ok {
		return
	}
	row, err := s.requestWorkspaceExecStdinClose(r.Context(), exec)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "close workspace exec stdin", err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkspaceExecEnvelope{Exec: workspaceExecResponse(row)})
}

func (s *Server) writeWorkspaceExecStdin(w http.ResponseWriter, r *http.Request) {
	exec, ok := s.loadWorkspaceExecForRequest(w, r, auth.PermissionExecManage)
	if !ok {
		return
	}
	var request api.WorkspaceExecStdinWriteRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace exec stdin request JSON: %w", err)))
		return
	}
	chunk, err := s.appendWorkspaceExecStreamChunk(r.Context(), exec, db.WorkspaceExecStreamStdin, request.Offset, request.Data)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "write workspace exec stdin", err)
		return
	}
	writeJSON(w, http.StatusOK, workspaceExecStreamChunkResponse(chunk))
}

func (s *Server) listWorkspaceExecStdout(w http.ResponseWriter, r *http.Request) {
	s.listWorkspaceExecStream(w, r, db.WorkspaceExecStreamStdout)
}

func (s *Server) listWorkspaceExecStderr(w http.ResponseWriter, r *http.Request) {
	s.listWorkspaceExecStream(w, r, db.WorkspaceExecStreamStderr)
}

func (s *Server) listWorkspaceExecStream(w http.ResponseWriter, r *http.Request, stream db.WorkspaceExecStream) {
	exec, ok := s.loadWorkspaceExecForRequest(w, r, auth.PermissionExecRead)
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
		s.followWorkspaceExecStream(w, r, exec, stream, cursor, limit)
		return
	}
	limit, err := parseWorkspacePrimitiveLimit(r, 100, 500)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if stream == db.WorkspaceExecStreamStdout || stream == db.WorkspaceExecStreamStderr {
		out, _, err := s.listWorkspaceExecTerminalOutput(r.Context(), exec, stream, cursor, limit)
		if err != nil {
			s.writeWorkspacePrimitiveError(w, "list workspace exec stream", err)
			return
		}
		writeJSON(w, http.StatusOK, api.ListWorkspaceExecStreamChunksResponse{Chunks: out})
		return
	}
	if err := s.ensureWorkspaceExecCursorAvailable(r.Context(), exec, stream, cursor); err != nil {
		writeError(w, err)
		return
	}
	rows, err := s.db.ListWorkspaceExecStreamChunksAfter(r.Context(), db.ListWorkspaceExecStreamChunksAfterParams{
		OrgID:         exec.OrgID,
		ProjectID:     exec.ProjectID,
		EnvironmentID: exec.EnvironmentID,
		WorkspaceID:   exec.WorkspaceID,
		ExecID:        exec.ID,
		Stream:        stream,
		CursorOffset:  cursor,
		LimitCount:    limit,
	})
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "list workspace exec stream", err)
		return
	}
	out := make([]api.WorkspaceExecStreamChunkResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, workspaceExecStreamChunkResponse(row))
	}
	writeJSON(w, http.StatusOK, api.ListWorkspaceExecStreamChunksResponse{Chunks: out})
}

func (s *Server) requestWorkspaceExecStdinClose(ctx context.Context, exec db.WorkspaceExec) (db.WorkspaceExec, error) {
	if execStateTerminal(exec.State) {
		return db.WorkspaceExec{}, conflict(errWorkspaceExecTerminal)
	}
	var row db.WorkspaceExec
	err := s.inTx(ctx, func(work *txWork) error {
		var err error
		row, err = work.q.CloseWorkspaceExecStdin(ctx, db.CloseWorkspaceExecStdinParams{
			OrgID:         exec.OrgID,
			ProjectID:     exec.ProjectID,
			EnvironmentID: exec.EnvironmentID,
			WorkspaceID:   exec.WorkspaceID,
			ID:            exec.ID,
		})
		if err != nil {
			if isNoRows(err) {
				current, getErr := work.q.GetWorkspaceExec(ctx, db.GetWorkspaceExecParams{
					OrgID:         exec.OrgID,
					ProjectID:     exec.ProjectID,
					EnvironmentID: exec.EnvironmentID,
					WorkspaceID:   exec.WorkspaceID,
					ID:            exec.ID,
				})
				if getErr == nil && execStateTerminal(current.State) {
					return conflict(errWorkspaceExecTerminal)
				}
			}
			return err
		}
		return nil
	})
	if err != nil {
		return db.WorkspaceExec{}, err
	}
	return row, nil
}

func (s *Server) createWorkspaceExecForRequest(ctx context.Context, actor auth.Actor, ws db.Workspace, command []string, cwd string, envShape []byte, detached bool, filesystemMode db.WorkspaceFilesystemMode, idempotencyKey string, fingerprint string) (db.WorkspaceExec, bool, error) {
	var row db.WorkspaceExec
	var existing bool
	err := s.inTx(ctx, func(work *txWork) error {
		if idempotencyKey != "" {
			idempotency, err := ensureWorkspaceOperationIdempotency(ctx, work.q, db.EnsureWorkspaceOperationIdempotencyParams{
				ID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
				OrgID:                pgvalue.UUID(actor.OrgID),
				ProjectID:            ws.ProjectID,
				EnvironmentID:        ws.EnvironmentID,
				WorkspaceID:          ws.ID,
				OperationKind:        workspaceExecCreateOperationKind,
				IdempotencyKey:       idempotencyKey,
				RequestFingerprint:   fingerprint,
				ResponseResourceType: "",
				ResponseResourceID:   pgtype.UUID{},
				ResponseBody:         []byte(`{}`),
				ExpiresAt:            pgvalue.Timestamptz(time.Now().Add(workspaceExecIdempotencyTTL)),
			})
			if err != nil {
				return err
			}
			if !idempotency.Inserted {
				if idempotency.RequestFingerprint != fingerprint {
					return errWorkspaceOperationIdempotencyUsed
				}
				if !idempotency.ResponseResourceID.Valid {
					return errWorkspaceOperationPending
				}
				row, err = work.q.GetWorkspaceExec(ctx, db.GetWorkspaceExecParams{
					OrgID:         pgvalue.UUID(actor.OrgID),
					ProjectID:     ws.ProjectID,
					EnvironmentID: ws.EnvironmentID,
					WorkspaceID:   ws.ID,
					ID:            idempotency.ResponseResourceID,
				})
				existing = true
				return err
			}
		}
		if err := ensureWorkspacePrimitiveWriterAvailable(ctx, work.q, pgvalue.UUID(actor.OrgID), ws.ProjectID, ws.EnvironmentID, ws.ID); err != nil {
			return err
		}
		commandJSON, err := json.Marshal(command)
		if err != nil {
			return err
		}
		row, err = work.q.CreateWorkspaceExec(ctx, db.CreateWorkspaceExecParams{
			ID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
			Command:              commandJSON,
			Cwd:                  cwd,
			EnvShape:             envShape,
			FilesystemMode:       filesystemMode,
			State:                db.WorkspaceExecStateMaterializing,
			Detached:             detached,
			IdempotencyKey:       idempotencyKey,
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
			ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:           pgvalue.UUID(actor.OrgID),
			ProjectID:       ws.ProjectID,
			EnvironmentID:   ws.EnvironmentID,
			WorkspaceID:     ws.ID,
			RequestPriority: 0,
			Request:         []byte(`{"reason":"workspace_exec"}`),
		})
		if err != nil {
			return err
		}
		nextState := db.WorkspaceExecStateMaterializing
		if mount.State == db.WorkspaceMountStateMounted {
			nextState = db.WorkspaceExecStateQueued
		}
		row, err = work.q.BindWorkspaceExecWorkspaceMount(ctx, db.BindWorkspaceExecWorkspaceMountParams{
			WorkspaceMountID: mount.ID,
			InstanceLeaseID:  pgtype.UUID{},
			WriteLeaseID:     pgtype.UUID{},
			State:            nextState,
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
			row, lease, err = ensureWorkspaceExecWriteLease(ctx, work.q, workspaceMountFromEnsureRow(mount), row)
			if err != nil {
				return err
			}
			request, err := execStartOperationRequest(row)
			if err != nil {
				return err
			}
			if err := requestWorkspacePrimitiveOperation(ctx, work.q, workspaceMountFromEnsureRow(mount), workspaceOperationKindStartExec, workspaceOperationResourceExec, row.ID, request, lease); err != nil {
				return err
			}
		}
		if idempotencyKey != "" {
			_, err = work.q.CompleteWorkspaceScopedOperationIdempotency(ctx, db.CompleteWorkspaceScopedOperationIdempotencyParams{
				OrgID:                pgvalue.UUID(actor.OrgID),
				ProjectID:            ws.ProjectID,
				EnvironmentID:        ws.EnvironmentID,
				OperationKind:        workspaceExecCreateOperationKind,
				WorkspaceID:          ws.ID,
				IdempotencyKey:       idempotencyKey,
				RequestFingerprint:   fingerprint,
				ResponseResourceType: "workspace_exec",
				ResponseResourceID:   row.ID,
				ResponseBody:         []byte(`{}`),
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return db.WorkspaceExec{}, false, err
	}
	return row, existing, nil
}

func (s *Server) appendWorkspaceExecStreamChunk(ctx context.Context, exec db.WorkspaceExec, stream db.WorkspaceExecStream, offset int64, data []byte) (db.WorkspaceExecStreamChunk, error) {
	if offset < 0 {
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
		if execStateTerminal(locked.State) {
			return conflict(errWorkspaceExecTerminal)
		}
		if stream == db.WorkspaceExecStreamStdin && locked.StdinClosedAt.Valid {
			return conflict(errWorkspaceExecStdinClosed)
		}
		want := execStreamCursor(locked, stream)
		if offset != want {
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
			receipt, receiptErr := work.q.GetWorkspaceExecStreamChunkReceiptAtOffset(ctx, db.GetWorkspaceExecStreamChunkReceiptAtOffsetParams{
				OrgID:         exec.OrgID,
				ProjectID:     exec.ProjectID,
				EnvironmentID: exec.EnvironmentID,
				WorkspaceID:   exec.WorkspaceID,
				ExecID:        exec.ID,
				Stream:        stream,
				OffsetStart:   offset,
			})
			if receiptErr == nil && receipt.OffsetEnd == offset+int64(len(data)) && receipt.DataSize == int32(len(data)) && bytes.Equal(receipt.DataSha256, streamDataSHA256(data)) {
				chunk = execChunkFromReceipt(receipt, data)
				return nil
			}
			return conflict(errWorkspaceStreamOffsetConflict)
		}
		chunk, err = work.q.InsertWorkspaceExecStreamChunk(ctx, db.InsertWorkspaceExecStreamChunkParams{
			OrgID:         exec.OrgID,
			CellID:        exec.CellID,
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
		if err != nil {
			if isUniqueViolation(err) || isExclusionViolation(err) {
				return conflict(errWorkspaceStreamOffsetConflict)
			}
			return err
		}
		if _, err := work.q.AdvanceWorkspaceExecStreamCursor(ctx, db.AdvanceWorkspaceExecStreamCursorParams{
			Stream:        stream,
			OffsetEnd:     chunk.OffsetEnd,
			OrgID:         exec.OrgID,
			ProjectID:     exec.ProjectID,
			EnvironmentID: exec.EnvironmentID,
			WorkspaceID:   exec.WorkspaceID,
			ExecID:        exec.ID,
			OffsetStart:   offset,
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

func (s *Server) ensureWorkspaceExecCursorAvailable(ctx context.Context, exec db.WorkspaceExec, stream db.WorkspaceExecStream, cursor int64) error {
	bounds, err := s.db.GetWorkspaceExecStreamBounds(ctx, db.GetWorkspaceExecStreamBoundsParams{
		OrgID:         exec.OrgID,
		ProjectID:     exec.ProjectID,
		EnvironmentID: exec.EnvironmentID,
		WorkspaceID:   exec.WorkspaceID,
		ExecID:        exec.ID,
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

func (s *Server) loadWorkspaceExecForRequest(w http.ResponseWriter, r *http.Request, permission auth.Permission) (db.WorkspaceExec, bool) {
	workspace, ok := s.loadWorkspaceForRequest(w, r, permission)
	if !ok {
		return db.WorkspaceExec{}, false
	}
	execID, err := parseRequiredWorkspaceUUID("exec_id", chi.URLParam(r, "execID"))
	if err != nil {
		writeError(w, badRequest(err))
		return db.WorkspaceExec{}, false
	}
	row, err := s.db.GetWorkspaceExec(r.Context(), db.GetWorkspaceExecParams{
		OrgID:         workspace.OrgID,
		ProjectID:     workspace.ProjectID,
		EnvironmentID: workspace.EnvironmentID,
		WorkspaceID:   workspace.ID,
		ID:            execID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("workspace exec not found")))
		return db.WorkspaceExec{}, false
	}
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "get workspace exec", err)
		return db.WorkspaceExec{}, false
	}
	return row, true
}

func workspaceExecResponse(row db.WorkspaceExec) api.WorkspaceExecResponse {
	return api.WorkspaceExecResponse{
		ID:               pgvalue.MustUUIDValue(row.ID).String(),
		WorkspaceID:      pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		WorkspaceMountID: optionalUUIDString(row.WorkspaceMountID),
		Command:          json.RawMessage(row.Command),
		Cwd:              row.Cwd,
		EnvShape:         json.RawMessage(row.EnvShape),
		FilesystemMode:   string(row.FilesystemMode),
		State:            string(row.State),
		Detached:         row.Detached,
		ProcessID:        row.ProcessID,
		ExitCode:         pgvalue.Int4Response(row.ExitCode),
		Signal:           row.Signal,
		Error:            json.RawMessage(row.Error),
		StdoutCursor:     row.StdoutCursor,
		StderrCursor:     row.StderrCursor,
		StdinCursor:      row.StdinCursor,
		StdinClosedAt:    pgvalue.TimePtr(row.StdinClosedAt),
		CreatedAt:        pgvalue.Time(row.CreatedAt),
		StartedAt:        pgvalue.TimePtr(row.StartedAt),
		ExitedAt:         pgvalue.TimePtr(row.ExitedAt),
		UpdatedAt:        pgvalue.Time(row.UpdatedAt),
	}
}

func workspaceExecFromExitedRow(row db.MarkWorkspaceExecExitedRow) db.WorkspaceExec {
	return db.WorkspaceExec(row)
}

func workspaceExecStreamChunkResponse(row db.WorkspaceExecStreamChunk) api.WorkspaceExecStreamChunkResponse {
	return api.WorkspaceExecStreamChunkResponse{
		ID:          pgvalue.MustUUIDValue(row.ID).String(),
		Stream:      string(row.Stream),
		OffsetStart: row.OffsetStart,
		OffsetEnd:   row.OffsetEnd,
		Data:        row.Data,
		ObservedAt:  pgvalue.Time(row.ObservedAt),
		CreatedAt:   pgvalue.Time(row.CreatedAt),
	}
}
