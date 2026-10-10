package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkerConnectionAPIVersionLeavesLifecycleStateUnchanged(t *testing.T) {
	f := agenttest.New(t)
	handler := newPostgresServer(t, f.Pool)
	hostSecret := seedHostSecret(t, f.Pool, f.Worker)
	hostCredential := issueWorkerHostCredential(t, handler, hostSecret.hostID.String(), hostSecret.secret, hostSecret.serviceID.String())
	before := workerHostState(t, f.Pool, f.Worker)
	for _, version := range []string{"", "helmr.worker-api.v1.r0"} {
		for path, body := range map[string]any{
			"/worker/v1/instance/credential": workerapi.HostCredentialRequest{APIVersion: version, WorkerHostID: hostSecret.hostID.String(), WorkerHostSecret: hostSecret.secret, ServiceID: uuid.NewV7().String()},
			"/worker/v1/instance/activate":   workerapi.ActivateRequest{APIVersion: version, Capabilities: validWorkerCapabilities(t)},
		} {
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
			req.Header.Set("Authorization", "Bearer "+hostCredential)
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, req)
			assertAdminError(t, out, http.StatusConflict, workerapi.APIVersionMismatchCode)
			if after := workerHostState(t, f.Pool, f.Worker); after != before {
				t.Fatalf("%s changed lifecycle state", path)
			}
		}
	}
	// Matching authentication advances the epoch without a version header.
	restarted := uuid.NewV7().String()
	issueWorkerHostCredential(t, handler, hostSecret.hostID.String(), hostSecret.secret, restarted)
	if workerHostState(t, f.Pool, f.Worker) == before {
		t.Fatal("matching host credential issue did not advance epoch")
	}
}

// Authentication may record host secret use before activation validates its body.
// Ignore that audit timestamp; ownership, lifecycle and execution state must stay intact.
func workerHostState(t *testing.T, pool *pgxpool.Pool, hostID uuid.UUID) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(t.Context(), `
		SELECT jsonb_build_object(
			'host', (SELECT to_jsonb(h) FROM worker_hosts h WHERE h.id=$1),
			'credentials', (SELECT jsonb_agg(to_jsonb(c) - 'last_used_at' ORDER BY c.id) FROM worker_host_secrets c WHERE c.worker_host_id=$1),
			'leases', (SELECT jsonb_agg(to_jsonb(l) ORDER BY l.computer_id,l.epoch) FROM computer_leases l WHERE l.worker_host_id=$1)
		)::text`, hostID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}
