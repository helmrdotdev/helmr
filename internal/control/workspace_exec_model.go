package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func NormalizeExecCommand(command []string) ([]string, error) {
	if len(command) == 0 {
		return nil, errors.New("command is required")
	}
	normalized := make([]string, 0, len(command))
	for _, part := range command {
		if strings.TrimSpace(part) == "" {
			return nil, errors.New("command entries cannot be empty")
		}
		if strings.Contains(part, "\x00") {
			return nil, errors.New("command entries cannot contain NUL")
		}
		normalized = append(normalized, part)
	}
	return normalized, nil
}

func NormalizeExecCwd(cwd string) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return "", nil
	}
	if !strings.HasPrefix(cwd, "/") {
		return "", errors.New("cwd must be absolute")
	}
	if strings.Contains(cwd, "\x00") {
		return "", errors.New("cwd cannot contain NUL")
	}
	for segment := range strings.SplitSeq(cwd, "/") {
		if segment == ".." {
			return "", errors.New("cwd cannot contain '..'")
		}
	}
	return cwd, nil
}

func ExecEnvShape(env map[string]string) ([]byte, error) {
	if env == nil {
		return []byte(`{}`), nil
	}
	for key := range env {
		if strings.TrimSpace(key) == "" || strings.Contains(key, "\x00") {
			return nil, errors.New("env names must be non-empty and cannot contain NUL")
		}
	}
	out, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func ExecCreateFingerprint(command []string, cwd string, envShape []byte, detached bool, filesystemMode db.WorkspaceFilesystemMode) (string, error) {
	payload, err := json.Marshal(struct {
		Command  []string        `json:"command"`
		Cwd      string          `json:"cwd"`
		EnvShape json.RawMessage `json:"env_shape"`
		Detached bool            `json:"detached"`
		Mode     string          `json:"filesystem_mode"`
	}{Command: command, Cwd: cwd, EnvShape: envShape, Detached: detached, Mode: string(filesystemMode)})
	if err != nil {
		return "", fmt.Errorf("encode workspace exec fingerprint payload: %w", err)
	}
	return wire.RequestFingerprint(string(db.WorkspaceOperationIdempotencyKindWorkspaceExecCreate), payload)
}

func ExecStateTerminal(state db.WorkspaceExecState) bool {
	switch state {
	case db.WorkspaceExecStateExited, db.WorkspaceExecStateLost, db.WorkspaceExecStateFailed, db.WorkspaceExecStateTerminated:
		return true
	default:
		return false
	}
}

func ExecStreamCursor(row db.LockWorkspaceExecForStreamAppendRow, stream db.WorkspaceExecStream) int64 {
	switch stream {
	case db.WorkspaceExecStreamStdin:
		return row.StdinCursor
	case db.WorkspaceExecStreamStdout:
		return row.StdoutCursor
	case db.WorkspaceExecStreamStderr:
		return row.StderrCursor
	default:
		return -1
	}
}

func ExecStreamCursorFromRow(row db.WorkspaceExec, stream db.WorkspaceExecStream) int64 {
	switch stream {
	case db.WorkspaceExecStreamStdout:
		return row.StdoutCursor
	case db.WorkspaceExecStreamStderr:
		return row.StderrCursor
	default:
		return 0
	}
}

func ExecStartOperationRequest(row db.WorkspaceExec) ([]byte, error) {
	return json.Marshal(struct {
		ExecID         string          `json:"exec_id"`
		Command        json.RawMessage `json:"command"`
		Cwd            string          `json:"cwd"`
		EnvShape       json.RawMessage `json:"env_shape"`
		FilesystemMode string          `json:"filesystem_mode"`
		Detached       bool            `json:"detached"`
	}{
		ExecID:         pgvalue.MustUUIDValue(row.ID).String(),
		Command:        json.RawMessage(row.Command),
		Cwd:            row.Cwd,
		EnvShape:       json.RawMessage(row.EnvShape),
		FilesystemMode: string(row.FilesystemMode),
		Detached:       row.Detached,
	})
}
