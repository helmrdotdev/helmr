package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func TestCapacityAgentProviderAbsenceRequiresAuthorityAndFencesExactHost(t *testing.T) {
	f := agenttest.New(t)
	otherHost, otherComputer := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$2::uuid,'resource_id','other-host','current_service_id',$2::uuid))).* FROM worker_hosts h WHERE id=$1;
 INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest) SELECT environment_id,$4,initial_root_id,initial_root_digest FROM computers WHERE id=$3;
 INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,computer_instance_id,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at)
 SELECT environment_id,$4,epoch,$2,worker_epoch,expires_at,$5,channel_credential_digest,status,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,delivered_at,initialized_at FROM computer_leases WHERE computer_id=$3`, pgx.QueryExecModeSimpleProtocol, f.Worker, otherHost, f.Computer, otherComputer, uuid.NewV7())
	handler := newPostgresServer(t, f.Pool, func(c *ServerConfig) { c.CapacityToken = capacityTestToken() })
	secret := seedHostSecret(t, f.Pool, f.Worker)
	credential := secret.issue(t, handler)
	request := func(method, path, token string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	path := "/capacity/v1/worker-hosts/" + f.Worker.String()
	if w := request(http.MethodPost, path+"/lost", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized loss %d: %s", w.Code, w.Body.String())
	}
	before := request(http.MethodGet, path, capacityTestToken())
	var host workergroup.WorkerHost
	if err := json.Unmarshal(before.Body.Bytes(), &host); err != nil || before.Code != http.StatusOK {
		t.Fatalf("get host %d %v: %s", before.Code, err, before.Body.String())
	}
	if host.DrainBlockers.UnreclaimedInstances != 1 || host.DrainBlockers.UnreconciledSessionProcesses != 1 {
		t.Fatalf("physical blockers: %+v", host.DrainBlockers)
	}
	for range 2 {
		w := request(http.MethodPost, path+"/lost", capacityTestToken())
		if err := json.Unmarshal(w.Body.Bytes(), &host); err != nil || w.Code != http.StatusOK {
			t.Fatalf("provider loss %d %v: %s", w.Code, err, w.Body.String())
		}
		if host.Status != workergroup.WorkerHostStatusLost || host.DrainBlockers.UnreclaimedInstances != 0 || host.DrainBlockers.UnreconciledSessionProcesses != 0 {
			t.Fatalf("loss response %+v", host)
		}
	}
	var exact bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT bool_and((fenced_at IS NOT NULL)=(worker_host_id=$1)) FROM computer_leases`, f.Worker).Scan(&exact); err != nil || !exact {
		t.Fatalf("cross-host physical fence %v %v", exact, err)
	}
	var revoked bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT bool_and(revoked_at IS NOT NULL) FROM worker_host_secrets WHERE worker_host_id=$1`, f.Worker).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("host secret survives loss %v %v", revoked, err)
	}
	if w := request(http.MethodGet, "/worker/v1/instance", credential); w.Code != http.StatusUnauthorized {
		t.Fatalf("old credential survived %d: %s", w.Code, w.Body.String())
	}
	listed := request(http.MethodGet, "/capacity/v1/worker-hosts?has_unreclaimed_instance=true", capacityTestToken())
	var hosts workergroup.ListWorkerHostsResponse
	if err := json.Unmarshal(listed.Body.Bytes(), &hosts); err != nil || listed.Code != http.StatusOK || len(hosts.WorkerHosts) != 1 || hosts.WorkerHosts[0].ID != otherHost.String() {
		t.Fatalf("physical blockers filter %d %v: %s", listed.Code, err, listed.Body.String())
	}
	if w := request(http.MethodPost, "/capacity/v1/worker-hosts/"+uuid.NewV7().String()+"/lost", capacityTestToken()); w.Code != http.StatusNotFound {
		t.Fatalf("unknown host %d: %s", w.Code, w.Body.String())
	}
}

func TestCapacityAgentProviderAbsenceContentionIsRetryable(t *testing.T) {
	f := agenttest.New(t)
	handler := newPostgresServer(t, f.Pool, func(c *ServerConfig) { c.CapacityToken = capacityTestToken() })
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	dbtest.MustExec(t, t.Context(), tx, `SELECT id FROM computers WHERE id=$1 FOR NO KEY UPDATE`, f.Computer)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/capacity/v1/worker-hosts/"+f.Worker.String()+"/lost", nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+capacityTestToken())
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request(); w.Code != http.StatusConflict {
		t.Fatalf("contention %d: %s", w.Code, w.Body.String())
	}
	var unchanged bool
	if err = f.Pool.QueryRow(ctx, `SELECT h.status='active' AND l.fenced_at IS NULL FROM worker_hosts h JOIN computer_leases l ON l.worker_host_id=h.id`).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("contention partially committed: %v %v", unchanged, err)
	}
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if w := request(); w.Code != http.StatusOK {
		t.Fatalf("retry %d: %s", w.Code, w.Body.String())
	}
}

func TestCapacityAgentOrdinaryLossNeedsProviderConfirmation(t *testing.T) {
	f := agenttest.New(t)
	handler := newPostgresServer(t, f.Pool, func(c *ServerConfig) { c.CapacityToken = capacityTestToken() })
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET status='lost',lost_at=clock_timestamp() WHERE id=$1`, f.Worker)
	var unfenced bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT fenced_at IS NULL FROM computer_leases`).Scan(&unfenced); err != nil || !unfenced {
		t.Fatalf("ordinary loss fenced a VM: %v %v", unfenced, err)
	}
	r := httptest.NewRequest(http.MethodPost, "/capacity/v1/worker-hosts/"+f.Worker.String()+"/lost", nil)
	r.Header.Set("Authorization", "Bearer "+capacityTestToken())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("provider confirmation %d: %s", w.Code, w.Body.String())
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT fenced_at IS NULL FROM computer_leases`).Scan(&unfenced); err != nil || unfenced {
		t.Fatalf("physical confirmation did not fence: %v %v", unfenced, err)
	}
}
