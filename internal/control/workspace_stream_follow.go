package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func workspaceStreamFollowRequested(r *http.Request) bool {
	return r.URL.Query().Get("follow") == "1" || strings.Contains(r.Header.Get("accept"), "text/event-stream")
}

func parseWorkspaceStreamFollowCursor(r *http.Request) (int64, error) {
	if raw := strings.TrimSpace(r.URL.Query().Get("cursor")); raw != "" {
		return parseNonNegativeWorkspaceStreamCursor(raw)
	}
	if raw := strings.TrimSpace(r.Header.Get("last-event-id")); raw != "" {
		return parseNonNegativeWorkspaceStreamCursor(raw)
	}
	return 0, nil
}

func parseNonNegativeWorkspaceStreamCursor(raw string) (int64, error) {
	cursor, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || cursor < 0 {
		return 0, errors.New("cursor must be a non-negative integer")
	}
	return cursor, nil
}

func (s *Server) followWorkspaceExecStream(w http.ResponseWriter, r *http.Request, exec db.WorkspaceExec, stream db.WorkspaceExecStream, cursor int64, limit int32) {
	if s.workspaceStreams == nil {
		writeError(w, unavailable(errors.New("workspace stream notifier is not configured")))
		return
	}
	if err := s.ensureWorkspaceExecCursorAvailable(r.Context(), exec, stream, cursor); err != nil {
		writeError(w, err)
		return
	}
	streamKey := workspaceStreamKey(exec.OrgID, "workspace_exec", exec.ID, string(stream))
	wakeCursor, err := s.workspaceStreams.LatestID(r.Context(), streamKey)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "read workspace stream wake cursor", err)
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	ctx, cancel := context.WithTimeout(r.Context(), workspaceStreamFollowMaxDuration)
	defer cancel()
	for {
		next, count, err := s.writeWorkspaceExecStreamChunksAfter(ctx, w, flusher, encoder, exec, stream, cursor, limit)
		if err != nil {
			if s.writeWorkspaceStreamFollowError(w, flusher, encoder, cursor, err) {
				return
			}
			s.log.Warn("follow workspace exec stream failed", "exec_id", pgvalue.MustUUIDValue(exec.ID).String(), "stream", string(stream), "error", err)
			return
		}
		cursor = next
		if count == int(limit) {
			continue
		}
		current, err := s.db.GetWorkspaceExec(ctx, db.GetWorkspaceExecParams{
			OrgID:         exec.OrgID,
			ProjectID:     exec.ProjectID,
			EnvironmentID: exec.EnvironmentID,
			WorkspaceID:   exec.WorkspaceID,
			ID:            exec.ID,
		})
		if err == nil && workspace.ExecStateTerminal(current.State) {
			if err := s.drainWorkspaceExecTerminal(ctx, w, flusher, encoder, current, stream, cursor, limit); err != nil {
				s.log.Debug("write workspace exec terminal stream failed", "exec_id", pgvalue.MustUUIDValue(exec.ID).String(), "stream", string(stream), "error", err)
			}
			return
		}
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.log.Warn("read workspace exec while following stream failed", "exec_id", pgvalue.MustUUIDValue(exec.ID).String(), "error", err)
			_ = s.writeWorkspaceStreamServerError(w, flusher, encoder, cursor, "workspace_stream_follow_failed", "failed to read workspace exec state")
			return
		}
		nextWakeCursor, err := s.workspaceStreams.Wait(ctx, streamKey, wakeCursor)
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				s.log.Warn("wait workspace exec stream wakeup failed", "exec_id", pgvalue.MustUUIDValue(exec.ID).String(), "stream", string(stream), "error", err)
			}
			return
		}
		if nextWakeCursor == wakeCursor {
			_, _ = fmt.Fprint(w, ": keep-alive\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		wakeCursor = nextWakeCursor
	}
}

