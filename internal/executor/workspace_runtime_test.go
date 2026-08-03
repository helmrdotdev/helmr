package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type workspaceRuntimeContractControlPlane struct {
	*testRunLeaseControlPlane
	createRequest  workerapi.CreateWorkspaceRequest
	createResponse workerapi.CreateWorkspaceResponse
	execRequest    workerapi.ExecuteWorkspaceRequest
	execResponse   workerapi.ExecuteWorkspaceResponse
	pollRequests   []workerapi.PollWorkspaceExecRequest
	pollResponses  []workerapi.ExecuteWorkspaceResponse
}

func (controlPlane *workspaceRuntimeContractControlPlane) CreateRunWorkspace(
	_ context.Context,
	request workerapi.CreateWorkspaceRequest,
) (workerapi.CreateWorkspaceResponse, error) {
	controlPlane.createRequest = request
	return controlPlane.createResponse, nil
}

func (*workspaceRuntimeContractControlPlane) RetrieveRunWorkspace(
	context.Context, workerapi.RetrieveWorkspaceRequest,
) (workerapi.RetrieveWorkspaceResponse, error) {
	panic("unexpected Workspace retrieve")
}

func (*workspaceRuntimeContractControlPlane) ReadRunWorkspaceFile(
	context.Context, workerapi.ReadWorkspaceFileRequest,
) (workerapi.ReadWorkspaceFileResponse, error) {
	panic("unexpected Workspace file read")
}

func (*workspaceRuntimeContractControlPlane) StatRunWorkspaceFile(
	context.Context, workerapi.ReadWorkspaceFileRequest,
) (workerapi.StatWorkspaceFileResponse, error) {
	panic("unexpected Workspace file stat")
}

func (*workspaceRuntimeContractControlPlane) ListRunWorkspaceFiles(
	context.Context, workerapi.ListWorkspaceFilesRequest,
) (workerapi.ListWorkspaceFilesResponse, error) {
	panic("unexpected Workspace file list")
}

func (controlPlane *workspaceRuntimeContractControlPlane) ExecuteRunWorkspace(
	_ context.Context,
	request workerapi.ExecuteWorkspaceRequest,
) (workerapi.ExecuteWorkspaceResponse, error) {
	controlPlane.execRequest = request
	return controlPlane.execResponse, nil
}

func (controlPlane *workspaceRuntimeContractControlPlane) PollRunWorkspaceExec(
	_ context.Context,
	request workerapi.PollWorkspaceExecRequest,
) (workerapi.ExecuteWorkspaceResponse, error) {
	controlPlane.pollRequests = append(controlPlane.pollRequests, request)
	response := controlPlane.pollResponses[0]
	controlPlane.pollResponses = controlPlane.pollResponses[1:]
	return response, nil
}

func (*workspaceRuntimeContractControlPlane) DeleteRunWorkspace(
	context.Context, workerapi.DeleteWorkspaceRequest,
) (workerapi.DeleteWorkspaceResponse, error) {
	panic("unexpected Workspace delete")
}

