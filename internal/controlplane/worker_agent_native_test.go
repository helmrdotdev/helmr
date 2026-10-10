//go:build linux || darwin

package controlplane

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

// The explicit opt-in starts a disposable privileged Docker fixture. Its native
// clients reach this real CP server through Docker Desktop's host loopback route.
// Model replies and non-MCP authored operations remain fixture-owned; this test
// qualifies the managed MCP/authority path, not complete Turn finalization.
func TestWorkerAgentNativeManagedMCP(t *testing.T) {
	if os.Getenv("HELMR_NATIVE_CP_PROOF") != "1" {
		t.Skip("requires authorized disposable privileged Docker on Docker Desktop")
	}
	f := agenttest.New(t)
	handler := newPostgresServer(t, f.Pool, func(cfg *ServerConfig) {
		cfg.WorkerHostCredentialTTL = 35 * time.Second
	})
	var exchanges, operations atomic.Int64
	var credentialsMu sync.Mutex
	var firstCredential, lastCredential string
	var cancelOnCredential atomic.Bool
	credentialObserved := make(chan struct{}, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/worker/v1/instance/credential":
			exchanges.Add(1)
			if cancelOnCredential.Load() {
				select {
				case credentialObserved <- struct{}{}:
				default:
				}
			}
		case "/worker/v1/sessions/operations":
			operations.Add(1)
			credentialsMu.Lock()
			if firstCredential == "" {
				firstCredential = r.Header.Get("Authorization")
			}
			lastCredential = r.Header.Get("Authorization")
			credentialsMu.Unlock()
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	secret := seedHostSecret(t, f.Pool, f.Worker)
	sessions := map[string]string{}
	for _, provider := range []string{"codex", "claude"} {
		id := uuid.NewV7()
		sessions[provider] = id.String()
		dbtest.MustExec(t, t.Context(), f.Pool, `
INSERT INTO sessions(history_retention_mode,environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth)
 VALUES('until_environment_deletion',$1,$2,$3,$4,$5,$2,0);
INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status)
 VALUES($1,$2,1,$5,1,'ready');`, pgx.QueryExecModeSimpleProtocol, f.Environment, id, f.Agent, f.Deployment, f.Computer)
	}
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	endpoint.Host = net.JoinHostPort("host.docker.internal", endpoint.Port())
	configuration, err := json.Marshal(struct {
		BaseURL, Environment, Computer, Worker, Service, Secret, TargetSession, ServerName string
		Certificate                                                                        []byte
		Sessions                                                                           map[string]string
	}{endpoint.String(), f.Environment.String(), f.Computer.String(), f.Worker.String(), secret.serviceID.String(), secret.secret, f.Session.String(), server.Certificate().DNSNames[0], server.Certificate().Raw, sessions})
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "cp.json")
	if err := os.WriteFile(configPath, configuration, 0600); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context) error {
		command := exec.CommandContext(ctx, filepath.Join(root, "tests/build/native-continuation.test.sh"))
		command.Dir = root
		temporary := t.TempDir()
		command.Env = append(os.Environ(), "HELMR_NATIVE_CP_CONFIG="+configPath, "TMPDIR="+temporary)
		// Bash defers traps while waiting on a foreground pipeline. Signal the
		// exclusively created process group so that pipeline also exits and Bash
		// can run its EXIT cleanup before WaitDelay expires.
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGTERM) }
		command.WaitDelay = 15 * time.Second
		output, err := command.CombinedOutput()
		t.Logf("native MCP fixture:\n%s", output)
		if command.Process != nil {
			image := "helmr-native-continuation-" + strconv.Itoa(command.Process.Pid)
			for _, args := range [][]string{{"ps", "-aq", "--filter", "ancestor=" + image}, {"image", "ls", "-q", "--filter", "reference=" + image}} {
				remaining, inspectErr := exec.Command("docker", args...).CombinedOutput()
				if inspectErr != nil || strings.TrimSpace(string(remaining)) != "" {
					t.Errorf("fixture resources remain or cannot be inspected: %s: %v", remaining, inspectErr)
				}
			}
		}
		files, inspectErr := os.ReadDir(temporary)
		if inspectErr != nil || len(files) != 0 {
			t.Errorf("fixture temporary files remain: %v: %v", files, inspectErr)
		}
		return err
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Minute)
	defer cancel()
	if err := run(ctx); err != nil {
		t.Fatal(err)
	}
	if exchanges.Load() < 2 || operations.Load() != 4 {
		t.Fatalf("credential exchanges=%d MCP operations=%d", exchanges.Load(), operations.Load())
	}
	credentialsMu.Lock()
	changed := firstCredential != "" && lastCredential != "" && firstCredential != lastCredential
	credentialsMu.Unlock()
	if !changed {
		t.Fatal("later native MCP calls did not use a renewed host credential")
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), "SELECT count(*) FROM turns WHERE environment_id=$1 AND session_id=$2", f.Environment, f.Session).Scan(&count); err != nil || count != 4 {
		t.Fatalf("native MCP accepted Turns=%d err=%v", count, err)
	}
	// Cancel once a credential exchange proves the privileged container is
	// running. The same launcher must close its group, container, image and
	// temporary directory on this path as on success.
	cancelOnCredential.Store(true)
	cancelCtx, cancelFixture := context.WithCancel(ctx)
	defer cancelFixture()
	joined := make(chan struct{})
	var cancelledActive atomic.Bool
	go func() {
		defer close(joined)
		select {
		case <-credentialObserved:
			cancelledActive.Store(true)
			cancelFixture()
		case <-cancelCtx.Done():
		}
	}()
	cancelErr := run(cancelCtx)
	cancelFixture()
	<-joined
	if !cancelledActive.Load() || cancelErr == nil || ctx.Err() != nil {
		t.Fatalf("expected active-fixture cancellation, got %v (parent=%v)", cancelErr, ctx.Err())
	}

}
