package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
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
		for _, scenario := range []string{"interaction", "interruption"} {
			t.Run(provider+"/"+scenario, func(t *testing.T) {
				f := newActorCheckpointFixtureWithInput(t, nil)
				principal := auth.Actor{OrgID: f.OrgID, Kind: auth.ActorKindAPIKey, Role: auth.RoleDeveloper,
					ProjectID: f.ProjectID.String(), EnvironmentID: f.EnvironmentID.String(),
					Permissions: []auth.Permission{auth.PermissionSessionsSend, auth.PermissionSessionsRead, auth.PermissionSessionsClose, auth.PermissionSessionsInterrupt, auth.PermissionSessionsResume}}
				r := chi.NewRouter()
				r.Use(func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						ctx := context.WithValue(r.Context(), actorContextKey{}, principal)
						ctx = context.WithValue(ctx, workerContextKey{}, f.worker)
						next.ServeHTTP(w, r.WithContext(ctx))
					})
				})
				config := func() map[string]any {
					return map[string]any{
						"sessionId": f.sessionID.String(), "runId": f.runID.String(),
						"deploymentId": f.DeploymentID.String(), "workspaceId": f.workspaceID.String(),
						"baseWorkspaceVersionId": pgvalue.UUIDString(f.claim.workspaceLease.BaseWorkspaceVersionID),
						"startInputSequence":     f.claim.actor.CommittedInputSequence,
						"inputHighWatermark":     max(int64(1), f.claim.actor.NextInputSequence-1),
						"runGeneration":          f.claim.actor.RunGeneration, "lease": f.fence(), "provider": provider,
					}
				}
				r.Get("/config", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, config()) })
				// Native runtime termination is observed by Node before these test-only
				// commands. Worker physical quiescence/capture remain explicit fixtures.
				type command struct {
					kind     string
					outcome  workerapi.ActorOutcome
					response chan any
				}
				commands := make(chan command)
				r.Post("/fixture/{operation}", func(w http.ResponseWriter, r *http.Request) {
					cmd := command{kind: chi.URLParam(r, "operation"), response: make(chan any, 1)}
					if cmd.kind != "finalize" && cmd.kind != "start" {
						http.NotFound(w, r)
						return
					}
					if cmd.kind == "finalize" {
						if err := json.NewDecoder(r.Body).Decode(&cmd.outcome); err != nil {
							http.Error(w, err.Error(), 400)
							return
						}
					}
					select {
					case commands <- cmd:
					case <-r.Context().Done():
						return
					}
					select {
					case result := <-cmd.response:
						writeJSON(w, http.StatusOK, result)
					case <-r.Context().Done():
						return
					}
				})
				r.Post("/v1/sessions/{sessionID}/send", f.server.sendSessionHTTP)
				r.Post("/v1/sessions/{sessionID}/enqueue", f.server.enqueueSessionHTTP)
				r.Get("/v1/sessions/{sessionID}", f.server.getSessionHTTP)
				r.Post("/v1/sessions/{sessionID}/resume", f.server.resumeSessionHTTP)
				r.Post("/v1/sessions/{sessionID}/turns/{turnID}/interrupt", f.server.interruptSessionTurnHTTP)
				r.Post("/worker/control", f.server.workerSessionControl)
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
				r.Post("/worker/settle", func(w http.ResponseWriter, r *http.Request) {
					var req workerapi.CommitActorTurnRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						http.Error(w, err.Error(), 400)
						return
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
				defer func() { server.CloseClientConnections(); server.Close() }()
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
				probeName := "session-native-local.test.ts"
				if scenario == "interruption" {
					probeName = "session-native-interruption.test.ts"
				}
				build := exec.CommandContext(ctx, "bun", "build", "runtime/typescript/probes/"+probeName, "--target=node", "--external", "@anthropic-ai/claude-agent-sdk", "--external", "@openai/codex", "--outfile", bundle)
				build.Dir = root
				if output, err := build.CombinedOutput(); err != nil {
					t.Fatalf("build: %v\n%s", err, output)
				}
				cmd := exec.CommandContext(ctx, "node", "--test", "--test-timeout=90000", bundle)
				cmd.Dir = root
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
				cmd.WaitDelay = 5 * time.Second
				// Do not forward operator provider credentials or configuration to children.
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "HELMR_LOCAL_BRIDGE=" + server.URL}
				var output bytes.Buffer
				cmd.Stdout = &output
				cmd.Stderr = &output
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- cmd.Wait() }()
				// All fixture transitions stay on the testing goroutine, so helper
				// failures still unwind cleanup instead of stranding an HTTP handler.
				running := true
				for running {
					select {
					case err = <-done:
						running = false
					case command := <-commands:
						switch command.kind {
						case "finalize":
							o := command.outcome
							if o.Interrupted == nil || o.RunGeneration != f.claim.actor.RunGeneration {
								t.Fatal("unexpected runtime interruption outcome")
							}
							hold, e := uuid.Parse(o.Interrupted.HoldID)
							if e != nil {
								t.Fatal(e)
							}
							if o.Interrupted.TurnID == nil {
								t.Fatal("expected interrupted Turn")
							}
							turn, e := uuid.Parse(*o.Interrupted.TurnID)
							if e != nil {
								t.Fatal(e)
							}
							req := interruptedCompletionRequest(t, f, hold, &turn)
							req.Outcome = o
							parsed, e := parseActorCompletionRequest(req)
							if e != nil {
								t.Fatal(e)
							}
							if e = f.server.completeActor(t.Context(), f.worker, req, parsed); e != nil {
								t.Fatal(e)
							}
							f.workerCall(t, f.server.workerStopWorkspaceMount, workerapi.WorkspaceMountStopRequest{
								OrgID: f.OrgID.String(), WorkspaceMountID: pgvalue.UUIDString(f.claim.workspaceMount.ID),
								CleanupProof: workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupSessionClosed, CompletedAt: time.Now()},
							}, nil)
							command.response <- map[string]any{}
						case "start":
							old := f.runID
							if e := f.Pool.QueryRow(t.Context(), `SELECT current_run_id FROM sessions WHERE id=$1`, f.sessionID).Scan(&f.runID); e != nil {
								t.Fatal(e)
							}
							if old == f.runID {
								t.Fatal("resume did not create a new Run")
							}
							f.placeAndStart(t)
							command.response <- config()
						}
					}
				}
				t.Log(output.String())
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
				wantCompleted, wantHandled, wantRejected := 2, 2, 2
				if scenario == "interruption" {
					wantCompleted, wantHandled, wantRejected = 1, 0, 1
				}
				if completed != wantCompleted || handled != wantHandled || rejected != wantRejected {
					t.Fatalf("completed=%d handled=%d rejected=%d", completed, handled, rejected)
				}
			})
		}
	}
}
