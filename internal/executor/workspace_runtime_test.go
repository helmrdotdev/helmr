package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type workspaceRuntimeContractControl struct {
	*testRunLeaseControl
	createRequest  workerapi.CreateWorkspaceRequest
	createResponse workerapi.CreateWorkspaceResponse
	execRequest    workerapi.ExecuteWorkspaceRequest
	execResponse   workerapi.ExecuteWorkspaceResponse
	pollRequests   []workerapi.PollWorkspaceExecRequest
	pollResponses  []workerapi.ExecuteWorkspaceResponse
}

func (control *workspaceRuntimeContractControl) CreateRunWorkspace(
	_ context.Context,
	request workerapi.CreateWorkspaceRequest,
) (workerapi.CreateWorkspaceResponse, error) {
	control.createRequest = request
	return control.createResponse, nil
}

func (*workspaceRuntimeContractControl) RetrieveRunWorkspace(
	context.Context, workerapi.RetrieveWorkspaceRequest,
) (workerapi.RetrieveWorkspaceResponse, error) {
	panic("unexpected Workspace retrieve")
}

func (*workspaceRuntimeContractControl) ReadRunWorkspaceFile(
	context.Context, workerapi.ReadWorkspaceFileRequest,
) (workerapi.ReadWorkspaceFileResponse, error) {
	panic("unexpected Workspace file read")
}

func (*workspaceRuntimeContractControl) StatRunWorkspaceFile(
	context.Context, workerapi.ReadWorkspaceFileRequest,
) (workerapi.StatWorkspaceFileResponse, error) {
	panic("unexpected Workspace file stat")
}

func (*workspaceRuntimeContractControl) ListRunWorkspaceFiles(
	context.Context, workerapi.ListWorkspaceFilesRequest,
) (workerapi.ListWorkspaceFilesResponse, error) {
	panic("unexpected Workspace file list")
}

func (control *workspaceRuntimeContractControl) ExecuteRunWorkspace(
	_ context.Context,
	request workerapi.ExecuteWorkspaceRequest,
) (workerapi.ExecuteWorkspaceResponse, error) {
	control.execRequest = request
	return control.execResponse, nil
}

func (control *workspaceRuntimeContractControl) PollRunWorkspaceExec(
	_ context.Context,
	request workerapi.PollWorkspaceExecRequest,
) (workerapi.ExecuteWorkspaceResponse, error) {
	control.pollRequests = append(control.pollRequests, request)
	response := control.pollResponses[0]
	control.pollResponses = control.pollResponses[1:]
	return response, nil
}

func (*workspaceRuntimeContractControl) DeleteRunWorkspace(
	context.Context, workerapi.DeleteWorkspaceRequest,
) (workerapi.DeleteWorkspaceResponse, error) {
	panic("unexpected Workspace delete")
}

func TestWorkspaceRuntimeVerticalContract(t *testing.T) {
	const correlationID = "019c0225-f0c9-7f66-8a23-7782ca0a8461"
	t.Run("create happy path", func(t *testing.T) {
		control := &workspaceRuntimeContractControl{
			testRunLeaseControl: &testRunLeaseControl{},
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
		}, control)
		if err != nil {
			t.Fatal(err)
		}
		if decision.GetKind() != "completed" ||
			control.createRequest.Lease.ID == "" ||
			control.createRequest.WorkspaceDeclaredID != "cache" ||
			control.createRequest.Key == nil || *control.createRequest.Key != key ||
			len(control.createRequest.Secrets) != 1 {
			t.Fatalf("decision = %+v request = %+v", decision, control.createRequest)
		}
	})
	t.Run("domain failure", func(t *testing.T) {
		control := &workspaceRuntimeContractControl{
			testRunLeaseControl: &testRunLeaseControl{},
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
		}, control)
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
		control := &workspaceRuntimeContractControl{
			testRunLeaseControl: &testRunLeaseControl{},
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
		}, control)
		if err != nil {
			t.Fatal(err)
		}
		if decision.GetKind() != "completed" ||
			len(control.pollRequests) != 1 ||
			control.execRequest.Lease.ID == "" ||
			control.pollRequests[0].Lease.ID == "" ||
			control.pollRequests[0].ProcessID != "019c0225-f0c9-7f66-8a23-7782ca0a8462" {
			t.Fatalf("decision = %+v exec = %+v polls = %+v", decision, control.execRequest, control.pollRequests)
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
			return &client.HTTPError{
				StatusCode: 503, Status: "503 Service Unavailable",
				Message: "temporary control failure",
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
	control *workspaceRuntimeContractControl,
) (*runv0.ResumeDecision, error) {
	t.Helper()
	lease := testFreshProgramClaim(t).Lease
	lease.ExpiresAt = time.Now().Add(time.Minute).UTC()
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	task := &guestRunLeaseTask{
		program: freshProgram{session: fakeGuestSession{stream: guest}},
		control: control,
		lease:   lease,
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
