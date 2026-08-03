package controlplane

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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
	if parsed.fingerprint == "" || parsed.rollback != nil {
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

func TestParseTaskCompletionFailureRequiresRollback(t *testing.T) {
	request := validTaskCompletionRequest(t)
	request.Outcome = workerapi.TaskOutcome{
		Failed: &workerapi.TaskFailure{Message: "boom", Details: json.RawMessage(`{"z":2,"a":1}`)},
	}
	request.Workspace = workerapi.TaskWorkspaceProof{RolledBack: validTaskWorkspaceRollback(t, request.Workspace.Captured)}
	parsed, err := parseTaskCompletionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.kind != taskCompletionFailed || string(parsed.errorObject) != `{"details":{"a":1,"z":2},"message":"boom"}` {
		t.Fatalf("parsed completion = %+v", parsed)
	}

	request.Workspace.RolledBack.Target.BaseWorkspaceVersionID = uuid.Must(uuid.NewV7()).String()
	if _, err := parseTaskCompletionRequest(request); err == nil {
		t.Fatal("rollback outside the admitted base was accepted")
	}
}

func TestParseTaskCompletionAcceptsEmptyFailureMessage(t *testing.T) {
	request := validTaskCompletionRequest(t)
	request.Outcome = workerapi.TaskOutcome{Failed: &workerapi.TaskFailure{}}
	request.Workspace = workerapi.TaskWorkspaceProof{RolledBack: validTaskWorkspaceRollback(t, request.Workspace.Captured)}
	parsed, err := parseTaskCompletionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if string(parsed.errorObject) != `{"message":""}` {
		t.Fatalf("error object = %s", parsed.errorObject)
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
		{name: "multiple proofs", mutate: func(r *workerapi.CompleteTaskRequest) {
			r.Workspace.RolledBack = validTaskWorkspaceRollback(t, r.Workspace.Captured)
		}},
		{name: "success rollback", mutate: func(r *workerapi.CompleteTaskRequest) {
			r.Workspace = workerapi.TaskWorkspaceProof{RolledBack: validTaskWorkspaceRollback(t, r.Workspace.Captured)}
		}},
		{name: "failure capture", mutate: func(r *workerapi.CompleteTaskRequest) {
			r.Outcome = workerapi.TaskOutcome{Failed: &workerapi.TaskFailure{Message: "failed"}}
		}},
		{name: "oversized message", mutate: func(r *workerapi.CompleteTaskRequest) {
			r.Outcome = workerapi.TaskOutcome{PayloadInvalid: &workerapi.TaskFailure{Message: strings.Repeat("x", maxTaskCompletionMessageBytes+1)}}
			r.Workspace = workerapi.TaskWorkspaceProof{RolledBack: validTaskWorkspaceRollback(t, r.Workspace.Captured)}
		}},
		{name: "noncanonical digest", mutate: func(r *workerapi.CompleteTaskRequest) {
			r.Workspace.Captured.Artifact.Digest = "SHA256:" + strings.Repeat("a", 64)
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
	lease := validRunLeaseAssignment(uuid.Must(uuid.NewV7()))
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
		Tree: workerapi.WorkspaceTreeIdentity{
			Digest: "sha256:" + strings.Repeat("b", 64), SizeBytes: 100, EntryCount: 2,
		},
		Artifact: workerapi.WorkspaceArtifact{
			Digest: "sha256:" + strings.Repeat("a", 64), MediaType: workspace.ArtifactMediaType,
			Encoding: workspace.ArtifactEncoding, SizeBytes: 1024, EntryCount: 2,
		},
	}
	setCaptureFingerprint(t, capture)
	return capture
}

func validTaskWorkspaceRollback(
	t *testing.T,
	capture *workerapi.TaskWorkspaceCapture,
) *workerapi.TaskWorkspaceRollback {
	t.Helper()
	receipt := capture.Receipt
	baseWorkspaceVersionID := receipt.Fence.BaseWorkspaceVersionID
	target := workspace.ResetTarget{
		Kind: workspace.ResetTargetEmpty, BaseVersionID: baseWorkspaceVersionID,
		Tree: workspace.TreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest},
	}
	rollback := &workerapi.TaskWorkspaceRollback{
		Receipt: receipt,
		Target: workerapi.WorkspaceResetTarget{
			BaseWorkspaceVersionID: baseWorkspaceVersionID,
			Tree:                   workerapi.WorkspaceTreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest},
			Empty:                  &workerapi.EmptyWorkspace{},
		},
	}
	fingerprint, err := workspace.FinalizationFingerprint(workspace.FinalizationResetKind, workspace.FinalizationRequest{
		OperationID: rollback.Receipt.OperationID, Fence: testFinalizationFence(rollback.Receipt.Fence), Target: target,
	})
	if err != nil {
		t.Fatal(err)
	}
	rollback.Receipt.RequestFingerprint = fingerprint
	return rollback
}

func validWorkspaceFinalizationReceipt(lease workerapi.RunLeaseAssignment) workerapi.WorkspaceFinalizationReceipt {
	return workerapi.WorkspaceFinalizationReceipt{
		OperationID: uuid.Must(uuid.NewV7()).String(),
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
