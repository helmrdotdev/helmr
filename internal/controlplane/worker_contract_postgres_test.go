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

func TestWorkerConnectionContractLeavesLifecycleStateUnchanged(t *testing.T) {
	f := runtest.New(t)
	f.AddRunLease(t, "running", time.Now())
	handler := newPostgresServer(t, f.Pool)
	credential := seedHostCredential(t, f.Pool, f.WorkerID)
	token := exchangeWorkerToken(t, handler, credential.hostID.String(), credential.secret, credential.serviceID.String())
	before := workerHostState(t, f.Pool, f.WorkerID)
	for _, contract := range []string{"", "helmr.worker-api.v1.r0"} {
		for path, body := range map[string]any{
			"/worker/v1/instance/token":    workerapi.TokenRequest{Contract: contract, WorkerHostID: credential.hostID.String(), WorkerHostSecret: credential.secret, ServiceID: uuid.NewV7().String()},
			"/worker/v1/instance/activate": workerapi.ActivateRequest{Contract: contract, Capabilities: validWorkerCapabilities(t)},
		} {
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
			req.Header.Set("Authorization", "Bearer "+token)
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, req)
			assertAdminError(t, out, http.StatusConflict, workerapi.ContractMismatchCode)
			if after := workerHostState(t, f.Pool, f.WorkerID); after != before {
				t.Fatalf("%s changed lifecycle state", path)
			}
		}
	}
	// Matching authentication advances the epoch without a contract header.
	restarted := uuid.NewV7().String()
	token = exchangeWorkerToken(t, handler, credential.hostID.String(), credential.secret, restarted)
	if workerHostState(t, f.Pool, f.WorkerID) == before {
		t.Fatal("matching token exchange did not advance epoch")
	}
}

// Authentication may record credential use before activation validates its body.
// Ignore that audit timestamp; ownership, lifecycle and execution state must stay intact.
func workerHostState(t *testing.T, pool *pgxpool.Pool, hostID uuid.UUID) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(t.Context(), `
		SELECT jsonb_build_object(
			'host', (SELECT to_jsonb(h) FROM worker_hosts h WHERE h.id=$1),
			'credentials', (SELECT jsonb_agg(to_jsonb(c) - 'last_used_at' ORDER BY c.id) FROM worker_host_credentials c WHERE c.worker_host_id=$1),
			'instances', (SELECT jsonb_agg(to_jsonb(i) ORDER BY i.id) FROM computer_instances i WHERE i.worker_host_id=$1),
			'leases', (SELECT jsonb_agg(to_jsonb(l) ORDER BY l.id) FROM run_leases l
			             JOIN computer_instances i ON i.id=l.computer_instance_id WHERE i.worker_host_id=$1)
		)::text`, hostID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}
