package slack

import (
	"bytes"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSlackComputerLossInterruptionHasOneTerminalNotice(t *testing.T) {
	for _, tc := range []struct {
		name, cause string
		wantNotice  bool
	}{
		{"covered_loss", `{"reason":"computer_execution_lost","recoveryGap":false}`, true},
		{"gap_already_explained", `{"reason":"computer_execution_lost","recoveryGap":true}`, false},
		{"ordinary_interruption", `{}`, false},
		{"storage_pressure", `{"reason":"storage_pressure"}`, false},
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
			if tc.wantNotice {
				want = 1
				var payload []byte
				if err := f.Pool.QueryRow(t.Context(), `SELECT payload FROM slack_posts WHERE thread_source_id=$1 AND role='lifecycle'`, f.participant).Scan(&payload); err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(payload, []byte("Mention me to try again")) || bytes.Contains(payload, []byte("was restored")) {
					t.Fatalf("terminal notice must ask for retry without claiming completed restoration: %s", payload)
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
