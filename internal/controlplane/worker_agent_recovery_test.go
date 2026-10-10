package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkerAgentStartupRecoveryFencesOnlyReportedPriorAllocations(t *testing.T) {
	f := agenttest.New(t)
	instance := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET computer_instance_id=$1`, instance)
	handler := newPostgresServer(t, f.Pool)
	server := httptest.NewServer(handler)
	defer server.Close()
	secret := seedHostSecret(t, f.Pool, f.Worker)
	oldCredential := secret.issue(t, handler)
	secret.serviceID = uuid.NewV7()
	client := secret.client(t, server.URL)
	if err := client.AuthenticateWorker(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertLease := func(want bool) {
		t.Helper()
		var fenced bool
		if err := f.Pool.QueryRow(t.Context(), `SELECT fenced_at IS NOT NULL FROM computer_leases`).Scan(&fenced); err != nil || fenced != want {
			t.Fatalf("physical fencing %v want %v: %v", fenced, want, err)
		}
	}
	assertLease(false)
	if err := client.ReportWorkerStartupRecovery(t.Context(), workerapi.StartupRecoveryRequest{Quarantined: []string{instance.String()}}); err != nil {
		t.Fatal(err)
	}
	assertLease(false)
	// The generated acknowledgement itself cannot substitute for missing cleanup.
	q := db.New(f.Pool)
	activation := db.ActivateWorkerHostParams{
		VMPlatformID:   pgtype.Text{String: "sha256:" + strings.Repeat("1", 64), Valid: true},
		EpochCPUMillis: 1000, EpochMemoryBytes: 1 << 30, EpochGuestEphemeralDiskBytes: 1 << 30,
		PerVMCPUMillis: 1000, PerVMMemoryBytes: 1 << 30, PerVMGuestEphemeralDiskBytes: 1 << 30,
		MaxVMSlots: 1, MaxVMStarts: 1, CPUEnvironment: []byte(`{}`), CPUEnvironmentDigest: pgtype.Text{String: "sha256:" + strings.Repeat("1", 64), Valid: true},
		WorkerHostID: pgtype.UUID{Bytes: f.Worker, Valid: true}, WorkerGroupID: pgtype.UUID{Bytes: f.Group, Valid: true}, WorkerEpoch: pgtype.Int8{Int64: 2, Valid: true},
	}
	if _, err := q.ActivateWorkerHost(t.Context(), activation); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("quarantined allocation did not block activation: %v", err)
	}
	if _, err := q.CompleteWorkerStartupRecovery(t.Context(), db.CompleteWorkerStartupRecoveryParams{WorkerHostID: pgtype.UUID{Bytes: f.Worker, Valid: true}, WorkerGroupID: pgtype.UUID{Bytes: f.Group, Valid: true}, WorkerEpoch: pgtype.Int8{Int64: 2, Valid: true}, RecoveryEvidence: []byte(`{"quarantined":[]}`)}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unobserved physical cleanup acknowledged: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/worker/v1/instance/recover", strings.NewReader(`{"quarantined":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+oldCredential)
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("old epoch response %d: %s", res.Code, res.Body.String())
	}
	assertLease(false)
	for range 2 {
		if err := client.ReportWorkerStartupRecovery(t.Context(), workerapi.StartupRecoveryRequest{Quarantined: []string{}}); err != nil {
			t.Fatal(err)
		}
	}
	assertLease(true)
	var holds int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds`).Scan(&holds); err != nil || holds != 1 {
		t.Fatalf("failure reconciliation %d %v", holds, err)
	}
	if row, err := q.ActivateWorkerHost(t.Context(), activation); err != nil || row.Status != "active" {
		t.Fatalf("cleaned worker cannot activate: %s %v", row.Status, err)
	}

}

func TestWorkerAgentNewHostStartsThroughNativeProtocol(t *testing.T) {
	f := agenttest.New(t)
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_group_tokens SET token_hash=$1 WHERE id=(SELECT token_id FROM worker_groups WHERE id=$2)`, token.Hash, f.Group)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	enrollmentClient, err := workerclient.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := enrollmentClient.EnrollWorker(t.Context(), token.Raw, workerapi.EnrollmentRequest{ResourceID: "native-startup-host", PoolName: "startup"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := workerclient.New(server.URL, workerclient.WithAuth(enrolled.WorkerHostID, enrolled.WorkerHostSecret), workerclient.WithService(uuid.NewV7().String()))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.AuthenticateWorker(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := client.ReportWorkerStartupRecovery(t.Context(), workerapi.StartupRecoveryRequest{Quarantined: []string{}}); err != nil {
		t.Fatal(err)
	}
	status, err := client.ActivateWorker(t.Context(), validWorkerCapabilities(t))
	if err != nil || status.Status != workerapi.StatusActive || status.ActiveInstances != 0 {
		t.Fatalf("startup status %+v %v", status, err)
	}
	observed, err := client.ObserveWorker(t.Context(), workerapi.Observation{})
	if err != nil || observed.Status != workerapi.StatusActive {
		t.Fatalf("startup observation %+v %v", observed, err)
	}
}

func TestWorkerAgentEnrollmentCannotTakeOverUnfencedAllocation(t *testing.T) {
	f := agenttest.New(t)
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_group_tokens SET token_hash=$1 WHERE id=(SELECT token_id FROM worker_groups WHERE id=$2)`, token.Hash, f.Group)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	secret := seedHostSecret(t, f.Pool, f.Worker)
	secret.serviceID = uuid.NewV7()
	owner := secret.client(t, server.URL)
	if err := owner.AuthenticateWorker(t.Context()); err != nil {
		t.Fatal(err)
	}
	registration, err := workerclient.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = registration.EnrollWorker(t.Context(), token.Raw, workerapi.EnrollmentRequest{ResourceID: "host", PoolName: "pool"}); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("registration took ownership of a retained VM: %v", err)
	}
	// Rejected enrollment must preserve the existing host secret and physical owner.
	if err := owner.ReportWorkerStartupRecovery(t.Context(), workerapi.StartupRecoveryRequest{Quarantined: []string{f.Computer.String()}}); err != nil {
		t.Fatal(err)
	}
	var blocked bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT fenced_at IS NULL FROM computer_leases`).Scan(&blocked); err != nil || !blocked {
		t.Fatalf("failed enrollment released old writer: %v %v", blocked, err)
	}
}

func TestWorkerAgentEnrollmentRechecksAllocationAfterAuthorityLock(t *testing.T) {
	f := agenttest.New(t)
	token, err := auth.GenerateEnrollmentToken()
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_group_tokens SET token_hash=$1 WHERE id=(SELECT token_id FROM worker_groups WHERE id=$2)`, token.Hash, f.Group)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	secret := seedHostSecret(t, f.Pool, f.Worker)
	secret.serviceID = uuid.NewV7()
	if err := secret.client(t, server.URL).AuthenticateWorker(t.Context()); err != nil {
		t.Fatal(err)
	}
	var allocation []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT to_jsonb(l) FROM computer_leases l`).Scan(&allocation); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM session_processes; DELETE FROM computer_leases`, pgx.QueryExecModeSimpleProtocol)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err = tx.Exec(t.Context(), `SELECT id FROM worker_groups WHERE id=$1 FOR UPDATE`, f.Group); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(t.Context(), `INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,computer_instance_id,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at) SELECT environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,computer_instance_id,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM jsonb_populate_record(NULL::computer_leases,$1::jsonb)`, allocation); err != nil {
		t.Fatal(err)
	}
	var blocker int32
	if err = tx.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	client, err := workerclient.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.EnrollWorker(ctx, token.Raw, workerapi.EnrollmentRequest{ResourceID: "host", PoolName: "pool"})
		done <- err
	}()
	for {
		var blocked bool
		if err = f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("enrollment did not wait: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("enrollment missed committed allocation: %v", err)
	}
}
