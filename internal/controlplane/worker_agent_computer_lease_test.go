package controlplane

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerAgentComputerLeaseRenewalAndPhysicalObservation(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	request := workerapi.AgentComputerLeaseRequest{EnvironmentID: f.Environment.String(), ComputerID: f.Computer.String(), InstanceID: f.Computer.String(), LeaseEpoch: 1}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET expires_at=clock_timestamp()+interval '10 seconds'`)
	response, err := client.RenewAgentComputerLease(t.Context(), request)
	if err != nil || time.Until(response.ExpiresAt) < 50*time.Second {
		t.Fatalf("lease renewal %+v %v", response, err)
	}
	other := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_hosts SELECT (jsonb_populate_record(NULL::worker_hosts,to_jsonb(h)||jsonb_build_object('id',$2::uuid,'resource_id','other-worker','current_service_id',$2::uuid))).* FROM worker_hosts h WHERE id=$1`, f.Worker, other)
	otherClient := seedHostSecret(t, f.Pool, other).client(t, server.URL)
	if err = otherClient.ObserveAgentComputerStopped(t.Context(), workerapi.AgentComputerStoppedRequest{AgentComputerLeaseRequest: request}); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("non-owner fenced Computer: %v", err)
	}
	wrong := request
	wrong.InstanceID = uuid.NewV7().String()
	if err = client.ObserveAgentComputerStopped(t.Context(), workerapi.AgentComputerStoppedRequest{AgentComputerLeaseRequest: wrong}); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("wrong instance fenced Computer: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second'`)
	if _, err = client.RenewAgentComputerLease(t.Context(), request); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("expired lease renewed: %v", err)
	}
	var unfenced bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT fenced_at IS NULL FROM computer_leases`).Scan(&unfenced); err != nil || !unfenced {
		t.Fatalf("renewal rejection fenced physical owner %v %v", unfenced, err)
	}
	for _, test := range []struct {
		id     string
		status int
	}{{"not-a-checkpoint", http.StatusBadRequest}, {uuid.NewV7().String(), http.StatusConflict}} {
		err = client.ObserveAgentComputerStopped(t.Context(), workerapi.AgentComputerStoppedRequest{AgentComputerLeaseRequest: request, InvalidCheckpointID: test.id})
		if !httpclient.IsStatus(err, test.status) {
			t.Fatalf("invalid checkpoint status: %v", err)
		}
		if err = f.Pool.QueryRow(t.Context(), `SELECT fenced_at IS NULL FROM computer_leases`).Scan(&unfenced); err != nil || !unfenced {
			t.Fatalf("invalid report fenced lease: %v %v", unfenced, err)
		}
	}
	for range 2 {
		if err = client.ObserveAgentComputerStopped(t.Context(), workerapi.AgentComputerStoppedRequest{AgentComputerLeaseRequest: request}); err != nil {
			t.Fatal(err)
		}
	}
	var fenced bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT status='lost' AND fenced_at IS NOT NULL AND fence_evidence IS NOT NULL FROM computer_leases`).Scan(&fenced); err != nil || !fenced {
		t.Fatalf("physical observation missing %v %v", fenced, err)
	}
	var holds int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds`).Scan(&holds); err != nil || holds != 1 {
		t.Fatalf("physical stop retries duplicated holds %d %v", holds, err)
	}
}
