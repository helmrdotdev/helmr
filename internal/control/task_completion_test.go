package control

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
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

func TestTaskCompletionFingerprintUsesSemanticJSONAndUTCInstants(t *testing.T) {
	first := validTaskCompletionRequest(t)
	second := first
	second.Lease.StartDeadlineAt = first.Lease.StartDeadlineAt.In(time.FixedZone("offset", 9*60*60))
	second.Lease.ExpiresAt = first.Lease.ExpiresAt.In(time.FixedZone("offset", 9*60*60))
	second.Workspace.Captured = cloneTaskWorkspaceCapture(first.Workspace.Captured)
	second.Workspace.Captured.Receipt.Fence.ExpiresAt = second.Lease.ExpiresAt
	second.Outcome.Succeeded = &api.WorkerTaskSucceeded{Output: json.RawMessage(`{"b":2,"a":1}`)}

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

	second.Lease.ExpiresAt = second.Lease.ExpiresAt.Add(time.Nanosecond)
	second.Workspace.Captured.Receipt.Fence.ExpiresAt = second.Lease.ExpiresAt
	setCaptureFingerprint(t, second.Workspace.Captured)
	changed, err := parseTaskCompletionRequest(second)
	if err != nil {
		t.Fatal(err)
	}
	if left.fingerprint == changed.fingerprint {
		t.Fatal("changed receipt expiry did not change fingerprint")
	}
}

func TestParseTaskCompletionFailureRequiresRollback(t *testing.T) {
	request := validTaskCompletionRequest(t)
	request.Outcome = api.WorkerTaskOutcome{
		Failed: &api.WorkerTaskFailure{Message: "boom", Details: json.RawMessage(`{"z":2,"a":1}`)},
	}
	request.Workspace = api.WorkerTaskWorkspaceProof{RolledBack: validTaskWorkspaceRollback(t, request.Lease)}
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
	request.Outcome = api.WorkerTaskOutcome{Failed: &api.WorkerTaskFailure{}}
	request.Workspace = api.WorkerTaskWorkspaceProof{RolledBack: validTaskWorkspaceRollback(t, request.Lease)}
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
		mutate func(*api.WorkerCompleteTaskRequest)
	}{
		{name: "missing outcome", mutate: func(r *api.WorkerCompleteTaskRequest) { r.Outcome = api.WorkerTaskOutcome{} }},
		{name: "multiple outcomes", mutate: func(r *api.WorkerCompleteTaskRequest) {
			r.Outcome.Failed = &api.WorkerTaskFailure{Message: "failed"}
		}},
		{name: "missing output", mutate: func(r *api.WorkerCompleteTaskRequest) { r.Outcome.Succeeded.Output = nil }},
		{name: "ambiguous output", mutate: func(r *api.WorkerCompleteTaskRequest) {
			r.Outcome.Succeeded.Output = json.RawMessage(`{"a":1,"a":2}`)
		}},
		{name: "multiple proofs", mutate: func(r *api.WorkerCompleteTaskRequest) {
			r.Workspace.RolledBack = validTaskWorkspaceRollback(t, r.Lease)
		}},
		{name: "success rollback", mutate: func(r *api.WorkerCompleteTaskRequest) {
			r.Workspace = api.WorkerTaskWorkspaceProof{RolledBack: validTaskWorkspaceRollback(t, r.Lease)}
		}},
		{name: "failure capture", mutate: func(r *api.WorkerCompleteTaskRequest) {
			r.Outcome = api.WorkerTaskOutcome{Failed: &api.WorkerTaskFailure{Message: "failed"}}
		}},
		{name: "oversized message", mutate: func(r *api.WorkerCompleteTaskRequest) {
			r.Outcome = api.WorkerTaskOutcome{PayloadInvalid: &api.WorkerTaskFailure{Message: strings.Repeat("x", maxTaskCompletionMessageBytes+1)}}
			r.Workspace = api.WorkerTaskWorkspaceProof{RolledBack: validTaskWorkspaceRollback(t, r.Lease)}
		}},
		{name: "noncanonical digest", mutate: func(r *api.WorkerCompleteTaskRequest) {
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

func validTaskCompletionRequest(t *testing.T) api.WorkerCompleteTaskRequest {
	t.Helper()
	lease := validRunLeaseReceipt(uuid.Must(uuid.NewV7()))
	lease.StartDeadlineAt = time.Unix(1_800_000_000, 123_456_789).UTC()
	lease.ExpiresAt = time.Unix(1_800_000_100, 987_654_321).UTC()
	return api.WorkerCompleteTaskRequest{
		Lease: lease,
		Outcome: api.WorkerTaskOutcome{Succeeded: &api.WorkerTaskSucceeded{
			Output: json.RawMessage(`{"b":2,"a":1}`),
		}},
		Workspace: api.WorkerTaskWorkspaceProof{Captured: validTaskWorkspaceCapture(t, lease)},
	}
}

func validTaskWorkspaceCapture(t *testing.T, lease api.WorkerRunLeaseReceipt) *api.WorkerTaskWorkspaceCapture {
	t.Helper()
	capture := &api.WorkerTaskWorkspaceCapture{
		Receipt: validWorkspaceFinalizationReceipt(lease),
		Tree: api.WorkerWorkspaceTreeIdentity{
			Digest: "sha256:" + strings.Repeat("b", 64), SizeBytes: 100, EntryCount: 2,
		},
		Artifact: api.WorkerWorkspaceArtifact{
			Digest: "sha256:" + strings.Repeat("a", 64), MediaType: workspace.ArtifactMediaType,
			Encoding: workspace.ArtifactEncoding, SizeBytes: 1024, EntryCount: 2,
		},
	}
	setCaptureFingerprint(t, capture)
	return capture
}

func validTaskWorkspaceRollback(t *testing.T, lease api.WorkerRunLeaseReceipt) *api.WorkerTaskWorkspaceRollback {
	t.Helper()
	target := workspace.ResetTarget{
		Kind: workspace.ResetTargetEmpty, BaseVersionID: lease.BaseWorkspaceVersionID,
		Tree: workspace.TreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest},
	}
	rollback := &api.WorkerTaskWorkspaceRollback{
		Receipt: validWorkspaceFinalizationReceipt(lease),
		Target: api.WorkerWorkspaceResetTarget{
			BaseWorkspaceVersionID: lease.BaseWorkspaceVersionID,
			Tree:                   api.WorkerWorkspaceTreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest},
			Empty:                  &api.WorkerEmptyWorkspace{},
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

func validWorkspaceFinalizationReceipt(lease api.WorkerRunLeaseReceipt) api.WorkerWorkspaceFinalizationReceipt {
	return api.WorkerWorkspaceFinalizationReceipt{
		OperationID: uuid.Must(uuid.NewV7()).String(),
		Fence: api.WorkerWorkspaceFinalizationFence{
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

func setCaptureFingerprint(t *testing.T, capture *api.WorkerTaskWorkspaceCapture) {
	t.Helper()
	fingerprint, err := workspace.FinalizationFingerprint(workspace.FinalizationCaptureKind, workspace.FinalizationRequest{
		OperationID: capture.Receipt.OperationID, Fence: testFinalizationFence(capture.Receipt.Fence),
	})
	if err != nil {
		t.Fatal(err)
	}
	capture.Receipt.RequestFingerprint = fingerprint
}

func testFinalizationFence(fence api.WorkerWorkspaceFinalizationFence) workspace.FinalizationFence {
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

func cloneTaskWorkspaceCapture(capture *api.WorkerTaskWorkspaceCapture) *api.WorkerTaskWorkspaceCapture {
	copy := *capture
	return &copy
}
