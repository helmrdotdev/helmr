package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// This qualification runs the real TS runtime and native provider children against
// real Postgres-backed HTTP handlers. Worker authentication, placement and capture
// are fixtures: this does not implement or qualify a local production executor.
func TestSessionNativeLocalPostgres(t *testing.T) {
	if os.Getenv("HELMR_NATIVE_SESSION_TEST") != "1" {
		t.Skip("explicit native SDK qualification")
	}
	for _, binary := range []string{"node", "npm", "bun", "initdb", "pg_ctl", "postgres"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("HELMR_SKIP_POSTGRES_TESTS") == "1" {
		t.Fatal("native qualification requires Postgres")
	}
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			f := newActorCheckpointFixtureWithInput(t, nil)
			principal := auth.Actor{OrgID: f.OrgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
				ProjectID: f.ProjectID.String(), EnvironmentID: f.EnvironmentID.String(),
				Permissions: []auth.Permission{auth.PermissionSessionsSend, auth.PermissionSessionsRead, auth.PermissionSessionsClose}}
			r := chi.NewRouter()
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					ctx := context.WithValue(r.Context(), actorContextKey{}, principal)
					ctx = context.WithValue(ctx, workerContextKey{}, f.worker)
					next.ServeHTTP(w, r.WithContext(ctx))
				})
			})
			r.Get("/config", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{"sessionId": f.sessionID.String(), "runId": f.runID.String(),
					"deploymentId": f.DeploymentID.String(), "workspaceId": f.workspaceID.String(), "baseWorkspaceVersionId": f.rootID.String(),
					"runGeneration": f.claim.actor.RunGeneration, "lease": f.fence(), "provider": provider})
			})
			r.Post("/v1/sessions/{sessionID}/send", f.server.sendSessionHTTP)
			r.Post("/v1/sessions/{sessionID}/enqueue", f.server.enqueueSessionHTTP)
			r.Post("/v1/sessions/{sessionID}/close", f.server.closeSessionHTTP)
			r.Get("/v1/sessions/{sessionID}/events", f.server.readSessionEventsHTTP)
			r.Get("/v1/sessions/{sessionID}/turns/{turnID}", f.server.getSessionTurnHTTP)
			r.Post("/v1/sessions/{sessionID}/turns/{turnID}/messages", f.server.sendSessionMessageHTTP)
			r.Post("/worker/receive", f.server.workerCreateRunWait)
			r.Post("/worker/ready", f.server.workerTurnMessagesReady)
			r.Post("/worker/claim", f.server.workerClaimTurnMessage)
			r.Post("/worker/handled", f.server.workerCompleteTurnMessage)
			r.Post("/worker/output", f.server.workerWriteTurnOutput)
			r.Post("/worker/settling", f.server.workerBeginTurnSettlement)
			// The existing capture fixture publishes marker bytes. Native conversation
			// files persist on the local filesystem, not through this Workspace capture.
			capture := f.capture(t, "local qualification capture fixture")
			r.Post("/worker/settle", func(w http.ResponseWriter, r *http.Request) {
				var req workerapi.CommitActorTurnRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				var base uuid.UUID
				if err := f.Pool.QueryRow(r.Context(), `SELECT base_workspace_version_id FROM workspace_leases WHERE owner_run_lease_id=$1`, f.claim.runLease.ID).Scan(&base); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
				req.BaseWorkspaceVersionID = base.String()
				req.Tree = capture.Tree
				if base == f.rootID {
					req.Artifact = &capture.Artifact
				}
				parsed, err := parseActorTurnCommitRequest(req)
				if err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				response, err := f.server.commitActorTurn(r.Context(), f.worker, req, parsed)
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
				writeJSON(w, http.StatusOK, response)
			})
			server := httptest.NewServer(r)
			defer server.Close()
			root, err := filepath.Abs("../..")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			bundleDir, err := os.MkdirTemp(filepath.Join(root, "dev/workflows/probes"), ".local-runtime-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(bundleDir)
			bundle := filepath.Join(bundleDir, "probe.mjs")
			build := exec.CommandContext(ctx, "bun", "build", "runtime/typescript/probes/session-native-local.test.ts", "--target=node", "--external", "@anthropic-ai/claude-agent-sdk", "--external", "@openai/codex", "--outfile", bundle)
			build.Dir = root
			if output, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build: %v\n%s", err, output)
			}
			cmd := exec.CommandContext(ctx, "node", "--test", "--test-timeout=90000", bundle)
			cmd.Dir = root
			// Do not forward operator provider credentials or configuration to children.
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "HELMR_LOCAL_BRIDGE=" + server.URL}
			output, err := cmd.CombinedOutput()
			t.Log(string(output))
			if err != nil {
				t.Fatalf("native runtime qualification: %v", err)
			}
			var completed, handled, rejected int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM session_turns WHERE session_id=$1 AND status='completed'`, f.sessionID).Scan(&completed); err != nil {
				t.Fatal(err)
			}
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE status='handled'),count(*) FILTER (WHERE status='rejected') FROM session_messages WHERE session_id=$1`, f.sessionID).Scan(&handled, &rejected); err != nil {
				t.Fatal(err)
			}
			if completed != 2 || handled != 2 || rejected != 2 {
				t.Fatalf("completed=%d handled=%d rejected=%d", completed, handled, rejected)
			}
		})
	}
}
