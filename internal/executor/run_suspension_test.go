package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestControlPlaneRunWaitsContinuesAlreadyOpenedWaitWithoutCreatingAnother(t *testing.T) {
	client := &fakeRunWaitClient{
		polls: []workerapi.RunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "resume_requested",
			ResumeKind: "completed", ResumePayload: json.RawMessage(`{"ok":true}`),
		}},
	}
	var resumed WaitResumeDecision
	request := testWaitRequest(workerapi.RunWaitKindChild)
	request.Resume = func(_ context.Context, decision WaitResumeDecision) error {
		resumed = decision
		return nil
	}
	err := (ControlPlaneRunWaits{Client: client}).ContinueRunWait(
		context.Background(),
		request,
		liveRunWaitResponse(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if client.createdRequest.CorrelationID != "" {
		t.Fatalf("unexpected duplicate create request = %+v", client.createdRequest)
	}
	if resumed.Kind != "completed" || string(resumed.Data) != `{"ok":true}` {
		t.Fatalf("resume = %+v", resumed)
	}
}

func TestControlPlaneRunWaitsDeliversLogicalResume(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []workerapi.RunWaitPollResponse{{
			RunID: "run-1", RunWaitID: "run-wait-id-1", Status: "resume_requested",
			ResumeKind: "completed", ResumePayload: json.RawMessage(`{"approved":true}`),
		}},
	}
	var got WaitResumeDecision
	request := testWaitRequest(workerapi.RunWaitKindActorInput)
	request.Resume = func(_ context.Context, decision WaitResumeDecision) error {
		got = decision
		return nil
	}
	err := ControlPlaneRunWaits{Client: client}.Wait(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "completed" || string(got.Data) != `{"approved":true}` {
		t.Fatalf("resume decision = %+v", got)
	}
	if client.resumeAck != nil {
		t.Fatalf("resume acknowledgement = %+v", client.resumeAck)
	}
	if len(client.pollRequests) != 1 || client.pollRequests[0].RunWaitID != "run-wait-id-1" {
		t.Fatalf("poll requests = %+v", client.pollRequests)
	}
}

func TestControlPlaneRunWaitsReturnsImmediateResumeDecision(t *testing.T) {
	immediate := liveRunWaitResponse()
	immediate.ResolutionKind = "completed"
	immediate.Resolution = json.RawMessage(`{"approved":true}`)
	client := &fakeRunWaitClient{created: immediate}
	var got WaitResumeDecision
	request := testWaitRequest(workerapi.RunWaitKindActorInput)
	request.Resume = func(_ context.Context, decision WaitResumeDecision) error {
		got = decision
		return nil
	}
	err := ControlPlaneRunWaits{Client: client}.Wait(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "completed" || string(got.Data) != `{"approved":true}` {
		t.Fatalf("resume decision = %+v", got)
	}
	if len(client.pollRequests) != 0 || client.resumeAck != nil {
		t.Fatalf("immediate resume unexpectedly polled or acknowledged: polls=%d ack=%+v", len(client.pollRequests), client.resumeAck)
	}
}

func TestControlPlaneRunWaitsRejectsMismatchedTypedIntent(t *testing.T) {
	client := &fakeRunWaitClient{
		created: liveRunWaitResponse(),
		polls: []workerapi.RunWaitPollResponse{{
			RunID: "another-run", RunWaitID: "run-wait-id-1", Status: "waiting",
		}},
	}
	err := ControlPlaneRunWaits{Client: client}.Wait(
		context.Background(), testWaitRequest(workerapi.RunWaitKindTimer),
	)
	if err == nil || !strings.Contains(err.Error(), "mismatched fence") {
		t.Fatalf("err = %v, want mismatched fence", err)
	}
}

func TestControlPlaneRunWaitsRejectsMismatchedCreationIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*workerapi.CreateRunWaitResponse)
	}{
		{name: "run", change: func(response *workerapi.CreateRunWaitResponse) {
			response.RunID = "another-run"
		}},
		{name: "wait", change: func(response *workerapi.CreateRunWaitResponse) {
			response.RunWaitID = "another-wait"
		}},
		{name: "resume attach", change: func(response *workerapi.CreateRunWaitResponse) {
			response.ResumeAttachID = "another-attach"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := liveRunWaitResponse()
			response.ResolutionKind = "completed"
			test.change(&response)
			err := ControlPlaneRunWaits{Client: &fakeRunWaitClient{created: response}}.Wait(
				context.Background(), testWaitRequest(workerapi.RunWaitKindTimer),
			)
			if err == nil || !strings.Contains(err.Error(), "exact request identity") {
				t.Fatalf("error = %v, want exact identity rejection", err)
			}
		})
	}
}

