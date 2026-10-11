package controlplane

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

// workerClaimsRace serves real host credential issue and Worker authentication. Each
// raced route commits its transition once, after authentication accepted the
// request and before the handler takes its authority locks.
type workerClaimsRace struct {
	client          *workerclient.Client
	mu              sync.Mutex
	hostCredentials int
	statuses        map[string][]int
	bodies          map[string][][]byte
}

func newWorkerClaimsRace(t *testing.T, f agenttest.Fixture, server *Server, handlers map[string]http.HandlerFunc, races map[string]func(context.Context) error) *workerClaimsRace {
	t.Helper()
	hostSecret := seedHostSecret(t, f.Pool, f.Worker)
	server.hostAuth = testHostAuthConfig(t)
	server.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	race := &workerClaimsRace{statuses: map[string][]int{}, bodies: map[string][][]byte{}}
	protected := map[string]http.Handler{}
	for path, handler := range handlers {
		pending := races[path]
		protected[path] = server.requireWorker(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if pending != nil {
				transition := pending
				pending = nil
				if err := transition(r.Context()); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			handler(w, r)
		}))
	}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		race.mu.Lock()
		defer race.mu.Unlock()
		if r.URL.Path == "/worker/v1/instance/credential" {
			race.hostCredentials++
			server.workerIssueHostCredential(w, r)
			return
		}
		handler, ok := protected[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		race.bodies[r.URL.Path] = append(race.bodies[r.URL.Path], body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		race.statuses[r.URL.Path] = append(race.statuses[r.URL.Path], response.Code)
		for key, values := range response.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
	}))
	t.Cleanup(httpServer.Close)
	race.client = hostSecret.client(t, httpServer.URL)
	return race
}

// requireReplayed checks that the raced request was refused for authentication
// and then succeeded with a byte-identical body under the one re-minted credential.
func (r *workerClaimsRace) requireReplayed(t *testing.T, path string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	statuses := r.statuses[path]
	if r.hostCredentials != 2 || len(statuses) != 2 || statuses[0] != http.StatusUnauthorized || (statuses[1] != http.StatusOK && statuses[1] != http.StatusNoContent) {
		t.Fatalf("%s: host credential requests=%d statuses=%v", path, r.hostCredentials, statuses)
	}
	if !bytes.Equal(r.bodies[path][0], r.bodies[path][1]) {
		t.Fatalf("%s: replay changed the request", path)
	}
}

func drainWorkerHost(f agenttest.Fixture) func(context.Context) error {
	return func(ctx context.Context) error {
		var claim int64
		if err := f.Pool.QueryRow(ctx, `SELECT claim_version FROM worker_hosts WHERE id=$1`, f.Worker).Scan(&claim); err != nil {
			return err
		}
		_, err := db.New(f.Pool).DrainWorkerHost(ctx, db.DrainWorkerHostParams{DrainReason: "shutdown",
			ID: pgvalue.UUID(f.Worker), WorkerGroupID: pgvalue.UUID(f.Group),
			ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, ExpectedClaimVersion: claim,
		})
		return err
	}
}

