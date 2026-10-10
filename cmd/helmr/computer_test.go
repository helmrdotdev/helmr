package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestComputerAddressRequiresExactlyOneAddress(t *testing.T) {
	for _, test := range []struct {
		name    string
		address computerAddressFlags
		wantErr bool
	}{
		{name: "id", address: computerAddressFlags{id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"}},
		{name: "key", address: computerAddressFlags{key: "repository"}},
		{name: "missing", wantErr: true},
		{name: "both", address: computerAddressFlags{id: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32", key: "repository"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.address.validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("validate() error = %v, wantErr = %v", err, test.wantErr)
			}
		})
	}
}

func TestComputerPairSplitsOnlyFirstEquals(t *testing.T) {
	name, value, err := computerPair("TOKEN=a=b", "--env")
	if err != nil {
		t.Fatal(err)
	}
	if name != "TOKEN" || value != "a=b" {
		t.Fatalf("pair = %q, %q", name, value)
	}
}

func TestComputerCommandCommandReturnsAdmissionReceipt(t *testing.T) {
	const computerID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
	const commandID = "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc36"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("authorization") != "Bearer test-key" {
			t.Errorf("authorization missing")
		}
		w.Header().Set("content-type", "application/json")
		if r.Method == "GET" && r.URL.Path == "/v1/computers/"+computerID {
			_ = json.NewEncoder(w).Encode(api.ComputerSnapshot{ID: computerID})
			return
		}
		if r.Method != "POST" || r.URL.Path != "/v1/computers/"+computerID+"/exec" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(api.CommandReceipt{CommandID: commandID})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "test-key")
	for _, jsonOutput := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		command := newRootCommand()
		command.SetOut(&stdout)
		command.SetErr(&stderr)
		args := []string{"computer", "exec", "--id", computerID, "--idempotency-key", "exec-1"}
		if jsonOutput {
			args = append(args, "--json")
		}
		args = append(args, "--", "false")
		command.SetArgs(args)
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		want := commandID
		if jsonOutput {
			want = `{"command_id":"` + commandID + `"}`
		}
		if strings.TrimSpace(stdout.String()) != want || stderr.Len() != 0 {
			t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
		}
	}
	if requests != 4 {
		t.Fatalf("requests=%d", requests)
	}
}

func TestComputerCreateSecretsFileUsesNestedAPI(t *testing.T) {
	var received api.CreateComputerRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/computer-definitions/reviewer/computers" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(api.ComputerSnapshot{ID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"})
	}))
	defer server.Close()
	t.Setenv(helmrAPIURLEnv, server.URL)
	t.Setenv(helmrAPIKeyEnv, "synthetic-key")
	path := filepath.Join(t.TempDir(), "bindings.json")
	if err := os.WriteFile(path, []byte(`[{"secretId":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33","env":{"name":"GH_TOKEN","mode":"protected","allowedOrigins":["https://api.github.com"]}},{"secretId":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc34","file":{"path":"/run/secrets/key"}}]`), 0600); err != nil {
		t.Fatal(err)
	}
	command := newRootCommand()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"computer", "create", "reviewer", "--secrets-file", path})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(received.Secrets) != 2 || received.Secrets[0].SecretID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33" || received.Secrets[0].Env.Mode != "protected" || received.Secrets[0].Env.AllowedOrigins[0] != "https://api.github.com" || received.Secrets[1].File.Path != "/run/secrets/key" {
		t.Fatal("CLI changed binding metadata")
	}
}

func TestComputerSecretsFileDecoderCause(t *testing.T) {
	t.Setenv(helmrAPIURLEnv, "http://127.0.0.1:1")
	t.Setenv(helmrAPIKeyEnv, "synthetic-key")
	path := filepath.Join(t.TempDir(), "bindings.json")
	if err := os.WriteFile(path, []byte(`[{"secretId":"019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33","env":{"name":"TOKEN","mode":"protected","allowed_origins":["https://example.com"]}}]`), 0600); err != nil {
		t.Fatal(err)
	}
	command := newRootCommand()
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	command.SetArgs([]string{"computer", "create", "reviewer", "--secrets-file", path})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), `unknown field "allowed_origins"`) || !strings.Contains(err.Error(), "allowedOrigins") {
		t.Fatalf("unhelpful decoder failure: %v", err)
	}
}
