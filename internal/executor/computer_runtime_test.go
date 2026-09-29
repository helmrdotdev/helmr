package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/httpclient"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type computerRuntimeContractControlPlane struct {
	*testRunLeaseControlPlane
	createRequest  workerapi.CreateComputerRequest
	createResponse workerapi.CreateComputerResponse
}

func (controlPlane *computerRuntimeContractControlPlane) CreateRunComputer(
	_ context.Context,
	request workerapi.CreateComputerRequest,
) (workerapi.CreateComputerResponse, error) {
	controlPlane.createRequest = request
	return controlPlane.createResponse, nil
}

func (*computerRuntimeContractControlPlane) RetrieveRunComputer(
	context.Context, workerapi.RetrieveComputerRequest,
) (workerapi.RetrieveComputerResponse, error) {
	panic("unexpected Computer retrieve")
}

func (*computerRuntimeContractControlPlane) ListRunComputerMembers(context.Context, workerapi.ComputerMembersRequest) (workerapi.ComputerMembersResponse, error) {
	panic("unexpected Computer members")
}

func (*computerRuntimeContractControlPlane) DeleteRunComputer(
	context.Context, workerapi.DeleteComputerRequest,
) (workerapi.DeleteComputerResponse, error) {
	panic("unexpected Computer delete")
}

func TestComputerRuntimeVerticalContract(t *testing.T) {
	const correlationID = "019c0225-f0c9-7f66-8a23-7782ca0a8461"
	t.Run("create happy path", func(t *testing.T) {
		controlPlane := &computerRuntimeContractControlPlane{
			testRunLeaseControlPlane: &testRunLeaseControlPlane{},
			createResponse: workerapi.CreateComputerResponse{
				CorrelationID: correlationID,
				Completed: &workerapi.CreateComputerResult{
					ComputerID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
				},
			},
		}
		key := "build-cache"
		idempotencyKey := "create:build-cache"
		decision, err := runComputerRuntimeContract(t, &programv0.RunEvent{
			Event: &programv0.RunEvent_ComputerCreateRequested{
				ComputerCreateRequested: &programv0.ComputerCreateRequested{
					CorrelationId: correlationID, DeclaredId: "cache", Key: &key,
					IdempotencyKey: &idempotencyKey,
					Secrets: []*programv0.ComputerSecretPlacement{{
						Secret: "TOKEN",
						Placement: &programv0.ComputerSecretPlacement_Env{
							Env: &programv0.SecretEnvBinding{Name: "TOKEN", Mode: "raw"},
						},
					}},
				},
			},
		}, controlPlane)
		if err != nil {
			t.Fatal(err)
		}
		if decision.GetKind() != "completed" ||
			controlPlane.createRequest.Lease.ID == "" ||
			controlPlane.createRequest.SandboxDeclaredID != "cache" ||
			controlPlane.createRequest.Key == nil || *controlPlane.createRequest.Key != key ||
			len(controlPlane.createRequest.Secrets) != 1 {
			t.Fatalf("decision = %+v request = %+v", decision, controlPlane.createRequest)
		}
	})
	t.Run("domain failure", func(t *testing.T) {
		controlPlane := &computerRuntimeContractControlPlane{
			testRunLeaseControlPlane: &testRunLeaseControlPlane{},
			createResponse: workerapi.CreateComputerResponse{
				CorrelationID: correlationID,
				Failed: &workerapi.RuntimeOperationFailure{
					Code: "computer_key_conflict", Message: "key is in use",
				},
			},
		}
		decision, err := runComputerRuntimeContract(t, &programv0.RunEvent{
			Event: &programv0.RunEvent_ComputerCreateRequested{
				ComputerCreateRequested: &programv0.ComputerCreateRequested{
					CorrelationId: correlationID, DeclaredId: "cache",
				},
			},
		}, controlPlane)
		if err != nil {
			t.Fatal(err)
		}
		var failure workerapi.RuntimeOperationFailure
		if err := json.Unmarshal([]byte(decision.GetDataJson()), &failure); err != nil {
			t.Fatal(err)
		}
		if decision.GetKind() != "failed" || failure.Code != "computer_key_conflict" {
			t.Fatalf("decision = %+v failure = %+v", decision, failure)
		}
	})

}

func TestWorkerComputerRequestsRequireTypedCanonicalIdentity(t *testing.T) {
	_, err := workerComputerRetrieveRequest(
		"019C0225-F0C9-7F66-8A23-7782CA0A8461",
		&programv0.ComputerAddress{ComputerId: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"},
	)
	if err == nil {
		t.Fatal("non-canonical correlation ID was accepted")
	}
	if _, err := workerComputerRetrieveRequest(
		"019c0225-f0c9-7f66-8a23-7782ca0a8461", nil,
	); err == nil {
		t.Fatal("missing Computer address was accepted")
	}
}

func TestComputerRuntimeRetryUsesRenewedAssignment(t *testing.T) {
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	task := &guestRunLeaseTask{lease: lease}
	var assignments []workerapi.RunLeaseAssignment
	firstAttempt := make(chan struct{})
	go func() {
		<-firstAttempt
		task.mu.Lock()
		task.lease.LeaseSequence++
		task.lease.ExpiresAt = task.lease.ExpiresAt.Add(time.Minute)
		task.mu.Unlock()
	}()
	err := task.callRunSourceRuntime(t.Context(), func(
		_ context.Context,
		current workerapi.RunLeaseAssignment,
	) error {
		assignments = append(assignments, current)
		if len(assignments) == 1 {
			close(firstAttempt)
			return &httpclient.Error{
				StatusCode: 503, Status: "503 Service Unavailable",
				Message: "temporary Control Plane failure",
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(assignments) != 2 ||
		assignments[1].LeaseSequence != assignments[0].LeaseSequence+1 ||
		!assignments[1].ExpiresAt.After(assignments[0].ExpiresAt) {
		t.Fatalf("assignments = %+v", assignments)
	}
}

func runComputerRuntimeContract(
	t *testing.T,
	event *programv0.RunEvent,
	controlPlane *computerRuntimeContractControlPlane,
) (*programv0.ResumeDecision, error) {
	t.Helper()
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	task := &guestRunLeaseTask{
		program:      freshProgram{session: fakeGuestSession{stream: guest}},
		controlPlane: testControlPlane(t, controlPlane),
		lease:        lease,
	}
	result := make(chan error, 1)
	go func() { result <- task.handleComputerRuntime(t.Context(), event) }()
	reader := bufio.NewReader(host)
	header, bodyLen, err := wire.ReadStreamFrameHeader(reader)
	if err != nil {
		return nil, err
	}
	decision, err := wire.ReadResumeDecision(header, reader, bodyLen)
	if err != nil {
		return nil, err
	}
	return decision, <-result
}