func runningComputerCommand(t *testing.T, f agenttest.Fixture) (uuid.UUID, uuid.UUID, int64) {
	t.Helper()
	var org, project uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&org, &project); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET observed_at=clock_timestamp() WHERE id=$1`, f.Worker)
	created, err := command.Create(t.Context(), f.Pool, command.CreateRequest{OrgID: org, ProjectID: project, EnvironmentID: f.Environment, ComputerID: f.Computer, Creator: command.Creator{SubjectType: string(auth.PrincipalKindSession), SubjectID: f.User.String()}, Argv: []string{"true"}, IdempotencyKey: "race"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := command.Claim(t.Context(), f.Pool, workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}, command.ClaimRequest{EnvironmentID: f.Environment, InstanceID: f.Computer, WriterGeneration: 1})
	if err != nil || claimed.Start == nil {
		t.Fatalf("claim: %v", err)
	}
	return pgvalue.MustUUIDValue(created.ID), f.Computer, 1
}

func TestComputerCommandOperationsReauthenticateAcrossDrain(t *testing.T) {
	for _, test := range []struct {
		name, path string
		stopping   bool
		handler    func(*Server) http.HandlerFunc
		call       func(*testing.T, agenttest.Fixture, *workerclient.Client, uuid.UUID, uuid.UUID, int64)
	}{
		{name: "completion", path: "/worker/v1/computer-commands/complete", handler: func(s *Server) http.HandlerFunc { return s.workerCompleteComputerCommand },
			call: func(t *testing.T, f agenttest.Fixture, client *workerclient.Client, command, instance uuid.UUID, generation int64) {
				code := int32(0)
				if err := client.CompleteComputerCommand(t.Context(), workerapi.ComputerCommandCompleteRequest{EnvironmentID: f.Environment.String(), CommandID: command.String(), ComputerInstanceID: instance.String(), WriterGeneration: generation, Outcome: "exited", ExitCode: &code, Stdout: workerapi.CommandOutputBoundary{ThroughSequence: 1, Complete: true}, Stderr: workerapi.CommandOutputBoundary{ThroughSequence: 1, Complete: true}}); err != nil {
					t.Fatalf("completion across drain: %v", err)
				}
				var completed bool
				if err := f.Pool.QueryRow(t.Context(), `SELECT c.status='exited' AND c.exit_code=0 AND h.status='draining' FROM computer_commands c JOIN worker_hosts h ON h.id=$2 WHERE c.id=$1`, command, f.Worker).Scan(&completed); err != nil || !completed {
					t.Fatalf("completion was not durable on the draining host: %v %v", completed, err)
				}
			}},
		{name: "claim", path: "/worker/v1/computer-commands/claim", handler: func(s *Server) http.HandlerFunc { return s.workerClaimComputerCommand },
			call: func(t *testing.T, f agenttest.Fixture, client *workerclient.Client, command, instance uuid.UUID, generation int64) {
				response, err := client.ClaimComputerCommand(t.Context(), workerapi.ComputerCommandClaimRequest{EnvironmentID: f.Environment.String(), ComputerInstanceID: instance.String(), WriterGeneration: generation})
				if err != nil {
					t.Fatalf("claim across drain: %v", err)
				}
				if response.Command == nil || response.Command.CommandID != command.String() || response.Command.WriterGeneration != generation {
					t.Fatalf("claim replay response: %+v", response)
				}
			}},
		{name: "cancellation", path: "/worker/v1/computer-commands/claim", stopping: true, handler: func(s *Server) http.HandlerFunc { return s.workerClaimComputerCommand },
			call: func(t *testing.T, f agenttest.Fixture, client *workerclient.Client, command, instance uuid.UUID, generation int64) {
				response, err := client.ClaimComputerCommand(t.Context(), workerapi.ComputerCommandClaimRequest{EnvironmentID: f.Environment.String(), ComputerInstanceID: instance.String(), WriterGeneration: generation})
				if err != nil {
					t.Fatalf("cancellation claim across drain: %v", err)
				}
				if response.Cancellation == nil || response.Cancellation.CommandID != command.String() || response.Cancellation.WriterGeneration != generation {
					t.Fatalf("cancellation replay response: %+v", response)
				}
			}},
		{name: "log append", path: "/worker/v1/computer-commands/logs/append", handler: func(s *Server) http.HandlerFunc { return s.workerAppendCommandLogs },
			call: func(t *testing.T, f agenttest.Fixture, client *workerclient.Client, command, instance uuid.UUID, generation int64) {
				if _, err := client.AppendCommandLog(t.Context(), workerapi.CommandLogAppendRequest{EnvironmentID: f.Environment.String(), CommandID: command.String(), ComputerInstanceID: instance.String(), WriterGeneration: generation, Stream: workerapi.LogStreamStdout, Kind: "data", ObservedSeq: 1, ThroughSequence: 1, ObservedAt: time.Now().UTC().Truncate(time.Millisecond), Content: []byte("output")}); err != nil {
					t.Fatalf("log append across drain: %v", err)
				}
				var accepted int
				if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM telemetry_outbox WHERE command_id=$1`, command).Scan(&accepted); err != nil || accepted != 1 {
					t.Fatalf("log append accepted=%d err=%v", accepted, err)
				}
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := agenttest.New(t)
			command, instance, generation := runningComputerCommand(t, f)
			if test.stopping {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='stopping',cancel_requested_at=now() WHERE id=$1`, command)
			}
			server := &Server{db: db.New(f.Pool), tx: f.Pool, diagnosticDB: f.Pool, diagnosticBounds: completeServerConfig(t).DiagnosticBounds, secretDelivery: emptyTestSecretDelivery{}}
			race := newWorkerClaimsRace(t, f, server, map[string]http.HandlerFunc{test.path: test.handler(server)}, map[string]func(context.Context) error{test.path: drainWorkerHost(f)})
			test.call(t, f, race.client, command, instance, generation)
			race.requireReplayed(t, test.path)
		})
	}
}

func TestSecretProxyReauthenticatesAcrossDrain(t *testing.T) {
	for _, resolve := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepare", true: "resolve"}[resolve], func(t *testing.T) {
			f := newSnapshotFixture(t, 1, true)
			path, handler := "/worker/v1/run/secret-proxy/prepare", f.server.workerPrepareSecretProxy
			if resolve {
				path, handler = "/worker/v1/run/secret-proxy/resolve", f.server.workerResolveSecretProxy
			}
			race := newWorkerClaimsRace(t, f.fixture, f.server, map[string]http.HandlerFunc{path: handler}, map[string]func(context.Context) error{path: drainWorkerHost(f.fixture)})
			if resolve {
				resolution, err := race.client.ResolveSecretProxy(t.Context(), workerapi.SecretProxyRequest{ComputerInstanceID: f.instance.String(), Origin: "https://example.com", Placeholders: f.markers})
				if err != nil {
					t.Fatalf("resolve across drain: %v", err)
				}
				if string(resolution.Values[f.markers[0]]) != "old-a" {
					t.Fatal("replayed resolution returned different material")
				}
			} else {
				preparation, err := race.client.PrepareSecretProxy(t.Context(), workerapi.SecretProxyRequest{ComputerInstanceID: f.instance.String()})
				if err != nil {
					t.Fatalf("prepare across drain: %v", err)
				}
				if len(preparation.Certificate) == 0 || len(preparation.PrivateKey) == 0 {
					t.Fatal("replayed preparation returned no proxy leaf")
				}
				clear(preparation.PrivateKey)
			}
			race.requireReplayed(t, path)
		})
	}
}

func TestSecretProxySeparatesStaleClaimsFromRevocation(t *testing.T) {
	for _, test := range []struct {
		name, sql string
		status    int
	}{
		{"host claims", `UPDATE worker_hosts SET claim_version=claim_version+1 WHERE id=$1`, http.StatusUnauthorized},
		{"group claims", `UPDATE worker_groups SET claim_version=claim_version+1 WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, http.StatusUnauthorized},
		{"new epoch", `UPDATE worker_hosts SET current_epoch=2,claim_version=claim_version+1 WHERE id=$1`, http.StatusConflict},
		{"lost", `UPDATE worker_hosts SET status='lost',lost_at=now(),claim_version=claim_version+1 WHERE id=$1`, http.StatusConflict},
		{"disabled group", `UPDATE worker_groups SET status='disabled',claim_version=claim_version+1 WHERE id=(SELECT worker_group_id FROM worker_hosts WHERE id=$1)`, http.StatusConflict},
		{"revoked Secret", "", http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSnapshotFixture(t, 1, true)
			if test.sql == "" {
				if _, err := f.store.Revoke(t.Context(), f.fixture.Environment, pgvalue.MustUUIDValue(f.secrets[0]), "revoke"); err != nil {
					t.Fatal(err)
				}
			} else {
				dbtest.MustExec(t, t.Context(), f.fixture.Pool, test.sql, f.fixture.Worker)
			}
			for _, resolve := range []bool{false, true} {
				status := test.status

				response := f.invoke(t.Context(), resolve)
				if response.Code != status {
					t.Fatalf("resolve=%v status=%d want=%d body=%s", resolve, response.Code, status, response.Body.String())
				}
				if status == http.StatusConflict && !bytes.Contains(response.Body.Bytes(), []byte(secret.ErrDeliveryUnavailable.Error())) {
					t.Fatalf("resolve=%v conflict body=%s", resolve, response.Body.String())
				}
			}
		})
	}
}
