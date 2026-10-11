package agent

import (
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestSessionEventPagesAndRetention(t *testing.T) {
	f := newFixture(t)
	a := f.enqueue(t, "events")
	if _, err := Dispatch(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	seq, err := RuntimeOutput(t.Context(), f.pool, *f.host(), f.execution(), a.TurnID, uuid.NewV7(), json.RawMessage(`"live\u0000text"`))
	if err != nil {
		t.Fatal(err)
	}
	req := TurnListRequest{EnvironmentID: f.env, SessionID: f.session, Limit: 1}
	page, err := ListEvents(t.Context(), f.pool, f.caller(), req)
	if err != nil || len(page.Records) != 1 || !page.HasMore || page.Records[0].Kind != "turn.queued" {
		t.Fatalf("first page %+v: %v", page, err)
	}
	req.After = page.NextAfter
	req.Limit = 1000
	page, err = ListEvents(t.Context(), f.pool, f.caller(), req)
	if err != nil || len(page.Records) != 2 || page.HasMore || page.NextAfter != seq || string(page.Records[1].Data) != `[{"text":"live\u0000text","type":"text"}]` {
		t.Fatalf("continuation %+v: %v", page, err)
	}
	req.After = seq + 1
	if _, err := ListEvents(t.Context(), f.pool, f.caller(), req); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("future cursor: %v", err)
	}
	req.After = seq
	page, err = ListEvents(t.Context(), f.pool, f.caller(), req)
	if err != nil || len(page.Records) != 0 || page.NextAfter != seq || page.HasMore {
		t.Fatalf("empty %+v: %v", page, err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_events SET data=NULL,payload_expired_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2 AND seq<=$3`, f.env, f.session, seq-1)
	req.After = 0
	_, err = ListEvents(t.Context(), f.pool, f.caller(), req)
	var expired *CursorExpired
	if !errors.As(err, &expired) || expired.RetainedAfter != seq-1 {
		t.Fatalf("expired %+v: %v", expired, err)
	}
	req.After = seq - 1
	page, err = ListEvents(t.Context(), f.pool, f.caller(), req)
	if err != nil || len(page.Records) != 1 || page.RetainedAfter != seq-1 {
		t.Fatalf("retained %+v: %v", page, err)
	}
	req.SessionID = uuid.NewV7()
	if _, err = ListEvents(t.Context(), f.pool, f.caller(), req); !errors.Is(err, ErrDenied) {
		t.Fatalf("foreign Session: %v", err)
	}
	req.SessionID = f.session
	req.EnvironmentID = uuid.NewV7()
	if _, err = ListEvents(t.Context(), f.pool, f.caller(), req); !errors.Is(err, ErrDenied) {
		t.Fatalf("foreign environment: %v", err)
	}
	req.EnvironmentID = f.env
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE org_members SET disabled_at=clock_timestamp() WHERE user_id=$1`, f.caller().ID)
	if _, err = ListEvents(t.Context(), f.pool, f.caller(), req); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked reader: %v", err)
	}
}
