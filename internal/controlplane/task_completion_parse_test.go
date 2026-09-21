package controlplane

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func TestParseTaskCompletionSuccess(t *testing.T) {
	request := validTaskCompletionRequest(t)
	parsed, err := parseTaskCompletionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.kind != taskCompletionSucceeded || string(parsed.output) != `{"a":1,"b":2}` || parsed.capture == nil {
		t.Fatalf("parsed completion = %+v", parsed)
	}
	if parsed.fingerprint == "" {
		t.Fatalf("parsed completion = %+v", parsed)
	}
}

func TestTaskCompletionFingerprintUsesSemanticJSONAndLeaseFence(t *testing.T) {
	first := validTaskCompletionRequest(t)
	second := first
	second.Workspace.Captured = cloneTaskWorkspaceCapture(first.Workspace.Captured)
	second.Outcome.Succeeded = &workerapi.TaskSucceeded{Output: json.RawMessage(`{"b":2,"a":1}`)}

	left, err := parseTaskCompletionRequest(first)
	if err != nil {
		t.Fatal(err)
	}
	right, err := parseTaskCompletionRequest(second)
	if err != nil {
		t.Fatal(err)
	}
	if left.fingerprint != right.fingerprint {
		t.Fatalf("fingerprints differ: %q != %q", left.fingerprint, right.fingerprint)
	}

	second.Lease.LeaseSequence++
	setCaptureFingerprint(t, second.Workspace.Captured)
	changed, err := parseTaskCompletionRequest(second)
	if err != nil {
		t.Fatal(err)
	}
	if left.fingerprint == changed.fingerprint {
		t.Fatal("changed lease fence did not change fingerprint")
	}
}

func TestParseTaskCompletionFailureRequiresRetainedCapture(t *testing.T) {
	request := validTaskCompletionRequest(t)
	request.Outcome = workerapi.TaskOutcome{
		Failed: &workerapi.TaskFailure{Message: "boom", Details: json.RawMessage(`{"z":2,"a":1}`)},
	}

	parsed, err := parseTaskCompletionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.kind != taskCompletionFailed || string(parsed.errorObject) != `{"details":{"a":1,"z":2},"message":"boom"}` {
		t.Fatalf("parsed completion = %+v", parsed)
	}

	request.Workspace.Captured = nil
	if _, err := parseTaskCompletionRequest(request); err == nil {
		t.Fatal("failure without retained capture was accepted")
	}
}

func TestParseTaskCompletionRequiresFailureMessage(t *testing.T) {
	request := validTaskCompletionRequest(t)
	request.Outcome = workerapi.TaskOutcome{Failed: &workerapi.TaskFailure{}}

	if _, err := parseTaskCompletionRequest(request); err == nil {
		t.Fatal("failure without a message was accepted")
	}
}

func TestParseTaskCompletionRejectsOpenOrMismatchedShapes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*workerapi.CompleteTaskRequest)
	}{
		{name: "missing outcome", mutate: func(r *workerapi.CompleteTaskRequest) { r.Outcome = workerapi.TaskOutcome{} }},
		{name: "multiple outcomes", mutate: func(r *workerapi.CompleteTaskRequest) {
			r.Outcome.Failed = &workerapi.TaskFailure{Message: "failed"}
		}},
		{name: "missing output", mutate: func(r *workerapi.CompleteTaskRequest) { r.Outcome.Succeeded.Output = nil }},
		{name: "ambiguous output", mutate: func(r *workerapi.CompleteTaskRequest) {
			r.Outcome.Succeeded.Output = json.RawMessage(`{"a":1,"a":2}`)
		}},
		{name: "oversized message", mutate: func(r *workerapi.CompleteTaskRequest) {
			r.Outcome = workerapi.TaskOutcome{PayloadInvalid: &workerapi.TaskFailure{Message: strings.Repeat("x", maxTaskCompletionMessageBytes+1)}}

		}},
		{name: "noncanonical message whitespace", mutate: func(r *workerapi.CompleteTaskRequest) {
			r.Outcome = workerapi.TaskOutcome{Failed: &workerapi.TaskFailure{Message: " failed "}}

		}},
		{name: "noncanonical digest", mutate: func(r *workerapi.CompleteTaskRequest) {
			r.Workspace.Captured.Disk.Artifact.Digest = "SHA256:" + strings.Repeat("a", 64)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validTaskCompletionRequest(t)
			test.mutate(&request)
			if _, err := parseTaskCompletionRequest(request); err == nil {
				t.Fatal("invalid completion was accepted")
			}
		})
	}
}

