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

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerclient"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestTaskCompletionRefreshesAuthenticationWithoutChangingReceipt(t *testing.T) {
	for _, transition := range []string{"drain", "group", "revoked"} {
		t.Run(transition, func(t *testing.T) {
			f, work, fence, request := taskHTTPExecutionFixture(t)
			ctx := t.Context()
			tx, err := f.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err = run.BeginExecutionFinalization(ctx, tx, run.ExecutionFinalization{Fence: fence, RunID: pgvalue.UUID(work.RunID), AttemptNumber: 1, OperationID: pgvalue.UUID(uuid.MustParse(request.OperationID)), Fingerprint: dbtest.Digest("auth-finalization")}); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			server := &Server{db: db.New(f.Pool), tx: f.Pool}
			keys, err := auth.NewKeys(bytes.Repeat([]byte{1}, auth.RootKeySize))
			if err != nil {
				t.Fatal(err)
			}
			secret := "test-worker-secret"
			hash, err := auth.HashToken(keys.WorkerHost, secret)
			if err != nil {
				t.Fatal(err)
			}
			credentialID, serviceID := uuid.NewV7(), uuid.NewV7()
			dbtest.MustExec(t, ctx, f.Pool, `UPDATE worker_hosts SET current_service_id=$2 WHERE id=$1`, f.WorkerID, serviceID)
			dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO worker_host_credentials (id,worker_group_id,worker_host_id,key_prefix,secret_hash) VALUES ($1,$2,$3,'test-worker',$4)`, credentialID, runtest.WorkerGroupID, f.WorkerID, hash)
			server.authKeys = keys
			server.workerTokenSigningKey = bytes.Repeat([]byte{2}, auth.RootKeySize)
			server.workerTokenTTL = time.Hour
			server.log = slog.New(slog.NewTextHandler(io.Discard, nil))
			drain := func(ctx context.Context) error {
				if transition == "group" {
					_, err := f.Pool.Exec(ctx, `UPDATE worker_groups SET claim_version=claim_version+1 WHERE id=$1`, runtest.WorkerGroupID)
					return err
				}
				_, err := db.New(f.Pool).DrainWorkerHost(ctx, db.DrainWorkerHostParams{
					ID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID),
					ExpectedEpoch: pgtype.Int8{Int64: 1, Valid: true}, ExpectedClaimVersion: 1,
				})
				if err == nil && transition == "revoked" {
					_, err = f.Pool.Exec(ctx, `UPDATE worker_host_credentials SET revoked_at=now() WHERE id=$1`, credentialID)
				}
				return err
			}
			var requestMu sync.Mutex
			var tokenRequests int
			var receiptBodies [][]byte
			var statuses []int
			complete := server.requireWorker(http.HandlerFunc(server.workerCompleteTask))
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestMu.Lock()
				defer requestMu.Unlock()
				if r.URL.Path == "/worker/v1/instance/token" {
					tokenRequests++
					server.workerAuthToken(w, r)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
				receiptBodies = append(receiptBodies, body)
				r.Body = io.NopCloser(bytes.NewReader(body))
				// Invalidate the issued Worker token before the first authenticated completion.
				if len(statuses) == 0 {
					if err := drain(r.Context()); err != nil {
						http.Error(w, err.Error(), 500)
						return
					}
				}
				response := httptest.NewRecorder()
				complete.ServeHTTP(response, r)
				statuses = append(statuses, response.Code)
				for key, values := range response.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(response.Code)
				_, _ = w.Write(response.Body.Bytes())
			}))
			defer httpServer.Close()
			client, err := workerclient.New(httpServer.URL, workerclient.WithAuth(f.WorkerID.String(), secret), workerclient.WithService(serviceID.String()))
			if err != nil {
				t.Fatal(err)
			}
			err = client.CompleteTask(ctx, request)
			requestMu.Lock()
			defer requestMu.Unlock()
			if transition == "revoked" {
				if !httpclient.IsStatus(err, http.StatusUnauthorized) || tokenRequests != 2 || len(statuses) != 1 || statuses[0] != http.StatusUnauthorized {
					t.Fatalf("revoked credential completion: err=%v token requests=%d statuses=%v", err, tokenRequests, statuses)
				}
				var unchanged bool
				if err := f.Pool.QueryRow(ctx, `SELECT r.status='running' AND l.status='finalizing' AND l.terminal_at IS NULL FROM runs r JOIN run_leases l ON l.id=r.current_run_lease_id WHERE r.id=$1`, work.RunID).Scan(&unchanged); err != nil || !unchanged {
					t.Fatalf("revoked completion changed authority: %v %v", unchanged, err)
				}

				return
			}
			if err != nil {
				t.Fatalf("complete through Worker client: %v; statuses=%v", err, statuses)
			}
			if tokenRequests != 2 || len(statuses) != 2 || statuses[0] != http.StatusUnauthorized || statuses[1] != http.StatusNoContent {
				t.Fatalf("token requests=%d completion statuses=%v", tokenRequests, statuses)
			}
			if !bytes.Equal(receiptBodies[0], receiptBodies[1]) {
				t.Fatal("completion receipt changed during authentication replay")
			}
			var completed bool
			if err := f.Pool.QueryRow(ctx, `SELECT r.status='succeeded' AND l.status='completed' AND l.terminal_request_fingerprint IS NOT NULL FROM runs r JOIN run_leases l ON l.run_id=r.id WHERE l.id=$1`, work.LeaseID).Scan(&completed); err != nil || !completed {
				t.Fatalf("refreshed completion not durable: %v %v", completed, err)
			}
		})
	}
}
