package executor

import (
	"bytes"
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/guest"
)

func TestDecodeTaskBundleResponseReturnsParseError(t *testing.T) {
	var buf bytes.Buffer
	if err := guest.WriteParseErrorFrame(&buf, "task_not_found", "task not found: deploy"); err != nil {
		t.Fatal(err)
	}
	body, err := guest.ReadMessageFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	_, err = decodeTaskBundleResponse(body)
	var parseErr TaskParseError
	if !errors.As(err, &parseErr) {
		t.Fatalf("err = %T %[1]v", err)
	}
	if parseErr.Kind != "task_not_found" || parseErr.Message != "task not found: deploy" {
		t.Fatalf("parse err = %+v", parseErr)
	}
}

func TestFailedResultPreservesTaskParseFailureKind(t *testing.T) {
	result := failedResult(TaskParseError{Kind: "duplicate_task_id", Message: "duplicate task id: deploy"})
	if result.FailureKind == nil || *result.FailureKind != "duplicate_task_id" {
		t.Fatalf("failure kind = %+v", result.FailureKind)
	}
}

func TestFailedResultMapsUnknownTaskParseKind(t *testing.T) {
	result := failedResult(TaskParseError{Kind: "bad_request", Message: "bad compiler input"})
	if result.FailureKind == nil || *result.FailureKind != "task_parse_failed" {
		t.Fatalf("failure kind = %+v", result.FailureKind)
	}
}
