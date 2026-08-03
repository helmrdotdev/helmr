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
SELECT actors.next_input_sequence,
       (SELECT count(*) FROM actor_records WHERE actor_id = actors.id),
       (SELECT count(*) FROM idempotency_claims
         WHERE environment_id = actors.environment_id
           AND operation = 'actor.input.send'),
       (SELECT count(*) FROM outbox_messages
         WHERE topic = 'actor.input.reconcile'
           AND partition_key = actors.id::text)
  FROM actors
 WHERE actors.id = $1`,
			started.ActorID,
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
		ActorID:        started.ActorID,
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
		`SELECT count(*) FROM actor_records WHERE id = $1`,
		recordID,
	).Scan(&recordCount); err != nil {
		t.Fatal(err)
	}
	if recordCount != 0 {
		t.Fatalf("oversized Actor input record count = %d, want 0", recordCount)
	}
}
