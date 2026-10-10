package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/agent"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbpool"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type observedDiagnosticPool struct {
	*pgxpool.Pool
	started chan struct{}
	once    *sync.Once
}

func (p observedDiagnosticPool) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	p.once.Do(func() { close(p.started) })
	return p.Pool.QueryRow(ctx, sql, args...)
}

func TestDiagnosticPoolExhaustionPreservesCommandLifecycle(t *testing.T) {
	f, worker, target := commandHTTPFixture(t)
	cfg := f.Pool.Config()
	cfg.MaxConns = 1
	cfg.MinConns = 0
	diagnostic, err := dbpool.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	held, err := diagnostic.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	started := make(chan struct{})
	worker.handler = newPostgresServer(t, f.Pool, func(cfg *ServerConfig) {
		cfg.DiagnosticDB = observedDiagnosticPool{diagnostic, started, new(sync.Once)}
	})
	done := make(chan int, 1)
	go func() {
		response := worker.send(t, "/worker/v1/computer-commands/logs/append", workerapi.CommandLogAppendRequest{
			EnvironmentID: f.EnvironmentID.String(), CommandID: target.ID.String(), ComputerInstanceID: target.InstanceID.String(), WriterGeneration: target.WriterGeneration,
			Stream: workerapi.LogStreamStdout, Kind: "data", ObservedSeq: 1, ThroughSequence: 1, ObservedAt: time.Now().UTC(), Content: []byte("output"),
		})
		done <- response.Code
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("log request did not reach diagnostic pool")
	}
	// An authenticated lifecycle mutation on the same Computer still commits.
	var claimed workerapi.ComputerCommandClaimResponse
	worker.post(t, "/worker/v1/computer-commands/claim", workerapi.ComputerCommandClaimRequest{
		EnvironmentID: f.EnvironmentID.String(), ComputerInstanceID: target.InstanceID.String(), WriterGeneration: target.WriterGeneration,
	}, http.StatusOK, &claimed)
	if claimed.Command == nil || claimed.Command.CommandID != target.ID.String() {
		t.Fatal("command lifecycle did not progress")
	}
	select {
	case status := <-done:
		t.Fatalf("diagnostic request bypassed exhausted pool: %d", status)
	default:
	}
	held.Release()
	select {
	case status := <-done:
		if status != http.StatusOK {
			t.Fatalf("log status = %d", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("log request did not recover")
	}
}

func TestDiagnosticAuthenticationLockPreservesAdmission(t *testing.T) {
	f, worker, target := commandHTTPFixture(t)
	config := f.Pool.Config()
	config.MaxConns = 1
	config.MinConns = 0
	lifecycle, err := dbpool.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Close()
	config = config.Copy()
	config.MaxConns = 2
	diagnostic, err := dbpool.New(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer diagnostic.Close()
	started := make(chan struct{})
	worker.handler = newPostgresServer(t, lifecycle, func(cfg *ServerConfig) {
		cfg.DiagnosticDB = observedDiagnosticPool{diagnostic, started, new(sync.Once)}
	})
	lock, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback(context.Background())
	if _, err = lock.Exec(t.Context(), `SELECT id FROM worker_host_secrets WHERE worker_host_id=$1 AND revoked_at IS NULL FOR UPDATE`, f.Worker); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// More diagnostic requests than either pool's capacity wait on the same
	// credential row. None may consume the ordinary pool while authenticating.
	done := make(chan int, 3)
	body := workerapi.CommandLogAppendRequest{EnvironmentID: f.EnvironmentID.String(), CommandID: target.ID.String(), ComputerInstanceID: target.InstanceID.String(), WriterGeneration: target.WriterGeneration, Stream: workerapi.LogStreamStdout, Kind: "data", ObservedSeq: 1, ThroughSequence: 1, ObservedAt: time.Now().UTC(), Content: []byte("output")}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		go func() {
			req := httptest.NewRequest(http.MethodPost, "/worker/v1/computer-commands/logs/append", bytes.NewReader(encoded)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+worker.hostCredential)
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			worker.handler.ServeHTTP(response, req)
			done <- response.Code
		}()
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("diagnostic authentication did not use isolated pool")
	}
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for diagnostic.Stat().AcquiredConns() != 2 {
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal("diagnostic authentication did not reach held credential")
		}
	}
	admission, err := agent.Enqueue(ctx, lifecycle, agent.Caller{Kind: "user", ID: f.User}, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "auth-contention", Input: json.RawMessage(`[{"type":"text","text":"{\"work\":1}"}]`)})
	if err != nil || admission.TurnID == uuid.Nil() {
		t.Fatalf("ordinary admission blocked by diagnostic authentication: %+v %v", admission, err)
	}
	if err = lock.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("diagnostic authentication did not recover")
		}
	}
}