func validTaskCompletionRequest(t *testing.T) workerapi.CompleteTaskRequest {
	t.Helper()
	lease := validRunLeaseAssignment(uuid.NewV7())
	lease.StartDeadlineAt = time.Unix(1_800_000_000, 123_456_789).UTC()
	lease.ExpiresAt = time.Unix(1_800_000_100, 987_654_321).UTC()
	return workerapi.CompleteTaskRequest{
		Lease: lease.Fence(),
		Outcome: workerapi.TaskOutcome{Succeeded: &workerapi.TaskSucceeded{
			Output: json.RawMessage(`{"b":2,"a":1}`),
		}},
		Workspace: workerapi.TaskWorkspaceProof{Captured: validTaskWorkspaceCapture(t, lease)},
	}
}

func validTaskWorkspaceCapture(t *testing.T, lease workerapi.RunLeaseAssignment) *workerapi.TaskWorkspaceCapture {
	t.Helper()
	capture := &workerapi.TaskWorkspaceCapture{
		Receipt: validWorkspaceFinalizationReceipt(lease),
		Disk: workerapi.CheckpointComputer{
			ComputerID: lease.WorkspaceID, LogicalBytes: 4096,
			Artifact: workerapi.CheckpointArtifact{
				Digest: "sha256:" + strings.Repeat("a", 64), MediaType: computer.DiskMediaType, SizeBytes: 1024,
			},
		},
	}
	setCaptureFingerprint(t, capture)
	return capture
}

func validWorkspaceFinalizationReceipt(lease workerapi.RunLeaseAssignment) workerapi.WorkspaceFinalizationReceipt {
	return workerapi.WorkspaceFinalizationReceipt{
		OperationID: uuid.NewV7().String(),
		Fence: workerapi.WorkspaceFinalizationFence{
			WorkerInstanceID: lease.WorkerInstanceID, WorkerEpoch: lease.WorkerEpoch,
			RuntimeInstanceID: lease.RuntimeInstanceID, RuntimeIdentityID: lease.RuntimeIdentityID,
			WorkspaceID: lease.WorkspaceID, WorkspaceMountID: lease.WorkspaceMountID,
			RunID: lease.RunID, AttemptNumber: lease.AttemptNumber, RunLeaseID: lease.ID,
			LeaseSequence: lease.LeaseSequence, WorkspaceLeaseID: lease.WorkspaceLeaseID,
			OwnershipGeneration: lease.OwnershipGeneration, WriterGeneration: lease.WriterGeneration,
			MountFencingGeneration: lease.MountFencingGeneration, ExpiresAt: lease.ExpiresAt,
			BaseWorkspaceVersionID: lease.BaseWorkspaceVersionID,
		},
	}
}

func setCaptureFingerprint(t *testing.T, capture *workerapi.TaskWorkspaceCapture) {
	t.Helper()
	fingerprint, err := workspace.FinalizationFingerprint(workspace.FinalizationCaptureKind, workspace.FinalizationRequest{
		OperationID: capture.Receipt.OperationID, Fence: testFinalizationFence(capture.Receipt.Fence),
	})
	if err != nil {
		t.Fatal(err)
	}
	capture.Receipt.RequestFingerprint = fingerprint
}

func testFinalizationFence(fence workerapi.WorkspaceFinalizationFence) workspace.FinalizationFence {
	return workspace.FinalizationFence{
		WorkerInstanceID: fence.WorkerInstanceID, WorkerEpoch: fence.WorkerEpoch,
		RuntimeInstanceID: fence.RuntimeInstanceID, RuntimeIdentityID: fence.RuntimeIdentityID,
		WorkspaceID: fence.WorkspaceID, WorkspaceMountID: fence.WorkspaceMountID,
		RunID: fence.RunID, AttemptNumber: uint32(fence.AttemptNumber), RunLeaseID: fence.RunLeaseID,
		LeaseSequence: fence.LeaseSequence, WorkspaceLeaseID: fence.WorkspaceLeaseID,
		OwnershipGeneration: fence.OwnershipGeneration, WriterGeneration: fence.WriterGeneration,
		MountFencingGeneration: fence.MountFencingGeneration, ExpiresAtUnixNano: fence.ExpiresAt.UnixNano(),
		BaseWorkspaceVersionID: fence.BaseWorkspaceVersionID,
	}
}

func cloneTaskWorkspaceCapture(capture *workerapi.TaskWorkspaceCapture) *workerapi.TaskWorkspaceCapture {
	copy := *capture
	return &copy
}
