package db

import (
	"strings"
	"testing"
)

func TestAppendExecutionChannelRecordJoinsCurrentRunLeaseByRunLeaseID(t *testing.T) {
	if strings.Contains(appendExecutionChannelRecord, "current_run_lease.id = inserted_record.run_lease_id") {
		t.Fatal("channel output append must not join a run id to a session id")
	}
	if !strings.Contains(appendExecutionChannelRecord, "event_seq.subject_id = current_run_lease.id") {
		t.Fatal("channel output append must join channel records back to the current run")
	}
	if !strings.Contains(appendExecutionChannelRecord, "locked_task_session AS MATERIALIZED") {
		t.Fatal("channel output append must lock the task session before reading the current run lease")
	}
	if !strings.Contains(appendExecutionChannelRecord, "runs.id = task_sessions.current_run_id") {
		t.Fatal("channel output append must require the leased run to be the current task-session run")
	}
	if !strings.Contains(appendExecutionChannelRecord, "task_sessions.status = 'open'") {
		t.Fatal("channel output append must require the task session to be open")
	}
	if !strings.Contains(appendExecutionChannelRecord, "FOR UPDATE OF task_sessions") {
		t.Fatal("channel output append must lock the task session before assigning a channel sequence")
	}
	if !strings.Contains(appendExecutionChannelRecord, "FOR UPDATE OF runs, run_leases") {
		t.Fatal("channel output append must lock the run and lease before assigning a channel sequence")
	}
	if !strings.Contains(appendExecutionChannelRecord, "next_sequence = channels.next_sequence + 1") {
		t.Fatal("channel output append must allocate sequence numbers from the channel row")
	}
}
