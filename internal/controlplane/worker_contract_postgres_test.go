package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A restarted worker on another contract changes nothing: token exchange does
// not advance the epoch, and recovery does not reclaim prior-epoch runtime
// state or reconcile its leases. The same requests with the matching contract
// do, which shows the rejected ones reached state the check protects.
func TestWorkerContractMismatchLeavesHostStateUnchanged(t *testing.T) {
	f := runtest.New(t)
	f.AddRunLease(t, "running", time.Now())
	handler := newPostgresServer(t, f.Pool)
	credential := seedHostCredential(t, f.Pool, f.WorkerID)
	// The restarted worker process holds the stored secret under a new
	// service, so an admitted exchange starts a new epoch.
	restarted := credential
	restarted.serviceID = uuid.NewV7()
	const foreign = "helmr.worker-api.v1.r0"
	send := func(path string, contract []string, token string, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		for _, value := range contract {
			request.Header.Add(workerapi.ContractHeader, value)
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	tokenBody, err := json.Marshal(workerapi.TokenRequest{
		WorkerHostID: restarted.hostID.String(), WorkerHostSecret: restarted.secret, ServiceID: restarted.serviceID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}

	before := workerHostState(t, f.Pool, f.WorkerID)
	for _, test := range []struct {
		contract []string
		worker   string
		body     string
	}{
		{contract: []string{foreign}, worker: foreign, body: string(tokenBody)},
		{body: string(tokenBody)},
		{contract: []string{foreign}, worker: foreign, body: `{"renamed_field":true}`},
	} {
		assertWorkerContractMismatch(t, send("/worker/v1/instance/token", test.contract, "", test.body), test.worker)
		if after := workerHostState(t, f.Pool, f.WorkerID); after != before {
			t.Fatalf("rejected token exchange changed host state:\nbefore %s\nafter  %s", before, after)
		}
	}

	token := exchangeWorkerToken(t, handler, restarted.hostID.String(), restarted.secret, restarted.serviceID.String())
	registering := workerHostState(t, f.Pool, f.WorkerID)
	if registering == before {
		t.Fatal("matching token exchange did not start a new epoch")
	}
	recoverBody, err := json.Marshal(workerapi.StartupRecoveryRequest{
		InventoryComplete: true, InventoryScope: "worker_runtime_state_roots_v0",
		ObservedAt: time.Now().UTC(), Inventory: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	activateBody, err := json.Marshal(workerapi.ActivateRequest{Capabilities: validWorkerCapabilities(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path     string
		contract []string
		worker   string
		body     string
	}{
		{path: "/worker/v1/instance/recover", contract: []string{foreign}, worker: foreign, body: string(recoverBody)},
		{path: "/worker/v1/instance/recover", body: string(recoverBody)},
		{path: "/worker/v1/instance/recover", contract: []string{foreign}, worker: foreign, body: `{"renamed_field":true}`},
		{path: "/worker/v1/instance/activate", contract: []string{foreign}, worker: foreign, body: string(activateBody)},
		{path: "/worker/v1/instance/activate"},
	} {
		assertWorkerContractMismatch(t, send(test.path, test.contract, token, test.body), test.worker)
		if after := workerHostState(t, f.Pool, f.WorkerID); after != registering {
			t.Fatalf("rejected %s changed host state:\nbefore %s\nafter  %s", test.path, registering, after)
		}
	}

	if response := send("/worker/v1/instance/recover", []string{workerapi.Contract}, token, string(recoverBody)); response.Code != http.StatusNoContent {
		t.Fatalf("matching recovery status = %d: %s", response.Code, response.Body.String())
	}
	var reclaimed bool
	if err := f.Pool.QueryRow(t.Context(),
		`SELECT bool_and(reclaimed_at IS NOT NULL) FROM computer_instances WHERE worker_host_id=$1`, f.WorkerID,
	).Scan(&reclaimed); err != nil {
		t.Fatal(err)
	}
	if !reclaimed {
		t.Fatal("matching recovery did not reclaim the prior-epoch Instance")
	}
}

// workerHostState is the durable state a worker host's lifecycle requests
// can change: the host row (epoch, status, capacity, observations), its
// credentials, its Instances and their Run Leases.
func workerHostState(t *testing.T, pool *pgxpool.Pool, hostID uuid.UUID) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(t.Context(), `
		SELECT jsonb_build_object(
			'host', (SELECT to_jsonb(h) FROM worker_hosts h WHERE h.id=$1),
			'credentials', (SELECT jsonb_agg(to_jsonb(c) ORDER BY c.id) FROM worker_host_credentials c WHERE c.worker_host_id=$1),
			'instances', (SELECT jsonb_agg(to_jsonb(i) ORDER BY i.id) FROM computer_instances i WHERE i.worker_host_id=$1),
			'leases', (SELECT jsonb_agg(to_jsonb(l) ORDER BY l.id) FROM run_leases l
			             JOIN computer_instances i ON i.id=l.computer_instance_id WHERE i.worker_host_id=$1)
		)::text`, hostID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}
