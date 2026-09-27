package controlplane

import (
	"encoding/json"
	"strings"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestParseTaskCompletionSuccess(t *testing.T) {
	request := validTaskCompletionRequest(t)
	parsed, err := parseTaskCompletionRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.kind != taskCompletionSucceeded || string(parsed.output) != `{"a":1,"b":2}` || parsed.operationID.String() != request.OperationID {
		t.Fatalf("parsed completion = %+v", parsed)
	}
	if parsed.fingerprint == "" {
		t.Fatalf("parsed completion = %+v", parsed)
	}
}

func TestTaskCompletionFingerprintUsesSemanticJSONAndLeaseFence(t *testing.T) {
	first := validTaskCompletionRequest(t)
	second := first
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
	changed, err := parseTaskCompletionRequest(second)
	if err != nil {
		t.Fatal(err)
	}
	if left.fingerprint == changed.fingerprint {
		t.Fatal("changed lease fence did not change fingerprint")
	}
}

func TestParseTaskCompletionFailureRequiresOperation(t *testing.T) {
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

	request.OperationID = ""
	if _, err := parseTaskCompletionRequest(request); err == nil {
		t.Fatal("failure without operation identity was accepted")
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
		{name: "invalid operation", mutate: func(r *workerapi.CompleteTaskRequest) {
			r.OperationID = "invalid"
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
	return workerapi.CompleteTaskRequest{
		Lease: workerapi.RunLeaseFence{ID: uuid.NewV7().String(), LeaseSequence: 1},
		Outcome: workerapi.TaskOutcome{Succeeded: &workerapi.TaskSucceeded{
			Output: json.RawMessage(`{"b":2,"a":1}`),
		}},
		OperationID: uuid.NewV7().String(),
	}
}
