package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cli/session"
	"github.com/helmrdotdev/helmr/internal/client"
)

func TestWorkspaceExecRequiresDashBeforeCommand(t *testing.T) {
	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"workspace", "exec", "workspace-1", "true"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "workspace exec WORKSPACE -- COMMAND") {
		t.Fatalf("err = %v", err)
	}
}

func TestWorkspaceExecRejectsForegroundJSONBeforeCreate(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"workspace", "exec", "workspace-1", "--json", "--", "true"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "workspace exec --json requires --detach") {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("server was called")
	}
}

func TestWorkspaceDeleteRequiresYes(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"workspace", "delete", "workspace-1"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "workspace delete requires --yes") {
		t.Fatalf("err = %v", err)
	}
	if called {
		t.Fatal("server was called")
	}
}

func TestWorkspaceOpenLoadsWorkspaceBeforePrintingURL(t *testing.T) {
	state, _ := installTestCLIConfig(t)
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/api/me" {
			_ = json.NewEncoder(w).Encode(api.MeResponse{UserID: "user-1", PublicURL: "https://console.example.test"})
			return
		}
		called = true
		if r.Method != http.MethodGet || r.URL.Path != "/api/projects/project-1/environments/env-1/workspaces/workspace-1" {
			t.Fatalf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(api.WorkspaceEnvelope{Workspace: api.WorkspaceResponse{
			ID:        "workspace-1",
			State:     "active",
			SandboxID: "sandbox-1",
		}})
	}))
	defer server.Close()
	if err := state.Save(session.Config{DefaultHost: server.URL}); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveToken(server.URL, "session-token"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"workspace", "open", "workspace-1", "-p", "project-1", "-e", "env-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("workspace was not loaded")
	}
	if strings.TrimSpace(out.String()) != "https://console.example.test/workspaces/workspace-1" {
		t.Fatalf("output = %q", out.String())
	}
}

