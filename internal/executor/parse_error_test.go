package executor

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/taskbundle"
)

func TestFailedResultPreservesTaskParseFailureKind(t *testing.T) {
	result := failedResult(taskbundle.ParseError{Kind: "duplicate_task_id", Message: "duplicate task id: deploy"})
	if result.FailureKind == nil || *result.FailureKind != "duplicate_task_id" {
		t.Fatalf("failure kind = %+v", result.FailureKind)
	}
}

func TestFailedResultMapsUnknownTaskParseKind(t *testing.T) {
	result := failedResult(taskbundle.ParseError{Kind: "bad_request", Message: "bad compiler input"})
	if result.FailureKind == nil || *result.FailureKind != "task_parse_failed" {
		t.Fatalf("failure kind = %+v", result.FailureKind)
	}
}
