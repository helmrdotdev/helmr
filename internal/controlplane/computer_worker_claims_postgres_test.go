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

	"github.com/helmrdotdev/helmr/internal/command/commandtest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
	"github.com/jackc/pgx/v5/pgtype"
)

// workerClaimsRace serves real host credential issue and Worker authentication. Each
// raced route commits its transition once, after authentication accepted the
// request and before the handler takes its authority locks.
type workerClaimsRace struct {
	client   *workerclient.Client
	mu       sync.Mutex
	tokens   int
	statuses map[string][]int
	bodies   map[string][][]byte
}

func newWorkerClaimsRace(t *testing.T, f runtest.Fixture, server *Server, handlers map[string]http.HandlerFunc, races map[string]func(context.Context) error) *workerClaimsRace {
	t.Helper()
	credential := seedHostCredential(t, f.Pool, f.WorkerID)
	server.hostCredentials = testHostCredentials(t)
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
			race.tokens++
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
	race.client = credential.client(t, httpServer.URL)
	return race
}

// requireReplayed checks that the raced request was refused for authentication
// and then succeeded with a byte-identical body under the one re-minted credential.
func (r *workerClaimsRace) requireReplayed(t *testing.T, path string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	statuses := r.statuses[path]
	if r.tokens != 2 || len(statuses) != 2 || statuses[0] != http.StatusUnauthorized || (statuses[1] != http.StatusOK && statuses[1] != http.StatusNoContent) {
		t.Fatalf("%s: token requests=%d statuses=%v", path, r.tokens, statuses)
	}
	if !bytes.Equal(r.bodies[path][0], r.bodies[path][1]) {
		t.Fatalf("%s: replay changed the request", path)
	}
}

func drainWorkerHost(f runtest.Fixture) func(context.Context) error {
	return func(ctx context.Context) error {
		var claim int64
		if err := f.Pool.QueryRow(ctx, `SELECT claim_version FROM worker_hosts WHERE id=$1`, f.WorkerID).Scan(&claim); err != nil {
			return err
		}
		_, err := db.New(f.Pool).DrainWorkerHost(ctx, db.DrainWorkerHostParams{
			ID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID),
			ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, ExpectedClaimVersion: claim,
		})
		return err
	}
}

func runningComputerCommand(t *testing.T, f runtest.Fixture) (uuid.UUID, uuid.UUID, int64) {
	t.Helper()
	member := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, member.RunID)
	command := commandtest.Bound(t, f, member.LeaseID, "running")
	return command.ID, command.InstanceID, command.WriterGeneration
}