func TestWorkspaceExecStreamsAndReturnsRemoteExitCode(t *testing.T) {
	var createRequest api.WorkspaceExecCreateRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/workspaces/workspace-1/execs":
			if err := json.NewDecoder(r.Body).Decode(&createRequest); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(api.WorkspaceExecEnvelope{Exec: api.WorkspaceExecResponse{
				ID:          "exec-1",
				WorkspaceID: "workspace-1",
				State:       "running",
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stdin/close":
			_ = json.NewEncoder(w).Encode(api.WorkspaceExecEnvelope{Exec: api.WorkspaceExecResponse{
				ID:          "exec-1",
				WorkspaceID: "workspace-1",
				State:       "running",
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stdout" && r.URL.Query().Get("follow") == "1":
			writeWorkspaceExecTestStream(t, w, "stdout", []byte("ok\n"))
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stderr" && r.URL.Query().Get("follow") == "1":
			writeWorkspaceExecTestStream(t, w, "stderr", []byte("warn\n"))
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stdout":
			_ = json.NewEncoder(w).Encode(workspaceExecTestListResponse(r, "stdout", []byte("ok\n")))
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stderr":
			_ = json.NewEncoder(w).Encode(workspaceExecTestListResponse(r, "stderr", []byte("warn\n")))
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1":
			code := int32(7)
			_ = json.NewEncoder(w).Encode(api.WorkspaceExecEnvelope{Exec: api.WorkspaceExecResponse{
				ID:          "exec-1",
				WorkspaceID: "workspace-1",
				State:       "exited",
				ExitCode:    &code,
			}})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out, stderr bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"workspace", "exec", "workspace-1", "--", "false"})
	err := cmd.Execute()
	var exitErr exitCodeError
	if err == nil || !errors.As(err, &exitErr) || exitErr.code != 7 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out.String(), "ok\n") || !strings.Contains(stderr.String(), "warn\n") {
		t.Fatalf("stdout=%q stderr=%q", out.String(), stderr.String())
	}
	if strings.Join(createRequest.Command, " ") != "false" {
		t.Fatalf("command = %+v", createRequest.Command)
	}
}

func TestWorkspaceExecWritesAndClosesStdin(t *testing.T) {
	var stdinRequest api.WorkspaceExecStdinWriteRequest
	stdinClosed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/workspaces/workspace-1/execs":
			_ = json.NewEncoder(w).Encode(api.WorkspaceExecEnvelope{Exec: api.WorkspaceExecResponse{
				ID:          "exec-1",
				WorkspaceID: "workspace-1",
				State:       "running",
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stdin":
			if err := json.NewDecoder(r.Body).Decode(&stdinRequest); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(api.WorkspaceExecStreamChunkResponse{
				ID:          "stdin-chunk-1",
				Stream:      "stdin",
				OffsetStart: 0,
				OffsetEnd:   int64(len(stdinRequest.Data)),
				Data:        stdinRequest.Data,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stdin/close":
			stdinClosed = true
			_ = json.NewEncoder(w).Encode(api.WorkspaceExecEnvelope{Exec: api.WorkspaceExecResponse{
				ID:          "exec-1",
				WorkspaceID: "workspace-1",
				State:       "running",
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stdout" && r.URL.Query().Get("follow") == "1":
			writeWorkspaceExecTestTerminal(t, w, "stdout", "exited", 0)
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stderr" && r.URL.Query().Get("follow") == "1":
			writeWorkspaceExecTestTerminal(t, w, "stderr", "exited", 0)
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stdout":
			_ = json.NewEncoder(w).Encode(api.ListWorkspaceExecStreamChunksResponse{})
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stderr":
			_ = json.NewEncoder(w).Encode(api.ListWorkspaceExecStreamChunksResponse{})
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1":
			code := int32(0)
			_ = json.NewEncoder(w).Encode(api.WorkspaceExecEnvelope{Exec: api.WorkspaceExecResponse{
				ID:          "exec-1",
				WorkspaceID: "workspace-1",
				State:       "exited",
				ExitCode:    &code,
			}})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetIn(strings.NewReader("input\n"))
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"workspace", "exec", "workspace-1", "--", "cat"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if stdinRequest.Offset != 0 || string(stdinRequest.Data) != "input\n" {
		t.Fatalf("stdin request = %+v", stdinRequest)
	}
	if !stdinClosed {
		t.Fatal("stdin was not closed")
	}
}

func TestWorkspaceExecLogsFollowStartsAfterReplayCursor(t *testing.T) {
	var stdoutFollowCursor string
	var stderrFollowCursor string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stdout" && r.URL.Query().Get("follow") == "":
			_ = json.NewEncoder(w).Encode(api.ListWorkspaceExecStreamChunksResponse{Chunks: []api.WorkspaceExecStreamChunkResponse{{
				ID:          "stdout-chunk-1",
				Stream:      "stdout",
				OffsetStart: 0,
				OffsetEnd:   3,
				Data:        []byte("out"),
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stderr" && r.URL.Query().Get("follow") == "":
			_ = json.NewEncoder(w).Encode(api.ListWorkspaceExecStreamChunksResponse{Chunks: []api.WorkspaceExecStreamChunkResponse{{
				ID:          "stderr-chunk-1",
				Stream:      "stderr",
				OffsetStart: 0,
				OffsetEnd:   4,
				Data:        []byte("warn"),
			}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stdout" && r.URL.Query().Get("follow") == "1":
			stdoutFollowCursor = r.URL.Query().Get("cursor")
			writeWorkspaceExecTestTerminal(t, w, "stdout", "exited", 3)
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stderr" && r.URL.Query().Get("follow") == "1":
			stderrFollowCursor = r.URL.Query().Get("cursor")
			writeWorkspaceExecTestTerminal(t, w, "stderr", "exited", 4)
		default:
			t.Fatalf("%s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var out, stderr bytes.Buffer
	cmd := newRootCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"workspace", "exec", "logs", "workspace-1", "exec-1", "--follow"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if out.String() != "out" || stderr.String() != "warn" {
		t.Fatalf("stdout=%q stderr=%q", out.String(), stderr.String())
	}
	if stdoutFollowCursor != "3" || stderrFollowCursor != "4" {
		t.Fatalf("follow cursors stdout=%q stderr=%q", stdoutFollowCursor, stderrFollowCursor)
	}
}

func TestWorkspaceExecReconnectsUntilTerminal(t *testing.T) {
	oldReconnectDelay := runEventReconnectDelay
	runEventReconnectDelay = time.Millisecond
	t.Cleanup(func() {
		runEventReconnectDelay = oldReconnectDelay
	})
	stdoutFollowCalls := 0
	stderrFollowCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/workspaces/workspace-1/execs":
			_ = json.NewEncoder(w).Encode(api.WorkspaceExecEnvelope{Exec: api.WorkspaceExecResponse{
				ID:          "exec-1",
				WorkspaceID: "workspace-1",
				State:       "running",
			}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stdin/close":
			_ = json.NewEncoder(w).Encode(api.WorkspaceExecEnvelope{Exec: api.WorkspaceExecResponse{
				ID:          "exec-1",
				WorkspaceID: "workspace-1",
				State:       "running",
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stdout" && r.URL.Query().Get("follow") == "1":
			stdoutFollowCalls++
			if stdoutFollowCalls == 1 {
				w.Header().Set("content-type", "text/event-stream")
				return
			}
			writeWorkspaceExecTestTerminal(t, w, "stdout", "exited", 0)
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stderr" && r.URL.Query().Get("follow") == "1":
			stderrFollowCalls++
			if stderrFollowCalls == 1 {
				w.Header().Set("content-type", "text/event-stream")
				return
			}
			writeWorkspaceExecTestTerminal(t, w, "stderr", "exited", 0)
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stdout":
			_ = json.NewEncoder(w).Encode(api.ListWorkspaceExecStreamChunksResponse{})
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stderr":
			_ = json.NewEncoder(w).Encode(api.ListWorkspaceExecStreamChunksResponse{})
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1":
			code := int32(0)
			_ = json.NewEncoder(w).Encode(api.WorkspaceExecEnvelope{Exec: api.WorkspaceExecResponse{
				ID:          "exec-1",
				WorkspaceID: "workspace-1",
				State:       "exited",
				ExitCode:    &code,
			}})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"workspace", "exec", "workspace-1", "--", "true"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if stdoutFollowCalls == 0 || stderrFollowCalls == 0 {
		t.Fatalf("follow calls stdout=%d stderr=%d", stdoutFollowCalls, stderrFollowCalls)
	}
	if stdoutFollowCalls+stderrFollowCalls < 3 {
		t.Fatalf("expected at least one stream reconnect, stdout=%d stderr=%d", stdoutFollowCalls, stderrFollowCalls)
	}
}

func TestWorkspaceExecLogsFollowReturnsStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stdout" && r.URL.Query().Get("follow") == "":
			_ = json.NewEncoder(w).Encode(api.ListWorkspaceExecStreamChunksResponse{})
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stderr" && r.URL.Query().Get("follow") == "":
			_ = json.NewEncoder(w).Encode(api.ListWorkspaceExecStreamChunksResponse{})
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stdout" && r.URL.Query().Get("follow") == "1":
			writeWorkspaceExecTestStreamError(t, w, "workspace_stream_cursor_expired", "cursor expired", 0)
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/execs/exec-1/stderr" && r.URL.Query().Get("follow") == "1":
			writeWorkspaceExecTestTerminal(t, w, "stderr", "exited", 0)
		default:
			t.Fatalf("%s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	cmd := newRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"workspace", "exec", "logs", "workspace-1", "exec-1", "--follow"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "workspace_stream_cursor_expired: cursor expired") {
		t.Fatalf("err = %v", err)
	}
}

func TestConnectWorkspacePtyContinuesOutputAfterStdinEOF(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/workspaces/workspace-1/pty/pty-1/output" && r.URL.Query().Get("follow") == "1":
			writeWorkspacePtyTestStream(t, w, []byte("ready\n"))
		case r.Method == http.MethodPost && r.URL.Path == "/api/workspaces/workspace-1/pty/pty-1/close":
			_ = json.NewEncoder(w).Encode(api.WorkspacePtyEnvelope{Pty: api.WorkspacePtyResponse{
				ID:          "pty-1",
				WorkspaceID: "workspace-1",
				State:       "closed",
			}})
		default:
			t.Fatalf("%s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")
	control, err := controlClient(nil)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := newRootCommand()
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	if err := connectWorkspacePty(cmd, control, "workspace-1", "pty-1", 0, 0, client.WorkspaceScopeOptions{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "ready\n" {
		t.Fatalf("output = %q", out.String())
	}
}

func writeWorkspaceExecTestStream(t *testing.T, w http.ResponseWriter, stream string, data []byte) {
	t.Helper()
	w.Header().Set("content-type", "text/event-stream")
	chunk := api.WorkspaceExecStreamChunkResponse{
		ID:          "chunk-1",
		Stream:      stream,
		OffsetStart: 0,
		OffsetEnd:   int64(len(data)),
		Data:        data,
	}
	payload, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(w, "id: %d\nevent: workspace_stream_chunk\ndata: %s\n\n", len(data), payload)
	terminal := api.WorkspaceStreamTerminalResponse{
		ResourceKind: "workspace_exec",
		ResourceID:   "exec-1",
		Stream:       stream,
		State:        "exited",
		Cursor:       int64(len(data)),
	}
	payload, err = json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(w, "id: %d\nevent: workspace_stream_terminal\ndata: %s\n\n", len(data), payload)
}

func writeWorkspacePtyTestStream(t *testing.T, w http.ResponseWriter, data []byte) {
	t.Helper()
	w.Header().Set("content-type", "text/event-stream")
	chunk := api.WorkspacePtyStreamChunkResponse{
		ID:          "pty-chunk-1",
		Stream:      "output",
		OffsetStart: 0,
		OffsetEnd:   int64(len(data)),
		Data:        data,
	}
	payload, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(w, "id: %d\nevent: workspace_stream_chunk\ndata: %s\n\n", len(data), payload)
	terminal := api.WorkspaceStreamTerminalResponse{
		ResourceKind: "workspace_pty",
		ResourceID:   "pty-1",
		Stream:       "output",
		State:        "closed",
		Cursor:       int64(len(data)),
	}
	payload, err = json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(w, "id: %d\nevent: workspace_stream_terminal\ndata: %s\n\n", len(data), payload)
}

func workspaceExecTestListResponse(r *http.Request, stream string, data []byte) api.ListWorkspaceExecStreamChunksResponse {
	cursor := r.URL.Query().Get("cursor")
	if cursor == fmt.Sprintf("%d", len(data)) {
		return api.ListWorkspaceExecStreamChunksResponse{}
	}
	return api.ListWorkspaceExecStreamChunksResponse{Chunks: []api.WorkspaceExecStreamChunkResponse{{
		ID:          stream + "-chunk-1",
		Stream:      stream,
		OffsetStart: 0,
		OffsetEnd:   int64(len(data)),
		Data:        data,
	}}}
}

func writeWorkspaceExecTestStreamError(t *testing.T, w http.ResponseWriter, code string, message string, cursor int64) {
	t.Helper()
	w.Header().Set("content-type", "text/event-stream")
	streamErr := api.WorkspaceStreamErrorResponse{Code: code, Message: message, Cursor: cursor}
	payload, err := json.Marshal(streamErr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(w, "id: %d\nevent: workspace_stream_error\ndata: %s\n\n", cursor, payload)
}

func writeWorkspaceExecTestTerminal(t *testing.T, w http.ResponseWriter, stream string, state string, cursor int64) {
	t.Helper()
	w.Header().Set("content-type", "text/event-stream")
	terminal := api.WorkspaceStreamTerminalResponse{
		ResourceKind: "workspace_exec",
		ResourceID:   "exec-1",
		Stream:       stream,
		State:        state,
		Cursor:       cursor,
	}
	payload, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(w, "id: %d\nevent: workspace_stream_terminal\ndata: %s\n\n", cursor, payload)
}
