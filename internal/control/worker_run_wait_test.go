package control

import (
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
	timeoutAt, idleTimeout, checkpointDueAt, delay, err := runWaitDeadlines(api.WorkerCreateRunWaitRequest{})
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
		if err := validateRootRunWaitActorCursor(authority, db.RunWait{
			ActorSpeculativeInputSequence: pgtype.Int8{Int64: cursor, Valid: true},
		}); err != nil {
			t.Fatalf("cursor %d rejected: %v", cursor, err)
		}
	}
	for _, cursor := range []pgtype.Int8{{}, {Int64: 3, Valid: true}, {Int64: 6, Valid: true}} {
		if err := validateRootRunWaitActorCursor(authority, db.RunWait{ActorSpeculativeInputSequence: cursor}); err == nil {
			t.Fatalf("invalid cursor %+v was accepted", cursor)
		}
	}

	authority = runLeaseClaimAuthority{run: db.Run{EntrypointKind: "task"}}
	if err := validateRootRunWaitActorCursor(authority, db.RunWait{}); err != nil {
		t.Fatalf("Task NULL cursor rejected: %v", err)
	}
	if err := validateRootRunWaitActorCursor(authority, db.RunWait{
		ActorSpeculativeInputSequence: pgtype.Int8{Int64: 0, Valid: true},
	}); err == nil {
		t.Fatal("Task Actor cursor was accepted")
	}
}

func TestRunWaitDeadlinesEnforceTokenBounds(t *testing.T) {
	timeoutTooLong := int32(maxTokenWaitTimeout/time.Second) + 1
	idleTooLong := int32(maxTokenWaitIdleTimeout/time.Second) + 1
	for _, test := range []struct {
		name    string
		request api.WorkerCreateRunWaitRequest
	}{
		{name: "timeout", request: api.WorkerCreateRunWaitRequest{TimeoutSeconds: &timeoutTooLong}},
		{name: "idle timeout", request: api.WorkerCreateRunWaitRequest{IdleTimeoutSeconds: &idleTooLong}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, _, _, err := runWaitDeadlines(test.request); err == nil {
				t.Fatal("out-of-range Token Wait deadline was accepted")
			}
		})
	}
}