func TestControlPlaneRunWaitsReleasesOnlyExactGuestResumeProof(t *testing.T) {
	client := &fakeRunWaitClient{}
	assignment := workerapi.RunLeaseAssignment{ID: "lease-2", RunID: "run-1", AttemptNumber: 2}
	err := (ControlPlaneRunWaits{Client: client}).AcknowledgeRestore(context.Background(), RestoreAcknowledgement{
		Lease: assignment, RunWaitID: "wait-1", CheckpointID: "checkpoint-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.resumeAck == nil || client.resumeAck.Lease != assignment.Fence() ||
		client.resumeAck.RunWaitID != "wait-1" || client.resumeAck.CheckpointID != "checkpoint-1" {
		t.Fatalf("resume release = %+v", client.resumeAck)
	}
}

type fakeRunWaitClient struct {
	created        workerapi.CreateRunWaitResponse
	polls          []workerapi.RunWaitPollResponse
	createdRequest workerapi.CreateRunWaitRequest
	pollRequests   []workerapi.RunWaitPollRequest
	resumeAck      *workerapi.RunWaitResumeAckRequest
}

func (c *fakeRunWaitClient) CreateRunWait(_ context.Context, request workerapi.CreateRunWaitRequest) (workerapi.CreateRunWaitResponse, error) {
	c.createdRequest = request
	return c.created, nil
}

func (c *fakeRunWaitClient) PollRunWait(_ context.Context, request workerapi.RunWaitPollRequest) (workerapi.RunWaitPollResponse, error) {
	c.pollRequests = append(c.pollRequests, request)
	if len(c.polls) == 0 {
		return workerapi.RunWaitPollResponse{}, errors.New("unexpected run wait poll")
	}
	response := c.polls[0]
	c.polls = c.polls[1:]
	return response, nil
}

func (c *fakeRunWaitClient) AcknowledgeRunWaitResume(_ context.Context, request workerapi.RunWaitResumeAckRequest) (workerapi.RunWaitResumeAckResponse, error) {
	c.resumeAck = &request
	return workerapi.RunWaitResumeAckResponse{
		RunID: "run-1", RunWaitID: request.RunWaitID,
		CheckpointID: request.CheckpointID,
	}, nil
}

func liveRunWaitResponse() workerapi.CreateRunWaitResponse {
	return workerapi.CreateRunWaitResponse{
		RunID: "run-1", RunWaitID: "run-wait-id-1", ResumeAttachID: "resume-attach-1",
		ComputerInstanceID: "computer-instance-1", WorkerEpoch: 42,
	}
}

func testWaitRequest(kind workerapi.RunWaitKind) WaitRequest {
	return WaitRequest{
		LeaseAssignment: testWaitRunLeaseAssignment(),
		CorrelationID:   "correlation-1",
		RunWaitID:       "run-wait-id-1",
		ResumeAttachID:  "resume-attach-1",
		Kind:            kind,
	}
}

func testWaitRunLeaseAssignment() workerapi.RunLeaseAssignment {
	return workerapi.RunLeaseAssignment{
		ID: "lease-1", RunID: "run-1", AttemptNumber: 2, WorkerGroupID: "01900000-0000-7000-8000-000000000902",
		WorkerHostID: "worker-1", WorkerEpoch: 42, LeaseSequence: 1,
		ComputerInstanceID: "computer-instance-1",
	}
}
