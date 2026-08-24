package db_test

import (
	"os"
	"strings"
	"testing"
)

func TestReleaseRestoredRunResumeWaitIsWaitOnlyAndFullyGuarded(t *testing.T) {
	body, err := os.ReadFile("query/run_waits.sql")
	if err != nil {
		t.Fatal(err)
	}
	const marker = "-- name: ReleaseRunResumeWait :one"
	start := strings.Index(string(body), marker)
	if start < 0 {
		t.Fatalf("release query %q is missing", marker)
	}
	releaseQuery := string(body[start:])
	if next := strings.Index(releaseQuery[len(marker):], "-- name:"); next >= 0 {
		releaseQuery = releaseQuery[:len(marker)+next]
	}
	if strings.Count(releaseQuery, "UPDATE ") != 1 ||
		!strings.Contains(releaseQuery, "UPDATE run_waits") {
		t.Fatalf("release query must contain exactly one run_waits update:\n%s", releaseQuery)
	}
	for _, guard := range []string{
		"suspension_state = 'resuming'",
		"suspend_checkpoint_id = sqlc.arg(checkpoint_id)::uuid",
		"resume_attach_id = sqlc.arg(resume_attach_id)",
		"resume_request_version = sqlc.arg(resume_request_version)",
		"resume_ack_version < resume_request_version",
	} {
		if !strings.Contains(releaseQuery, guard) {
			t.Fatalf("release query missing guard %q:\n%s", guard, releaseQuery)
		}
	}
	if strings.Contains(releaseQuery, "UPDATE runs") ||
		strings.Contains(releaseQuery, "UPDATE run_leases") ||
		strings.Contains(releaseQuery, "outbox") {
		t.Fatalf("release query mutates authority outside run_waits:\n%s", releaseQuery)
	}
}