func TestComputerCommandOperationsReauthenticateAcrossDrain(t *testing.T) {
	for _, test := range []struct {
		name, path string
		stopping   bool
		handler    func(*Server) http.HandlerFunc
		call       func(*testing.T, runtest.Fixture, *workerclient.Client, uuid.UUID, uuid.UUID, int64)
	}{
		{name: "completion", path: "/worker/v1/run/computer-commands/complete", handler: func(s *Server) http.HandlerFunc { return s.workerCompleteComputerCommand },
			call: func(t *testing.T, f runtest.Fixture, client *workerclient.Client, command, instance uuid.UUID, generation int64) {
				code := int32(0)
				if err := client.CompleteComputerCommand(t.Context(), workerapi.ComputerCommandCompleteRequest{OrgID: f.OrgID.String(), CommandID: command.String(), ComputerInstanceID: instance.String(), WriterGeneration: generation, Outcome: "exited", ExitCode: &code}); err != nil {
					t.Fatalf("completion across drain: %v", err)
				}
				var completed bool
				if err := f.Pool.QueryRow(t.Context(), `SELECT c.status='exited' AND c.exit_code=0 AND h.status='draining' FROM computer_commands c JOIN worker_hosts h ON h.id=$2 WHERE c.id=$1`, command, f.WorkerID).Scan(&completed); err != nil || !completed {
					t.Fatalf("completion was not durable on the draining host: %v %v", completed, err)
				}
			}},
		{name: "claim", path: "/worker/v1/run/computer-commands/claim", handler: func(s *Server) http.HandlerFunc { return s.workerClaimComputerCommand },
			call: func(t *testing.T, f runtest.Fixture, client *workerclient.Client, command, instance uuid.UUID, generation int64) {
				response, err := client.ClaimComputerCommand(t.Context(), workerapi.ComputerCommandClaimRequest{OrgID: f.OrgID.String(), EnvironmentID: f.EnvironmentID.String(), ComputerInstanceID: instance.String(), WriterGeneration: generation})
				if err != nil {
					t.Fatalf("claim across drain: %v", err)
				}
				if response.Command == nil || response.Command.CommandID != command.String() || response.Command.WriterGeneration != generation {
					t.Fatalf("claim replay response: %+v", response)
				}
			}},
		{name: "cancellation", path: "/worker/v1/run/computer-commands/claim", stopping: true, handler: func(s *Server) http.HandlerFunc { return s.workerClaimComputerCommand },
			call: func(t *testing.T, f runtest.Fixture, client *workerclient.Client, command, instance uuid.UUID, generation int64) {
				response, err := client.ClaimComputerCommand(t.Context(), workerapi.ComputerCommandClaimRequest{OrgID: f.OrgID.String(), EnvironmentID: f.EnvironmentID.String(), ComputerInstanceID: instance.String(), WriterGeneration: generation})
				if err != nil {
					t.Fatalf("cancellation claim across drain: %v", err)
				}
				if response.Cancellation == nil || response.Cancellation.CommandID != command.String() || response.Cancellation.WriterGeneration != generation {
					t.Fatalf("cancellation replay response: %+v", response)
				}
			}},
		{name: "log append", path: "/worker/v1/run/computer-commands/logs/append", handler: func(s *Server) http.HandlerFunc { return s.workerAppendCommandLogs },
			call: func(t *testing.T, f runtest.Fixture, client *workerclient.Client, command, instance uuid.UUID, generation int64) {
				if err := client.AppendCommandLog(t.Context(), workerapi.CommandLogAppendRequest{OrgID: f.OrgID.String(), CommandID: command.String(), ComputerInstanceID: instance.String(), WriterGeneration: generation, Stream: workerapi.LogStreamStdout, ObservedAt: time.Now().UTC().Truncate(time.Millisecond), Content: []byte("output")}); err != nil {
					t.Fatalf("log append across drain: %v", err)
				}
				var accepted int
				if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM telemetry_outbox WHERE command_id=$1`, command).Scan(&accepted); err != nil || accepted != 1 {
					t.Fatalf("log append accepted=%d err=%v", accepted, err)
				}
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := runtest.New(t)
			command, instance, generation := runningComputerCommand(t, f)
			if test.stopping {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET status='stopping',cancel_requested_at=now() WHERE id=$1`, command)
			}
			server := &Server{db: db.New(f.Pool), tx: f.Pool, secretDelivery: claimHTTPSecrets{}}
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
				resolution, err := race.client.ResolveSecretProxy(t.Context(), workerapi.SecretProxyRequest{ComputerInstanceID: f.runtime.String(), Origin: "https://example.com", Placeholders: f.markers})
				if err != nil {
					t.Fatalf("resolve across drain: %v", err)
				}
				if string(resolution.Values[f.markers[0]]) != "old-a" {
					t.Fatal("replayed resolution returned different material")
				}
			} else {
				preparation, err := race.client.PrepareSecretProxy(t.Context(), workerapi.SecretProxyRequest{ComputerInstanceID: f.runtime.String()})
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
				if _, err := f.store.Revoke(t.Context(), f.fixture.EnvironmentID, pgvalue.MustUUIDValue(f.secrets[0]), "revoke"); err != nil {
					t.Fatal(err)
				}
			} else {
				dbtest.MustExec(t, t.Context(), f.fixture.Pool, test.sql, f.fixture.WorkerID)
			}
			for _, resolve := range []bool{false, true} {
				status := test.status
				if !resolve && test.name == "revoked Secret" {
					status = http.StatusOK // Preparation carries no Secret material.
				}
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
