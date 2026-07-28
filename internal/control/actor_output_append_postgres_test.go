package control

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestActorOutputAppendPostgresSequencesAndReplays(t *testing.T) {
	fixture := newActorStartPostgresFixture(t, 1)
	key := "output:actor"
	started, err := fixture.server.startActor(t.Context(), fixture.request(0, &key, "actor-start"))
	if err != nil {
		t.Fatal(err)
	}
	appendRequest, err := idempotency.NewActorOutputAppendRequest(
		fixture.environmentID,
		started.ActorID,
		"output-1",
		[]byte(`{"b":2,"a":1}`),
		"application/json",
	)
	if err != nil {
		t.Fatal(err)
	}

	tx, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	claims, err := idempotency.TransactionFor(tx)
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := claims.Acquire(t.Context(), appendRequest)
	if err != nil {
		t.Fatal(err)
	}
	recordID := uuid.Must(uuid.NewV7())
	row, err := db.New(tx).AppendActorOutputRecord(t.Context(), db.AppendActorOutputRecordParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID), ClaimID: acquired.Claim.ID,
		ActorID: pgvalue.UUID(started.ActorID), ProducerRunID: pgvalue.UUID(started.BootRunID),
		ProducerAttemptNumber: 1, ExpectedRequestFingerprint: acquired.Claim.RequestFingerprint,
		ID: pgvalue.UUID(recordID), Data: []byte(`{"a":1,"b":2}`), ContentType: "application/json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !row.Appended || row.Sequence != 1 || row.ClaimFingerprintMismatch {
		t.Fatalf("append = %+v", row)
	}
	if _, err := db.New(tx).CompleteActorOutputClaim(t.Context(), db.CompleteActorOutputClaimParams{
		EnvironmentID: row.EnvironmentID, ClaimID: acquired.Claim.ID,
		RequestFingerprint: acquired.Claim.RequestFingerprint,
		ActorID:            row.ActorID, RecordID: row.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	replay, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Rollback(context.Background())
	replayClaims, err := idempotency.TransactionFor(replay)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := replayClaims.Acquire(t.Context(), appendRequest)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.New || replayed.Claim.State != "completed" {
		t.Fatalf("replayed claim = %+v", replayed)
	}
	stored, err := db.New(replay).GetActorOutputRecordByID(t.Context(), db.GetActorOutputRecordByIDParams{
		EnvironmentID: pgvalue.UUID(fixture.environmentID),
		ActorID:       pgvalue.UUID(started.ActorID),
		ID:            pgvalue.UUID(recordID),
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalJSON(stored.Data)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Sequence != 1 || !bytes.Equal(canonical, []byte(`{"a":1,"b":2}`)) {
		t.Fatalf("stored record = %+v", stored)
	}
	if err := replay.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	conflictRequest, err := idempotency.NewActorOutputAppendRequest(
		fixture.environmentID,
		started.ActorID,
		"output-1",
		[]byte(`{"a":2}`),
		"application/json",
	)
	if err != nil {
		t.Fatal(err)
	}
	conflictTX, err := fixture.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conflictTX.Rollback(context.Background())
	conflictClaims, err := idempotency.TransactionFor(conflictTX)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conflictClaims.Acquire(t.Context(), conflictRequest); err == nil {
		t.Fatal("conflicting output append was accepted")
	} else {
		var conflict idempotency.ConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("conflict error = %v", err)
		}
	}
}
