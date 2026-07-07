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

func normalizeExecCommand(command []string) ([]string, error) {
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

func normalizeExecCwd(cwd string) (string, error) {
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

func execEnvShape(env map[string]string) ([]byte, error) {
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

func execCreateFingerprint(command []string, cwd string, envShape []byte, detached bool, filesystemMode db.WorkspaceFilesystemMode) (string, error) {
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
	return wire.RequestFingerprint("workspace_command_create", payload)
}

func execStateTerminal(state db.WorkspaceProcessState) bool {
	switch state {
	case db.WorkspaceProcessStateExited, db.WorkspaceProcessStateLost, db.WorkspaceProcessStateFailed:
		return true
	default:
		return false
	}
}

func execStreamCursor(row db.LockWorkspaceExecForStreamAppendRow, stream string) int64 {
	switch stream {
	case workspaceStreamStdin:
		return row.StdinCursor
	case workspaceStreamStdout:
		return row.StdoutCursor
	case workspaceStreamStderr:
		return row.StderrCursor
	default:
		return -1
	}
}

func execStreamCursorFromRow(row db.WorkspaceProcess, stream string) int64 {
	switch stream {
	case workspaceStreamStdout:
		return row.StdoutCursor
	case workspaceStreamStderr:
		return row.StderrCursor
	default:
		return 0
	}
}

func execStartOperationRequest(row db.WorkspaceProcess) ([]byte, error) {
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
