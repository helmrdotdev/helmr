package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestWorkspaceAddressRequiresExactlyOneAddress(t *testing.T) {
	for _, test := range []struct {
		name    string
		address workspaceAddressFlags
		wantErr bool
	}{
		{name: "id", address: workspaceAddressFlags{id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"}},
		{name: "key", address: workspaceAddressFlags{key: "repository"}},
		{name: "missing", wantErr: true},
		{name: "both", address: workspaceAddressFlags{id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32", key: "repository"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.address.validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("validate() error = %v, wantErr = %v", err, test.wantErr)
			}
		})
	}
}

func TestWorkspacePairSplitsOnlyFirstEquals(t *testing.T) {
	name, value, err := workspacePair("TOKEN=a=b", "--env")
	if err != nil {
		t.Fatal(err)
	}
	if name != "TOKEN" || value != "a=b" {
		t.Fatalf("pair = %q, %q", name, value)
	}
}

func TestWorkspaceExecCommandPreservesPollingOutputJSONAndExitCode(t *testing.T) {
	const workspaceID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
	const processID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("authorization") != "Bearer test-key" {
			t.Fatalf("authorization = %q", r.Header.Get("authorization"))
		}
		switch requests {
		case 1:
			if r.Method != http.MethodGet || r.URL.Path != "/v1/workspaces/"+workspaceID {
				t.Fatalf("request = %s %s", r.Method, r.URL.Path)
			}
			w.Header().Set("content-type", "application/json")
			_ = json.NewEncoder(w).Encode(api.WorkspaceSnapshot{ID: workspaceID})
		case 2:
			if r.Method != http.MethodPost || r.URL.Path != "/v1/workspaces/"+workspaceID+"/exec" {
				t.Fatalf("request = %s %s", r.Method, r.URL.Path)
			}
			writeWorkspaceExecCommandResponse(t, w, http.StatusAccepted, api.WorkspaceExecProcess{
				ProcessID: processID, Status: api.WorkspaceExecProcessStatusPending,
			})
		case 3:
			if r.Method != http.MethodGet || r.URL.Path != "/v1/workspaces/"+workspaceID+"/exec/"+processID {
				t.Fatalf("request = %s %s", r.Method, r.URL.Path)
			}
			exitCode := int32(17)
			stdout := base64.StdEncoding.EncodeToString([]byte("stdout\n"))
			stderr := base64.StdEncoding.EncodeToString([]byte("stderr\n"))
			writeWorkspaceExecCommandResponse(t, w, http.StatusOK, api.WorkspaceExecProcess{
				ProcessID: processID, Status: api.WorkspaceExecProcessStatusExited,
				ExitCode: &exitCode, StdoutBase64: &stdout, StderrBase64: &stderr,
			})
		case 4:
			if r.Method != http.MethodGet || r.URL.Path != "/v1/workspaces/"+workspaceID {
				t.Fatalf("request = %s %s", r.Method, r.URL.Path)
			}
			w.Header().Set("content-type", "application/json")
			_ = json.NewEncoder(w).Encode(api.WorkspaceSnapshot{ID: workspaceID})
		case 5:
			if r.Method != http.MethodPost || r.URL.Path != "/v1/workspaces/"+workspaceID+"/exec" {
				t.Fatalf("request = %s %s", r.Method, r.URL.Path)
			}
			exitCode := int32(0)
			stdout := base64.StdEncoding.EncodeToString([]byte("json\n"))
			stderr := ""
			writeWorkspaceExecCommandResponse(t, w, http.StatusOK, api.WorkspaceExecProcess{
				ProcessID: processID, Status: api.WorkspaceExecProcessStatusExited,
				ExitCode: &exitCode, StdoutBase64: &stdout, StderrBase64: &stderr,
			})
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command := newRootCommand()
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs([]string{"workspace", "exec", "--id", workspaceID, "--idempotency-key", "exec-1", "--", "false"})
	err := command.Execute()
	var exitErr exitCodeError
	if !errors.As(err, &exitErr) || exitErr.code != 17 {
		t.Fatalf("error = %#v", err)
	}
	if stdout.String() != "stdout\n" || stderr.String() != "stderr\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	command = newRootCommand()
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	command.SetArgs([]string{"workspace", "exec", "--id", workspaceID, "--idempotency-key", "exec-2", "--json", "--", "true"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout.String()) != `{"exit_code":0,"stdout_base64":"anNvbgo=","stderr_base64":""}` || stderr.Len() != 0 {
		t.Fatalf("json stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if requests != 5 {
		t.Fatalf("requests = %d", requests)
	}
}

func writeWorkspaceExecCommandResponse(t *testing.T, w http.ResponseWriter, status int, response api.WorkspaceExecProcess) {
	t.Helper()
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		t.Fatal(err)
	}
}
