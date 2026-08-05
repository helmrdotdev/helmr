package controlplane

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
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
	data := []byte(`"` + strings.Repeat("x", maxActorInputBytes) + `"`)
	canonical, err := canonicalJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical) <= maxActorInputBytes {
		t.Fatalf("canonical input size = %d, want over %d", len(canonical), maxActorInputBytes)
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
       (SELECT count(*) FROM session_records WHERE session_id = sessions.id),
       (SELECT count(*) FROM idempotency_claims
         WHERE environment_id = sessions.environment_id
           AND operation = 'session.input.send'),
       (SELECT count(*) FROM outbox_messages
         WHERE topic = 'session.input.reconcile'
           AND partition_key = sessions.id::text)
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
	recordID := uuid.Must(uuid.NewV7())
	_, err = fixture.server.appendActorInput(t.Context(), appendActorInputRequest{
		EnvironmentID:  fixture.environmentID,
		SessionID:      started.SessionID,
		RecordID:       recordID,
		Data:           data,
		SourceKind:     "external",
		IdempotencyKey: "oversized-input",
	})
	if !errors.Is(err, errActorInputTooLarge) {
		t.Fatalf("append error = %v, want Actor input too large", err)
	}
	after := readState()
	if after != before {
		t.Fatalf("Actor input state changed: before=%+v after=%+v", before, after)
	}
	var recordCount int
	if err := fixture.pool.QueryRow(
		t.Context(),
		`SELECT count(*) FROM session_records WHERE id = $1`,
		recordID,
	).Scan(&recordCount); err != nil {
		t.Fatal(err)
	}
	if recordCount != 0 {
		t.Fatalf("oversized Actor input record count = %d, want 0", recordCount)
	}
}
