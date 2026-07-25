package control

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestRunWaitDeadlinesApplyTokenDefaults(t *testing.T) {
	before := time.Now().UTC()
	timeoutAt, idleTimeout, checkpointDueAt, delay, err := runWaitDeadlines(api.WorkerCreateRunWaitRequest{}, defaultTokenWaitIdleTimeout)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	if timeoutAt.Valid {
		t.Fatal("omitted Token Wait timeout became terminal deadline")
	}
	if !idleTimeout.Valid || idleTimeout.Int64 != defaultTokenWaitIdleTimeout.Milliseconds() {
		t.Fatalf("idle timeout = %+v, want %dms", idleTimeout, defaultTokenWaitIdleTimeout.Milliseconds())
	}
	if delay != defaultTokenWaitIdleTimeout || !checkpointDueAt.Valid ||
		checkpointDueAt.Time.Before(before.Add(delay)) || checkpointDueAt.Time.After(after.Add(delay)) {
		t.Fatalf("checkpoint deadline = %s delay=%s, want registration time + default idle", checkpointDueAt.Time, delay)
	}
}

func TestRunWaitDeadlinesPreserveMillisecondPrecision(t *testing.T) {
	timeoutMS := int64(1)
	idleTimeoutMS := int64(1501)
	before := time.Now().UTC()
	timeoutAt, idleTimeout, checkpointDueAt, delay, err := runWaitDeadlines(api.WorkerCreateRunWaitRequest{
		TimeoutMS: &timeoutMS, IdleTimeoutMS: &idleTimeoutMS,
	}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	if !timeoutAt.Valid || timeoutAt.Time.Before(before.Add(time.Millisecond)) || timeoutAt.Time.After(after.Add(time.Millisecond)) {
		t.Fatalf("timeout_at = %s, want registration time + 1ms", timeoutAt.Time)
	}
	expectedDelay := time.Millisecond + shortWaitGrace
	if !idleTimeout.Valid || idleTimeout.Int64 != idleTimeoutMS || delay != expectedDelay ||
		!checkpointDueAt.Valid || checkpointDueAt.Time.Before(before.Add(delay)) || checkpointDueAt.Time.After(after.Add(delay)) {
		t.Fatalf("idle/checkpoint = %+v/%s/%s", idleTimeout, delay, checkpointDueAt.Time)
	}
}

func TestActorInputWaitIdleTimeoutReadsFullImmutableManifest(t *testing.T) {
	idleTimeout, err := actorInputWaitIdleTimeout([]byte(`{
		"run":{"queue":"default","retry":{"enabled":false}},
		"idleTimeoutMs":1501
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if idleTimeout != 1501*time.Millisecond {
		t.Fatalf("idle timeout = %s", idleTimeout)
	}
	for _, raw := range []json.RawMessage{
		[]byte(`{"idleTimeoutMs":0}`),
		[]byte(`{"idleTimeoutMs":3600001}`),
		[]byte(`{"idleTimeoutMs":9223372036854775807}`),
		[]byte(`{"idleTimeoutMs":"30s"}`),
	} {
		if _, err := actorInputWaitIdleTimeout(raw); err == nil {
			t.Fatalf("invalid manifest accepted: %s", raw)
		}
	}
}

func TestDerivedRunWaitIDBindsAttemptIdentity(t *testing.T) {
	runID := uuid.Must(uuid.NewV7())
	correlationID := uuid.Must(uuid.NewV7())
	first := derivedRunWaitID(runID, 1, correlationID, "wait")
	if replay := derivedRunWaitID(runID, 1, correlationID, "wait"); replay != first {
		t.Fatalf("same Attempt replay ID = %s, want %s", replay, first)
	}
	if retry := derivedRunWaitID(runID, 2, correlationID, "wait"); retry == first {
		t.Fatal("retry Attempt reused the prior Run Wait ID")
	}
}

func TestValidateRootRunWaitActorCursor(t *testing.T) {
	actorID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	authority := runLeaseClaimAuthority{
		run:       db.Run{ID: runID, EntrypointKind: "actor", ActorID: actorID},
		actor:     db.Actor{ID: actorID, CurrentRunID: runID, State: "open", CommittedInputSequence: 4, NextInputSequence: 6},
		attempt:   db.RunAttempt{ActorStartInputSequence: pgtype.Int8{Int64: 3, Valid: true}},
		workspace: db.Workspace{OwnerActorID: actorID},
	}
	for _, cursor := range []int64{4, 5} {
		if err := validateRunWaitActorCursor(authority, db.RunWait{
			ActorSpeculativeInputSequence: pgtype.Int8{Int64: cursor, Valid: true},
		}); err != nil {
			t.Fatalf("cursor %d rejected: %v", cursor, err)
		}
	}
	for _, cursor := range []pgtype.Int8{{}, {Int64: 3, Valid: true}, {Int64: 6, Valid: true}} {
		if err := validateRunWaitActorCursor(authority, db.RunWait{ActorSpeculativeInputSequence: cursor}); err == nil {
			t.Fatalf("invalid cursor %+v was accepted", cursor)
		}
	}

	authority = runLeaseClaimAuthority{run: db.Run{EntrypointKind: "task"}}
	if err := validateRunWaitActorCursor(authority, db.RunWait{}); err != nil {
		t.Fatalf("Task NULL cursor rejected: %v", err)
	}
	if err := validateRunWaitActorCursor(authority, db.RunWait{
		ActorSpeculativeInputSequence: pgtype.Int8{Int64: 0, Valid: true},
	}); err == nil {
		t.Fatal("Task Actor cursor was accepted")
	}
}

func TestRunWaitDeadlinesEnforceTokenBounds(t *testing.T) {
	timeoutTooLong := maxTokenWaitTimeout.Milliseconds() + 1
	idleTooLong := maxTokenWaitIdleTimeout.Milliseconds() + 1
	for _, test := range []struct {
		name    string
		request api.WorkerCreateRunWaitRequest
	}{
		{name: "timeout", request: api.WorkerCreateRunWaitRequest{TimeoutMS: &timeoutTooLong}},
		{name: "idle timeout", request: api.WorkerCreateRunWaitRequest{IdleTimeoutMS: &idleTooLong}},
		{name: "timeout overflow", request: api.WorkerCreateRunWaitRequest{TimeoutMS: int64Pointer(math.MaxInt64)}},
		{name: "idle timeout overflow", request: api.WorkerCreateRunWaitRequest{IdleTimeoutMS: int64Pointer(math.MaxInt64)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, _, _, err := runWaitDeadlines(test.request, defaultTokenWaitIdleTimeout); err == nil {
				t.Fatal("out-of-range Token Wait deadline was accepted")
			}
		})
	}
}

func int64Pointer(value int64) *int64 { return &value }