func TestWorkspaceRuntimeVerticalContract(t *testing.T) {
	const correlationID = "019c0225-f0c9-7f66-8a23-7782ca0a8461"
	t.Run("create happy path", func(t *testing.T) {
		controlPlane := &workspaceRuntimeContractControlPlane{
			testRunLeaseControlPlane: &testRunLeaseControlPlane{},
			createResponse: workerapi.CreateWorkspaceResponse{
				CorrelationID: correlationID,
				Completed: &api.CreateWorkspaceResponse{
					WorkspaceID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32",
				},
			},
		}
		key := "build-cache"
		idempotencyKey := "create:build-cache"
		decision, err := runWorkspaceRuntimeContract(t, &runv0.RunEvent{
			Event: &runv0.RunEvent_WorkspaceCreateRequested{
				WorkspaceCreateRequested: &runv0.WorkspaceCreateRequested{
					CorrelationId: correlationID, DeclaredId: "cache", Key: &key,
					IdempotencyKey: &idempotencyKey,
					Secrets: []*runv0.WorkspaceSecretPlacement{{
						Name: "TOKEN",
						Placement: &runv0.WorkspaceSecretPlacement_Env{
							Env: "TOKEN",
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
			controlPlane.createRequest.WorkspaceDeclaredID != "cache" ||
			controlPlane.createRequest.Key == nil || *controlPlane.createRequest.Key != key ||
			len(controlPlane.createRequest.Secrets) != 1 {
			t.Fatalf("decision = %+v request = %+v", decision, controlPlane.createRequest)
		}
	})
	t.Run("domain failure", func(t *testing.T) {
		controlPlane := &workspaceRuntimeContractControlPlane{
			testRunLeaseControlPlane: &testRunLeaseControlPlane{},
			createResponse: workerapi.CreateWorkspaceResponse{
				CorrelationID: correlationID,
				Failed: &workerapi.RuntimeOperationFailure{
					Code: "workspace_key_conflict", Message: "key is in use",
				},
			},
		}
		decision, err := runWorkspaceRuntimeContract(t, &runv0.RunEvent{
			Event: &runv0.RunEvent_WorkspaceCreateRequested{
				WorkspaceCreateRequested: &runv0.WorkspaceCreateRequested{
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
		if decision.GetKind() != "failed" || failure.Code != "workspace_key_conflict" {
			t.Fatalf("decision = %+v failure = %+v", decision, failure)
		}
	})
	t.Run("exec admission survives through poll", func(t *testing.T) {
		controlPlane := &workspaceRuntimeContractControlPlane{
			testRunLeaseControlPlane: &testRunLeaseControlPlane{},
			execResponse: workerapi.ExecuteWorkspaceResponse{
				CorrelationID: correlationID,
				Pending:       &workerapi.WorkspaceExecPending{ProcessID: "019c0225-f0c9-7f66-8a23-7782ca0a8462"},
			},
			pollResponses: []workerapi.ExecuteWorkspaceResponse{{
				CorrelationID: correlationID,
				Completed: &api.ExecuteWorkspaceResult{
					ExitCode: 0, StdoutBase64: "b2s=", StderrBase64: "",
				},
			}},
		}
		timeout := uint64(1_000)
		decision, err := runWorkspaceRuntimeContract(t, &runv0.RunEvent{
			Event: &runv0.RunEvent_WorkspaceExecRequested{
				WorkspaceExecRequested: &runv0.WorkspaceExecRequested{
					CorrelationId: correlationID,
					Workspace: &runv0.WorkspaceAddress{
						Address: &runv0.WorkspaceAddress_WorkspaceKey{WorkspaceKey: "cache"},
					},
					Command:   []string{"sh", "-c", "printf ok"},
					TimeoutMs: &timeout, IdempotencyKey: "exec:ok",
				},
			},
		}, controlPlane)
		if err != nil {
			t.Fatal(err)
		}
		if decision.GetKind() != "completed" ||
			len(controlPlane.pollRequests) != 1 ||
			controlPlane.execRequest.Lease.ID == "" ||
			controlPlane.pollRequests[0].Lease.ID == "" ||
			controlPlane.pollRequests[0].ProcessID != "019c0225-f0c9-7f66-8a23-7782ca0a8462" {
			t.Fatalf("decision = %+v exec = %+v polls = %+v", decision, controlPlane.execRequest, controlPlane.pollRequests)
		}
	})
}

func TestWorkerWorkspaceRequestsRequireTypedCanonicalIdentity(t *testing.T) {
	_, err := workerWorkspaceRetrieveRequest(
		"019C0225-F0C9-7F66-8A23-7782CA0A8461",
		&runv0.WorkspaceAddress{Address: &runv0.WorkspaceAddress_WorkspaceKey{
			WorkspaceKey: "cache",
		}},
	)
	if err == nil {
		t.Fatal("non-canonical correlation ID was accepted")
	}
	if _, err := workerWorkspaceRetrieveRequest(
		"019c0225-f0c9-7f66-8a23-7782ca0a8461", nil,
	); err == nil {
		t.Fatal("missing Workspace address was accepted")
	}
}

func TestWorkspaceRuntimeRetryUsesRenewedAssignment(t *testing.T) {
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

func runWorkspaceRuntimeContract(
	t *testing.T,
	event *runv0.RunEvent,
	controlPlane *workspaceRuntimeContractControlPlane,
) (*runv0.ResumeDecision, error) {
	t.Helper()
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	task := &guestRunLeaseTask{
		program:      freshProgram{session: fakeGuestSession{stream: guest}},
		controlPlane: controlPlane,
		lease:        lease,
	}
	result := make(chan error, 1)
	go func() { result <- task.handleWorkspaceRuntime(t.Context(), event) }()
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
