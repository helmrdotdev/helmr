package controlplane

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"net/http"
	"net/http/httptest"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerComputerProcessListingRequiresExactAllocation(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	allocations, err := client.ListAllocations(t.Context(), nil)
	if err != nil || len(allocations.Allocations) != 1 {
		t.Fatalf("allocations: %+v %v", allocations, err)
	}
	identity := allocations.Allocations[0]
	page, err := client.ListComputerProcesses(t.Context(), identity, nil)
	if err != nil || len(page.Processes) != 1 || page.Processes[0].Epoch != 1 {
		t.Fatalf("processes: %+v %v", page, err)
	}
	end, err := client.ListComputerProcesses(t.Context(), identity, &page.Processes[0])
	if err != nil || len(end.Processes) != 0 {
		t.Fatalf("cursor: %+v %v", end, err)
	}
	wrong := identity
	wrong.Epoch++
	if _, err := client.ListComputerProcesses(t.Context(), wrong, nil); err == nil {
		t.Fatal("foreign lease exposed processes")
	}
	wrong = identity
	wrong.Kind = "preparation"
	if _, err := client.ListComputerProcesses(t.Context(), wrong, nil); !httpclient.IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("wrong allocation kind: %v", err)
	}
}

func TestWorkerAllocationListingAndPhysicalStop(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	client := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_leases SET status='lost',expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1`, f.Environment)
	page, err := client.ListAllocations(t.Context(), nil)
	if err != nil || len(page.Allocations) != 1 {
		t.Fatalf("list owned expired allocation: %+v %v", page, err)
	}
	record := page.Allocations[0]
	if record.Kind != "computer" || record.EnvironmentID != f.Environment.String() || record.OwnerID != f.Computer.String() || record.InstanceID != f.Computer.String() || record.Epoch != 1 {
		t.Fatalf("wrong physical identity: %+v", record)
	}
	last, err := client.ListAllocations(t.Context(), &record)
	if err != nil || len(last.Allocations) != 0 {
		t.Fatalf("cursor: %+v %v", last, err)
	}
	if err := client.ObserveAgentComputerStopped(t.Context(), workerapi.AgentComputerStoppedRequest{AgentComputerLeaseRequest: workerapi.AgentComputerLeaseRequest{EnvironmentID: record.EnvironmentID, ComputerID: record.OwnerID, InstanceID: record.InstanceID, LeaseEpoch: record.Epoch}}); err != nil {
		t.Fatal(err)
	}
	page, err = client.ListAllocations(t.Context(), nil)
	if err != nil || len(page.Allocations) != 0 {
		t.Fatalf("stopped allocation: %+v %v", page, err)
	}
}

func TestDeletedComputerRequestsPhysicalStopWithoutReleasingCustody(t *testing.T) {
	f := agenttest.New(t)
	server := httptest.NewServer(newPostgresServer(t, f.Pool))
	defer server.Close()
	worker := seedHostSecret(t, f.Pool, f.Worker).client(t, server.URL)
	identity := workerapi.AllocationIdentity{Kind: "computer", EnvironmentID: f.Environment.String(), OwnerID: f.Computer.String(), InstanceID: f.Computer.String(), Epoch: 1}
	scope := computer.Scope{EnvironmentID: f.Environment}
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&scope.OrgID, &scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	deletion := computer.Deletion{Scope: scope, ComputerID: f.Computer, IdempotencyKey: "delete"}
	if _, err := computer.Delete(t.Context(), f.Pool, deletion); !errors.Is(err, computer.ErrBusy) {
		t.Fatalf("live member deletion: %v", err)
	}
	host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	execution := agent.Execution{EnvironmentID: f.Environment, SessionID: f.Session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.Worker, WorkerEpoch: 1, AuthorityGeneration: 1}
	attachment, err := agent.AcquireRuntimeAttachment(t.Context(), f.Pool, host, execution)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.ControlSession(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.SessionControlRequest{EnvironmentID: f.Environment, SessionID: f.Session, Kind: "cancel", RetryKey: "cancel"}); err != nil {
		t.Fatal(err)
	}
	if err := agent.ObserveSessionStopped(t.Context(), f.Pool, host, execution, attachment.Sequence); err != nil {
		t.Fatal(err)
	}
	if _, err := computer.Delete(t.Context(), f.Pool, deletion); err != nil {
		t.Fatal(err)
	}
	wrong := identity
	wrong.InstanceID = uuid.NewV7().String()
	if _, err := worker.ListComputerProcesses(t.Context(), wrong, nil); err == nil {
		t.Fatal("foreign allocation received closure authority")
	}
	for range 2 {
		_, err := worker.ListComputerProcesses(t.Context(), identity, nil)
		var response *httpclient.Error
		if !errors.As(err, &response) || response.Code != workerapi.AllocationClosed {
			t.Fatalf("deleted Computer stop instruction: %v", err)
		}
	}
	var retained bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='releasing' AND fenced_at IS NULL AND reserved_cpu_millis>0 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2`, f.Environment, f.Computer).Scan(&retained); err != nil || !retained {
		t.Fatalf("delete released physical custody: %v %v", retained, err)
	}
	if err := worker.ObserveAgentComputerStopped(t.Context(), workerapi.AgentComputerStoppedRequest{AgentComputerLeaseRequest: workerapi.AgentComputerLeaseRequest{EnvironmentID: identity.EnvironmentID, ComputerID: identity.OwnerID, InstanceID: identity.InstanceID, LeaseEpoch: identity.Epoch}}); err != nil {
		t.Fatal(err)
	}
	page, err := worker.ListAllocations(t.Context(), nil)
	if err != nil || len(page.Allocations) != 0 {
		t.Fatalf("stopped allocation still offered: %+v %v", page, err)
	}
}
