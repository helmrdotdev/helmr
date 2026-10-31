package slack

import (
	"bytes"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSlackComputerLossInterruptionHasOneTerminalNotice(t *testing.T) {
	for _, tc := range []struct {
		name, cause, notice string
	}{
		{"covered_loss", `{"reason":"computer_execution_lost","recoveryGap":false}`, "Mention me to try again"},
		{"gap_already_explained", `{"reason":"computer_execution_lost","recoveryGap":true}`, ""},
		{"turn_deadline", `{"reason":"turn_deadline_elapsed"}`, "stopped after the maximum Turn duration. Queued requests continue."},
		{"ordinary_interruption", `{}`, ""},
		{"storage_pressure", `{"reason":"storage_pressure"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStatusFixture(t)
			write := f.outputWriter(t)
			write("work in progress")
			f.projectAll(t)
			var turn uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM turns WHERE environment_id=$1 AND session_id=$2 AND status='running'`, f.Environment, f.Session).Scan(&turn); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE turns SET status='interrupted',terminal_at=clock_timestamp(),processing_closed_at=clock_timestamp() WHERE id=$1`, turn)
			dbtest.MustExec(t, t.Context(), f.Pool, `WITH advanced AS (UPDATE sessions SET next_event_seq=next_event_seq+1 WHERE environment_id=$1 AND id=$2 RETURNING next_event_seq-1 AS seq)
 INSERT INTO session_events(environment_id,session_id,seq,turn_id,kind,data) SELECT $1,$2,seq,$3,'turn.interrupted',$4 FROM advanced`, f.Environment, f.Session, turn, []byte(tc.cause))
			// Repeated catch-up must neither duplicate the notice nor restart work.
			f.projectAll(t)
			f.projectAll(t)
			var notices, openIntermediate, queued int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FILTER(WHERE role='lifecycle'),count(*) FILTER(WHERE role='intermediate' AND closed_at IS NULL) FROM slack_posts WHERE thread_source_id=$1`, f.participant).Scan(&notices, &openIntermediate); err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.notice != "" {
				want = 1
				var payload []byte
				if err := f.Pool.QueryRow(t.Context(), `SELECT payload FROM slack_posts WHERE thread_source_id=$1 AND role='lifecycle'`, f.participant).Scan(&payload); err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(payload, []byte(tc.notice)) || bytes.Contains(payload, []byte("was restored")) {
					t.Fatalf("terminal notice must state its cause without claiming completed restoration: %s", payload)
				}
			}
			if notices != want || openIntermediate != 0 {
				t.Fatalf("notice count=%d want=%d open intermediate=%d", notices, want, openIntermediate)
			}
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM turns WHERE environment_id=$1 AND session_id=$2 AND status IN ('queued','running')`, f.Environment, f.Session).Scan(&queued); err != nil || queued != 0 {
				t.Fatalf("interrupted work was restarted: %d %v", queued, err)
			}
		})
	}
}

// A recovery gap is projected once per event, so a notice to a running peer and
// a held conversation's acknowledgement request are distinct posts.
func TestSlackRecoveryGapNoticesAreKeyedByEvent(t *testing.T) {
	f := newStatusFixture(t)
	held := uuid.NewV7()
	for _, data := range []string{
		`{"holdId":null,"computerId":"` + f.Computer.String() + `","leaseEpoch":1,"recoverySaveId":null,"restoredAt":null,"affectedTurnIds":[]}`,
		`{"holdId":"` + held.String() + `","computerId":"` + f.Computer.String() + `","leaseEpoch":2,"recoverySaveId":null,"restoredAt":null,"affectedTurnIds":["` + uuid.NewV7().String() + `"]}`,
	} {
		dbtest.MustExec(t, t.Context(), f.Pool, `WITH advanced AS (UPDATE sessions SET next_event_seq=next_event_seq+1 WHERE environment_id=$1 AND id=$2 RETURNING next_event_seq-1 AS seq)
 INSERT INTO session_events(environment_id,session_id,seq,kind,data) SELECT $1,$2,seq,'session.recovery_gap',$3 FROM advanced`, f.Environment, f.Session, []byte(data))
	}
	f.projectAll(t)
	f.projectAll(t)
	rows, err := f.Pool.Query(t.Context(), `SELECT payload FROM slack_posts WHERE thread_source_id=$1 AND role='lifecycle' AND post_key LIKE 'recovery:%' ORDER BY seq`, f.participant)
	if err != nil {
		t.Fatal(err)
	}
	var payloads [][]byte
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
	}
	rows.Close()
	if len(payloads) != 2 || !bytes.Contains(payloads[0], []byte("No completed requests in this conversation were affected")) || bytes.Contains(payloads[0], []byte("Resume")) ||
		!bytes.Contains(payloads[1], []byte("1 completed requests")) || !bytes.Contains(payloads[1], []byte("Mention me to continue")) {
		t.Fatalf("recovery posts: %q", payloads)
	}
}

// A peer notified of a rollback without a hold still learns that its running
// request was lost: the notice does not replace the interruption message.
func TestSlackNotifiedPeerStillReportsInterruptedRequest(t *testing.T) {
	f := newStatusFixture(t)
	write := f.outputWriter(t)
	write("work in progress")
	f.projectAll(t)
	var turn uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM turns WHERE environment_id=$1 AND session_id=$2 AND status='running'`, f.Environment, f.Session).Scan(&turn); err != nil {
		t.Fatal(err)
	}
	notice := `{"holdId":null,"computerId":"` + f.Computer.String() + `","leaseEpoch":1,"recoverySaveId":null,"restoredAt":null,"affectedTurnIds":[]}`
	dbtest.MustExec(t, t.Context(), f.Pool, `WITH advanced AS (UPDATE sessions SET next_event_seq=next_event_seq+1 WHERE environment_id=$1 AND id=$2 RETURNING next_event_seq-1 AS seq)
 INSERT INTO session_events(environment_id,session_id,seq,kind,data) SELECT $1,$2,seq,'session.recovery_gap',$3 FROM advanced`, f.Environment, f.Session, []byte(notice))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE turns SET status='interrupted',terminal_at=clock_timestamp(),processing_closed_at=clock_timestamp() WHERE id=$1`, turn)
	dbtest.MustExec(t, t.Context(), f.Pool, `WITH advanced AS (UPDATE sessions SET next_event_seq=next_event_seq+1 WHERE environment_id=$1 AND id=$2 RETURNING next_event_seq-1 AS seq)
 INSERT INTO session_events(environment_id,session_id,seq,turn_id,kind,data) SELECT $1,$2,seq,$3,'turn.interrupted',$4 FROM advanced`, f.Environment, f.Session, turn, []byte(`{"reason":"computer_execution_lost","computerId":"`+f.Computer.String()+`","leaseEpoch":1,"recoveryGap":false}`))
	f.projectAll(t)
	rows, err := f.Pool.Query(t.Context(), `SELECT payload FROM slack_posts WHERE thread_source_id=$1 AND role='lifecycle' ORDER BY seq`, f.participant)
	if err != nil {
		t.Fatal(err)
	}
	var payloads [][]byte
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
	}
	rows.Close()
	if len(payloads) != 2 || !bytes.Contains(payloads[0], []byte("new requests can run")) || bytes.Contains(payloads[0], []byte("continues")) ||
		!bytes.Contains(payloads[1], []byte("Mention me to try again")) {
		t.Fatalf("notified peer posts: %q", payloads)
	}
}
