package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
)

const workspaceStreamFollowMaxDuration = 30 * time.Minute

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

func (s *Server) followWorkspaceExecStream(w http.ResponseWriter, r *http.Request, exec db.WorkspaceProcess, stream string, cursor int64, limit int32) {
	if s.eventStream == nil {
		writeError(w, unavailable(errors.New("event stream is not configured")))
		return
	}
	if stream != workspaceStreamStdout && stream != workspaceStreamStderr {
		writeError(w, badRequest(errors.New("workspace exec follow is only available for output streams")))
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	ctx, cancel := context.WithTimeout(r.Context(), workspaceStreamFollowMaxDuration)
	defer cancel()
	query := telemetry.TerminalOutputQuery{
		OrgID:         pgvalue.MustUUIDValue(exec.OrgID),
		WorkerGroupID: exec.WorkerGroupID,
		ProjectID:     pgvalue.MustUUIDValue(exec.ProjectID),
		EnvironmentID: pgvalue.MustUUIDValue(exec.EnvironmentID),
		WorkspaceID:   pgvalue.MustUUIDValue(exec.WorkspaceID),
		ResourceKind:  "workspace_process",
		ResourceID:    pgvalue.MustUUIDValue(exec.ID),
		StreamName:    stream,
	}
	err := s.eventStream.ReadTerminalOutput(ctx, query, cursor, limit, func(chunk telemetry.TerminalOutputChunk) error {
		response := workspaceExecTerminalOutputResponse(chunk)
		if err := writeWorkspaceStreamSSE(w, flusher, encoder, strconv.FormatInt(response.OffsetEnd, 10), "workspace_stream_chunk", response); err != nil {
			return err
		}
		cursor = response.OffsetEnd
		return nil
	}, func() error {
		current, err := s.db.GetWorkspaceExec(ctx, db.GetWorkspaceExecParams{
			OrgID:         exec.OrgID,
			ProjectID:     exec.ProjectID,
			EnvironmentID: exec.EnvironmentID,
			WorkspaceID:   exec.WorkspaceID,
			ID:            exec.ID,
		})
		if err == nil && execStateTerminal(current.State) {
			pending, pendingErr := s.hasUnpublishedTerminalOutput(ctx, pgvalue.MustUUIDValue(exec.OrgID), exec.WorkerGroupID, "workspace_process", pgvalue.MustUUIDValue(exec.ID), stream)
			if pendingErr != nil {
				return pendingErr
			}
			if !pending {
				eventName := "workspace_stream_terminal"
				if current.State == db.WorkspaceProcessStateLost {
					eventName = "workspace_stream_lost"
				}
				if err := writeWorkspaceStreamSSE(w, flusher, encoder, strconv.FormatInt(cursor, 10), eventName, api.WorkspaceStreamTerminalResponse{
					ResourceKind: "workspace_exec",
					ResourceID:   pgvalue.MustUUIDValue(exec.ID).String(),
					Stream:       stream,
					State:        string(current.State),
					Cursor:       cursor,
					Error:        json.RawMessage(current.Error),
				}); err != nil {
					return err
				}
				return errLiveTelemetryFollowComplete
			}
		}
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.log.Warn("read workspace exec while following stream failed", "exec_id", pgvalue.MustUUIDValue(exec.ID).String(), "error", err)
			return s.writeWorkspaceStreamServerError(w, flusher, encoder, cursor, "workspace_stream_follow_failed", "failed to read workspace exec state")
		}
		_, _ = fmt.Fprint(w, ": keep-alive\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil && !errors.Is(err, errLiveTelemetryFollowComplete) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		if s.writeWorkspaceStreamFollowError(w, flusher, encoder, cursor, err) {
			return
		}
		s.log.Warn("follow workspace exec stream failed", "exec_id", pgvalue.MustUUIDValue(exec.ID).String(), "stream", string(stream), "error", err)
	}
}

func (s *Server) followWorkspacePtyOutput(w http.ResponseWriter, r *http.Request, pty db.WorkspaceProcess, cursor int64, limit int32) {
	if s.eventStream == nil {
		writeError(w, unavailable(errors.New("event stream is not configured")))
		return
	}
	flusher, _ := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	ctx, cancel := context.WithTimeout(r.Context(), workspaceStreamFollowMaxDuration)
	defer cancel()
	query := telemetry.TerminalOutputQuery{
		OrgID:         pgvalue.MustUUIDValue(pty.OrgID),
		WorkerGroupID: pty.WorkerGroupID,
		ProjectID:     pgvalue.MustUUIDValue(pty.ProjectID),
		EnvironmentID: pgvalue.MustUUIDValue(pty.EnvironmentID),
		WorkspaceID:   pgvalue.MustUUIDValue(pty.WorkspaceID),
		ResourceKind:  "workspace_process",
		ResourceID:    pgvalue.MustUUIDValue(pty.ID),
		StreamName:    workspaceStreamOutput,
	}
	err := s.eventStream.ReadTerminalOutput(ctx, query, cursor, limit, func(chunk telemetry.TerminalOutputChunk) error {
		response := workspacePtyTerminalOutputResponse(chunk)
		if err := writeWorkspaceStreamSSE(w, flusher, encoder, strconv.FormatInt(response.OffsetEnd, 10), "workspace_stream_chunk", response); err != nil {
			return err
		}
		cursor = response.OffsetEnd
		return nil
	}, func() error {
		current, err := s.db.GetWorkspacePtySession(ctx, db.GetWorkspacePtySessionParams{
			OrgID:         pty.OrgID,
			ProjectID:     pty.ProjectID,
			EnvironmentID: pty.EnvironmentID,
			WorkspaceID:   pty.WorkspaceID,
			ID:            pty.ID,
		})
		if err == nil && ptyStateTerminal(current.State) {
			pending, pendingErr := s.hasUnpublishedTerminalOutput(ctx, pgvalue.MustUUIDValue(pty.OrgID), pty.WorkerGroupID, "workspace_process", pgvalue.MustUUIDValue(pty.ID), workspaceStreamOutput)
			if pendingErr != nil {
				return pendingErr
			}
			if !pending {
				eventName := "workspace_stream_terminal"
				if current.State == db.WorkspaceProcessStateLost {
					eventName = "workspace_stream_lost"
				}
				if err := writeWorkspaceStreamSSE(w, flusher, encoder, strconv.FormatInt(cursor, 10), eventName, api.WorkspaceStreamTerminalResponse{
					ResourceKind: "workspace_pty",
					ResourceID:   pgvalue.MustUUIDValue(pty.ID).String(),
					Stream:       workspaceStreamOutput,
					State:        string(current.State),
					Cursor:       cursor,
					Error:        json.RawMessage(current.Error),
				}); err != nil {
					return err
				}
				return errLiveTelemetryFollowComplete
			}
		}
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.log.Warn("read workspace pty while following output failed", "pty_id", pgvalue.MustUUIDValue(pty.ID).String(), "error", err)
			return s.writeWorkspaceStreamServerError(w, flusher, encoder, cursor, "workspace_stream_follow_failed", "failed to read workspace pty state")
		}
		_, _ = fmt.Fprint(w, ": keep-alive\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	})
	if err != nil && !errors.Is(err, errLiveTelemetryFollowComplete) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		if s.writeWorkspaceStreamFollowError(w, flusher, encoder, cursor, err) {
			return
		}
		s.log.Warn("follow workspace pty output failed", "pty_id", pgvalue.MustUUIDValue(pty.ID).String(), "error", err)
	}
}

func (s *Server) listWorkspaceExecTerminalOutput(ctx context.Context, exec db.WorkspaceProcess, stream string, cursor int64, limit int32) ([]api.WorkspaceExecStreamChunkResponse, int64, error) {
	page, err := s.telemetryReader.ListTerminalOutput(ctx, telemetry.TerminalOutputQuery{
		OrgID:         pgvalue.MustUUIDValue(exec.OrgID),
		WorkerGroupID: exec.WorkerGroupID,
		ProjectID:     pgvalue.MustUUIDValue(exec.ProjectID),
		EnvironmentID: pgvalue.MustUUIDValue(exec.EnvironmentID),
		WorkspaceID:   pgvalue.MustUUIDValue(exec.WorkspaceID),
		ResourceKind:  "workspace_process",
		ResourceID:    pgvalue.MustUUIDValue(exec.ID),
		StreamName:    stream,
		AfterOffset:   cursor,
		Limit:         limit,
	})
	if err != nil {
		return nil, cursor, err
	}
	out := make([]api.WorkspaceExecStreamChunkResponse, 0, len(page.Chunks))
	for _, row := range page.Chunks {
		out = append(out, workspaceExecTerminalOutputResponse(row))
	}
	return out, page.LastOffset, nil
}

func (s *Server) listWorkspacePtyTerminalOutput(ctx context.Context, pty db.WorkspaceProcess, cursor int64, limit int32) ([]api.WorkspacePtyStreamChunkResponse, int64, error) {
	page, err := s.telemetryReader.ListTerminalOutput(ctx, telemetry.TerminalOutputQuery{
		OrgID:         pgvalue.MustUUIDValue(pty.OrgID),
		WorkerGroupID: pty.WorkerGroupID,
		ProjectID:     pgvalue.MustUUIDValue(pty.ProjectID),
		EnvironmentID: pgvalue.MustUUIDValue(pty.EnvironmentID),
		WorkspaceID:   pgvalue.MustUUIDValue(pty.WorkspaceID),
		ResourceKind:  "workspace_process",
		ResourceID:    pgvalue.MustUUIDValue(pty.ID),
		StreamName:    string(workspaceStreamOutput),
		AfterOffset:   cursor,
		Limit:         limit,
	})
	if err != nil {
		return nil, cursor, err
	}
	out := make([]api.WorkspacePtyStreamChunkResponse, 0, len(page.Chunks))
	for _, row := range page.Chunks {
		out = append(out, workspacePtyTerminalOutputResponse(row))
	}
	return out, page.LastOffset, nil
}

func workspaceExecTerminalOutputResponse(row telemetry.TerminalOutputChunk) api.WorkspaceExecStreamChunkResponse {
	return api.WorkspaceExecStreamChunkResponse{
		ID:          row.ID,
		Stream:      row.Stream,
		OffsetStart: row.OffsetStart,
		OffsetEnd:   row.OffsetEnd,
		Data:        row.Data,
		ObservedAt:  row.ObservedAt,
		CreatedAt:   row.CreatedAt,
	}
}

func workspacePtyTerminalOutputResponse(row telemetry.TerminalOutputChunk) api.WorkspacePtyStreamChunkResponse {
	return api.WorkspacePtyStreamChunkResponse{
		ID:          row.ID,
		Stream:      row.Stream,
		OffsetStart: row.OffsetStart,
		OffsetEnd:   row.OffsetEnd,
		Data:        row.Data,
		ObservedAt:  row.ObservedAt,
		CreatedAt:   row.CreatedAt,
	}
}

func (s *Server) writeWorkspaceStreamFollowError(w http.ResponseWriter, flusher http.Flusher, encoder *json.Encoder, cursor int64, err error) bool {
	var lagging telemetry.LaggingError
	if errors.As(err, &lagging) {
		_ = s.writeWorkspaceStreamServerError(w, flusher, encoder, cursor, errWorkspaceStreamCursorExpired.code, workspaceStreamCursorExpiredAt(lagging.WantSeq).Error())
		return true
	}
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
