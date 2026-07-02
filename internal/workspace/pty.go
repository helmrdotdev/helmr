package workspace

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func NormalizePtySize(cols int32, rows int32) (int32, int32, error) {
	if cols <= 0 {
		cols = 80
	}
	if rows <= 0 {
		rows = 24
	}
	if cols > 500 || rows > 500 {
		return 0, 0, errors.New("pty size must be at most 500x500")
	}
	return cols, rows, nil
}

func PtyCreateFingerprint(cwd string, cols int32, rows int32, filesystemMode db.WorkspaceFilesystemMode) (string, error) {
	payload, err := json.Marshal(struct {
		Cwd  string `json:"cwd"`
		Cols int32  `json:"cols"`
		Rows int32  `json:"rows"`
		Mode string `json:"filesystem_mode"`
	}{Cwd: cwd, Cols: cols, Rows: rows, Mode: string(filesystemMode)})
	if err != nil {
		return "", fmt.Errorf("encode workspace pty fingerprint payload: %w", err)
	}
	return wire.RequestFingerprint(string(db.WorkspaceOperationIdempotencyKindWorkspacePtyCreate), payload)
}

func PtyStateTerminal(state db.WorkspacePtyState) bool {
	switch state {
	case db.WorkspacePtyStateClosed, db.WorkspacePtyStateLost, db.WorkspacePtyStateFailed:
		return true
	default:
		return false
	}
}

func PtyStreamCursor(row db.LockWorkspacePtyForStreamAppendRow, stream db.WorkspacePtyStream) int64 {
	switch stream {
	case db.WorkspacePtyStreamInput:
		return row.InputCursor
	case db.WorkspacePtyStreamOutput:
		return row.OutputCursor
	default:
		return -1
	}
}

func PtyCreateOperationRequest(row db.WorkspacePtySession) ([]byte, error) {
	return json.Marshal(struct {
		PtyID          string `json:"pty_id"`
		Cwd            string `json:"cwd"`
		Cols           int32  `json:"cols"`
		Rows           int32  `json:"rows"`
		FilesystemMode string `json:"filesystem_mode"`
	}{
		PtyID:          pgvalue.MustUUIDValue(row.ID).String(),
		Cwd:            row.Cwd,
		Cols:           row.Cols,
		Rows:           row.Rows,
		FilesystemMode: string(row.FilesystemMode),
	})
}

func PtyResizeOperationRequest(row db.WorkspacePtySession) ([]byte, error) {
	if !row.ResizeCols.Valid || !row.ResizeRows.Valid {
		return nil, errors.New("workspace pty resize target is required")
	}
	return json.Marshal(struct {
		PtyID string `json:"pty_id"`
		Cols  int32  `json:"cols"`
		Rows  int32  `json:"rows"`
	}{
		PtyID: pgvalue.MustUUIDValue(row.ID).String(),
		Cols:  row.ResizeCols.Int32,
		Rows:  row.ResizeRows.Int32,
	})
}

func PtyCloseOperationRequest(row db.WorkspacePtySession) ([]byte, error) {
	return json.Marshal(struct {
		PtyID string `json:"pty_id"`
	}{
		PtyID: pgvalue.MustUUIDValue(row.ID).String(),
	})
}