func (s *Server) drainWorkspaceExecTerminal(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, encoder *json.Encoder, exec db.WorkspaceExec, stream db.WorkspaceExecStream, cursor int64, limit int32) error {
	for {
		next, count, err := s.writeWorkspaceExecStreamChunksAfter(ctx, w, flusher, encoder, exec, stream, cursor, limit)
		if err != nil {
			_ = s.writeWorkspaceStreamFollowError(w, flusher, encoder, cursor, err)
			return err
		}
		cursor = next
		if count < int(limit) {
			break
		}
	}
	eventName := "workspace_stream_terminal"
	if exec.State == db.WorkspaceExecStateLost {
		eventName = "workspace_stream_lost"
	}
	return writeWorkspaceStreamSSE(w, flusher, encoder, strconv.FormatInt(cursor, 10), eventName, api.WorkspaceStreamTerminalResponse{
		ResourceKind: "workspace_exec",
		ResourceID:   pgvalue.MustUUIDValue(exec.ID).String(),
		Stream:       string(stream),
		State:        string(exec.State),
		Cursor:       cursor,
		Error:        json.RawMessage(exec.Error),
	})
}

func (s *Server) writeWorkspaceExecStreamChunksAfter(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, encoder *json.Encoder, exec db.WorkspaceExec, stream db.WorkspaceExecStream, cursor int64, limit int32) (int64, int, error) {
	if err := s.ensureWorkspaceExecCursorAvailable(ctx, exec, stream, cursor); err != nil {
		return cursor, 0, err
	}
	rows, err := s.db.ListWorkspaceExecStreamChunksAfter(ctx, db.ListWorkspaceExecStreamChunksAfterParams{
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
		return cursor, 0, err
	}
	for _, row := range rows {
		chunk := workspaceExecStreamChunkResponse(row)
		if err := writeWorkspaceStreamSSE(w, flusher, encoder, strconv.FormatInt(chunk.OffsetEnd, 10), "workspace_stream_chunk", chunk); err != nil {
			return cursor, 0, err
		}
		cursor = row.OffsetEnd
	}
	return cursor, len(rows), nil
}

func (s *Server) followWorkspacePtyOutput(w http.ResponseWriter, r *http.Request, pty db.WorkspacePtySession, cursor int64, limit int32) {
	if s.workspaceStreams == nil {
		writeError(w, unavailable(errors.New("workspace stream notifier is not configured")))
		return
	}
	if err := s.ensureWorkspacePtyCursorAvailable(r.Context(), pty, db.WorkspacePtyStreamOutput, cursor); err != nil {
		writeError(w, err)
		return
	}
	streamKey := workspaceStreamKey(pty.OrgID, "workspace_pty", pty.ID, "output")
	wakeCursor, err := s.workspaceStreams.LatestID(r.Context(), streamKey)
	if err != nil {
		s.writeWorkspacePrimitiveError(w, "read workspace stream wake cursor", err)
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	ctx, cancel := context.WithTimeout(r.Context(), workspaceStreamFollowMaxDuration)
	defer cancel()
	for {
		next, count, err := s.writeWorkspacePtyOutputChunksAfter(ctx, w, flusher, encoder, pty, cursor, limit)
		if err != nil {
			if s.writeWorkspaceStreamFollowError(w, flusher, encoder, cursor, err) {
				return
			}
			s.log.Warn("follow workspace pty output failed", "pty_id", pgvalue.MustUUIDValue(pty.ID).String(), "error", err)
			return
		}
		cursor = next
		if count == int(limit) {
			continue
		}
		current, err := s.db.GetWorkspacePtySession(ctx, db.GetWorkspacePtySessionParams{
			OrgID:         pty.OrgID,
			ProjectID:     pty.ProjectID,
			EnvironmentID: pty.EnvironmentID,
			WorkspaceID:   pty.WorkspaceID,
			ID:            pty.ID,
		})
		if err == nil && workspace.PtyStateTerminal(current.State) {
			if err := s.drainWorkspacePtyTerminal(ctx, w, flusher, encoder, current, cursor, limit); err != nil {
				s.log.Debug("write workspace pty terminal stream failed", "pty_id", pgvalue.MustUUIDValue(pty.ID).String(), "error", err)
			}
			return
		}
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.log.Warn("read workspace pty while following output failed", "pty_id", pgvalue.MustUUIDValue(pty.ID).String(), "error", err)
			_ = s.writeWorkspaceStreamServerError(w, flusher, encoder, cursor, "workspace_stream_follow_failed", "failed to read workspace pty state")
			return
		}
		nextWakeCursor, err := s.workspaceStreams.Wait(ctx, streamKey, wakeCursor)
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				s.log.Warn("wait workspace pty output wakeup failed", "pty_id", pgvalue.MustUUIDValue(pty.ID).String(), "error", err)
			}
			return
		}
		if nextWakeCursor == wakeCursor {
			_, _ = fmt.Fprint(w, ": keep-alive\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		wakeCursor = nextWakeCursor
	}
}

func (s *Server) drainWorkspacePtyTerminal(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, encoder *json.Encoder, pty db.WorkspacePtySession, cursor int64, limit int32) error {
	for {
		next, count, err := s.writeWorkspacePtyOutputChunksAfter(ctx, w, flusher, encoder, pty, cursor, limit)
		if err != nil {
			_ = s.writeWorkspaceStreamFollowError(w, flusher, encoder, cursor, err)
			return err
		}
		cursor = next
		if count < int(limit) {
			break
		}
	}
	eventName := "workspace_stream_terminal"
	if pty.State == db.WorkspacePtyStateLost {
		eventName = "workspace_stream_lost"
	}
	return writeWorkspaceStreamSSE(w, flusher, encoder, strconv.FormatInt(cursor, 10), eventName, api.WorkspaceStreamTerminalResponse{
		ResourceKind: "workspace_pty",
		ResourceID:   pgvalue.MustUUIDValue(pty.ID).String(),
		Stream:       "output",
		State:        string(pty.State),
		Cursor:       cursor,
		Error:        json.RawMessage(pty.Error),
	})
}

func (s *Server) writeWorkspacePtyOutputChunksAfter(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, encoder *json.Encoder, pty db.WorkspacePtySession, cursor int64, limit int32) (int64, int, error) {
	if err := s.ensureWorkspacePtyCursorAvailable(ctx, pty, db.WorkspacePtyStreamOutput, cursor); err != nil {
		return cursor, 0, err
	}
	rows, err := s.db.ListWorkspacePtyStreamChunksAfter(ctx, db.ListWorkspacePtyStreamChunksAfterParams{
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
		return cursor, 0, err
	}
	for _, row := range rows {
		chunk := workspacePtyStreamChunkResponse(row)
		if err := writeWorkspaceStreamSSE(w, flusher, encoder, strconv.FormatInt(chunk.OffsetEnd, 10), "workspace_stream_chunk", chunk); err != nil {
			return cursor, 0, err
		}
		cursor = row.OffsetEnd
	}
	return cursor, len(rows), nil
}

func (s *Server) writeWorkspaceStreamFollowError(w http.ResponseWriter, flusher http.Flusher, encoder *json.Encoder, cursor int64, err error) bool {
	var apiErr apiError
	if errors.As(err, &apiErr) {
		var coded codedError
		if apiErr.kind == errGone && errors.As(apiErr.err, &coded) && coded.code == "workspace_stream_cursor_expired" {
			_ = s.writeWorkspaceStreamServerError(w, flusher, encoder, cursor, coded.code, coded.Error())
			return true
		}
	}
	return false
}

func (s *Server) writeWorkspaceStreamServerError(w http.ResponseWriter, flusher http.Flusher, encoder *json.Encoder, cursor int64, code string, message string) error {
	return writeWorkspaceStreamSSE(w, flusher, encoder, strconv.FormatInt(cursor, 10), "workspace_stream_error", api.WorkspaceStreamErrorResponse{
		Code:    code,
		Message: message,
		Cursor:  cursor,
	})
}

func writeWorkspaceStreamSSE(w http.ResponseWriter, flusher http.Flusher, encoder *json.Encoder, id string, event string, payload any) error {
	if _, err := fmt.Fprintf(w, "id: %s\n", id); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\n", event); err != nil {
		return err
	}
	if _, err := fmt.Fprint(w, "data: "); err != nil {
		return err
	}
	if err := encoder.Encode(payload); err != nil {
		return err
	}
	if _, err := fmt.Fprint(w, "\n"); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}
