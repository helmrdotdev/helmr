package controlplane

import (
	"errors"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/session"
)

func TestActorInputAppendPostgresRejectsOversizedCanonicalInputWithoutResidue(
	t *testing.T,
) {
	fixture := newActorStartPostgresFixture(t, 1)
	started, err := fixture.server.startActor(
		t.Context(),
		fixture.request(0, nil, "actor-input-size-start"),
	)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`"` + strings.Repeat("x", (1<<20)) + `"`)
	canonical, err := canonicalJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical) <= (1 << 20) {
		t.Fatalf("canonical input size = %d, want over %d", len(canonical), (1 << 20))
	}

	type state struct {
		nextSequence int64
		records      int
		claims       int
		outbox       int
	}
	readState := func() state {
		var value state
		if err := fixture.pool.QueryRow(t.Context(), `
SELECT sessions.next_input_sequence,
       (SELECT count(*) FROM session_turns WHERE session_id = sessions.id),
       (SELECT count(*) FROM idempotency_claims
         WHERE environment_id = sessions.environment_id
           AND operation = 'session.enqueue'),
       (SELECT count(*) FROM control_outbox
         WHERE topic = 'session.input.reconcile'
           AND payload->>'sessionId' = sessions.id::text)
  FROM sessions
 WHERE sessions.id = $1`,
			started.SessionID,
		).Scan(
			&value.nextSequence,
			&value.records,
			&value.claims,
			&value.outbox,
		); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := readState()
	_, err = fixture.server.applySessionAdmission(t.Context(), session.AdmissionRequest{
		Target: session.Target{EnvironmentID: fixture.environmentID, SessionID: started.SessionID}, Mode: session.EnqueueOnly,
		Data:           data,
		IdempotencyKey: "oversized-input",
	})
	var failure *session.OperationError
	if !errors.As(err, &failure) || failure.Code != "invalid_request" {
		t.Fatalf("append error = %v, want Actor input too large", err)
	}
	after := readState()
	if after != before {
		t.Fatalf("Actor input state changed: before=%+v after=%+v", before, after)
	}
}
