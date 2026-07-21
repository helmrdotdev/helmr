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
	if parsed.fingerprint == "" || parsed.rollbackBaseID != uuid.Nil {
		t.Fatalf("parsed completion = %+v", parsed)
	}
}

func TestTaskCompletionFingerprintUsesSemanticJSONAndUTCInstants(t *testing.T) {
	first := validTaskCompletionRequest(t)
	second := first
	second.Lease.StartDeadlineAt = first.Lease.StartDeadlineAt.In(time.FixedZone("offset", 9*60*60))
	second.Lease.ExpiresAt = first.Lease.ExpiresAt.In(time.FixedZone("offset", 9*60*60))
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
	request.Workspace = api.WorkerTaskWorkspaceProof{
		RolledBack: &api.WorkerTaskWorkspaceRollback{BaseWorkspaceVersionID: request.Lease.BaseWorkspaceVersionID},
	}
	parsed, err := parseTaskCompletionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.kind != taskCompletionFailed || string(parsed.errorObject) != `{"details":{"a":1,"z":2},"message":"boom"}` {
		t.Fatalf("parsed completion = %+v", parsed)
	}

	request.Workspace.RolledBack.BaseWorkspaceVersionID = uuid.Must(uuid.NewV7()).String()
	if _, err := parseTaskCompletionRequest(request); err == nil {
		t.Fatal("rollback to another Workspace version was accepted")
	}
}

func TestParseTaskCompletionAcceptsEmptyFailureMessage(t *testing.T) {
	request := validTaskCompletionRequest(t)
	request.Outcome = api.WorkerTaskOutcome{Failed: &api.WorkerTaskFailure{}}
	request.Workspace = api.WorkerTaskWorkspaceProof{
		RolledBack: &api.WorkerTaskWorkspaceRollback{BaseWorkspaceVersionID: request.Lease.BaseWorkspaceVersionID},
	}
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
			r.Workspace.RolledBack = &api.WorkerTaskWorkspaceRollback{BaseWorkspaceVersionID: r.Lease.BaseWorkspaceVersionID}
		}},
		{name: "success rollback", mutate: func(r *api.WorkerCompleteTaskRequest) {
			r.Workspace = api.WorkerTaskWorkspaceProof{RolledBack: &api.WorkerTaskWorkspaceRollback{BaseWorkspaceVersionID: r.Lease.BaseWorkspaceVersionID}}
		}},
		{name: "failure capture", mutate: func(r *api.WorkerCompleteTaskRequest) {
			r.Outcome = api.WorkerTaskOutcome{Failed: &api.WorkerTaskFailure{Message: "failed"}}
		}},
		{name: "oversized message", mutate: func(r *api.WorkerCompleteTaskRequest) {
			r.Outcome = api.WorkerTaskOutcome{PayloadInvalid: &api.WorkerTaskFailure{Message: strings.Repeat("x", maxTaskCompletionMessageBytes+1)}}
			r.Workspace = api.WorkerTaskWorkspaceProof{RolledBack: &api.WorkerTaskWorkspaceRollback{BaseWorkspaceVersionID: r.Lease.BaseWorkspaceVersionID}}
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
		Workspace: api.WorkerTaskWorkspaceProof{Captured: &api.WorkerTaskWorkspaceCapture{
			Artifact: api.WorkerWorkspaceArtifact{
				Digest:     "sha256:" + strings.Repeat("a", 64),
				MediaType:  workspace.ArtifactMediaType,
				Encoding:   workspace.ArtifactEncoding,
				SizeBytes:  1024,
				EntryCount: 2,
			},
		}},
	}
}
