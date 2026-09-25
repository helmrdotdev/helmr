package controlplane

import (
	"encoding/json"
	"math"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestRunWaitDeadlinesApplyTokenDefaults(t *testing.T) {
	before := time.Now().UTC()
	timeoutAt, idleTimeout, checkpointDueAt, err := runWaitDeadlines(workerapi.CreateRunWaitRequest{}, defaultRunWaitIdleTimeout)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	if timeoutAt.Valid {
		t.Fatal("omitted Token Wait timeout became terminal deadline")
	}
	if !idleTimeout.Valid || idleTimeout.Int64 != defaultRunWaitIdleTimeout.Milliseconds() {
		t.Fatalf("idle timeout = %+v, want %dms", idleTimeout, defaultRunWaitIdleTimeout.Milliseconds())
	}
	if !checkpointDueAt.Valid || checkpointDueAt.Time.Before(before.Add(defaultRunWaitIdleTimeout)) ||
		checkpointDueAt.Time.After(after.Add(defaultRunWaitIdleTimeout)) {
		t.Fatalf("checkpoint deadline = %s, want registration time + default idle", checkpointDueAt.Time)
	}
}

func TestRunWaitDeadlinesPreserveMillisecondPrecision(t *testing.T) {
	timeoutMS := int64(1)
	idleTimeoutMS := int64(1501)
	before := time.Now().UTC()
	timeoutAt, idleTimeout, checkpointDueAt, err := runWaitDeadlines(workerapi.CreateRunWaitRequest{
		TimeoutMS: &timeoutMS, IdleTimeoutMS: &idleTimeoutMS,
	}, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	if !timeoutAt.Valid || timeoutAt.Time.Before(before.Add(time.Millisecond)) || timeoutAt.Time.After(after.Add(time.Millisecond)) {
		t.Fatalf("timeout_at = %s, want registration time + 1ms", timeoutAt.Time)
	}
	expectedDelay := time.Duration(idleTimeoutMS) * time.Millisecond
	if !idleTimeout.Valid || idleTimeout.Int64 != idleTimeoutMS || !checkpointDueAt.Valid ||
		checkpointDueAt.Time.Before(before.Add(expectedDelay)) || checkpointDueAt.Time.After(after.Add(expectedDelay)) {
		t.Fatalf("idle/checkpoint = %+v/%s", idleTimeout, checkpointDueAt.Time)
	}
}

func TestTimerWaitDeadlinesSeparateDueAtFromFailureTimeout(t *testing.T) {
	timeoutMS := int64(1501)
	duration := "1501ms"
	before := time.Now().UTC()
	params, dueAt, idleTimeout, checkpointDueAt, err := timerWaitDeadlines(
		workerapi.CreateRunWaitRequest{
			Params:    json.RawMessage(`{"duration":"1501ms"}`),
			TimeoutMS: &timeoutMS,
		}, defaultRunWaitIdleTimeout,
	)
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	if params.Duration == nil || *params.Duration != duration || params.Date != nil {
		t.Fatalf("normalized params = %+v", params)
	}
	if dueAt.Before(before.Add(1501*time.Millisecond)) ||
		dueAt.After(after.Add(1501*time.Millisecond)) {
		t.Fatalf("due_at = %s, want registration time + 1501ms", dueAt)
	}
	if !idleTimeout.Valid || idleTimeout.Int64 != defaultRunWaitIdleTimeout.Milliseconds() {
		t.Fatalf("idle timeout = %+v", idleTimeout)
	}
	expectedDelay := defaultRunWaitIdleTimeout
	if !checkpointDueAt.Valid || checkpointDueAt.Time.Before(before.Add(expectedDelay)) ||
		checkpointDueAt.Time.After(after.Add(expectedDelay)) {
		t.Fatalf("checkpoint deadline = %s", checkpointDueAt.Time)
	}
}

func TestTimerWaitUntilKeepsAbsoluteDueAt(t *testing.T) {
	timeoutMS := int64(time.Minute / time.Millisecond)
	date := time.Now().UTC().Add(time.Minute).Truncate(time.Millisecond)
	paramsJSON, err := json.Marshal(map[string]string{"date": date.Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	params, dueAt, _, _, err := timerWaitDeadlines(workerapi.CreateRunWaitRequest{
		Params: paramsJSON, TimeoutMS: &timeoutMS,
	}, defaultRunWaitIdleTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if !dueAt.Equal(date) || params.Date == nil || *params.Date != date.Format(time.RFC3339Nano) {
		t.Fatalf("absolute timer = %s params=%+v, want %s", dueAt, params, date)
	}
}

func TestTimerWaitDeadlinesRejectAmbiguousOrInconsistentInput(t *testing.T) {
	oneSecond := int64(1000)
	for _, raw := range []json.RawMessage{
		[]byte(`{}`),
		[]byte(`{"duration":"1s","date":"2026-01-01T00:00:00Z"}`),
		[]byte(`{"duration":"1000"}`),
		[]byte(`{"duration":"1s","unexpected":true}`),
	} {
		if _, _, _, _, err := timerWaitDeadlines(workerapi.CreateRunWaitRequest{
			Params: raw, TimeoutMS: &oneSecond,
		}, defaultRunWaitIdleTimeout); err == nil {
			t.Fatalf("invalid timer params accepted: %s", raw)
		}
	}
	mismatch := int64(999)
	if _, _, _, _, err := timerWaitDeadlines(workerapi.CreateRunWaitRequest{
		Params: json.RawMessage(`{"duration":"1s"}`), TimeoutMS: &mismatch,
	}, defaultRunWaitIdleTimeout); err == nil {
		t.Fatal("timer duration and timeout mismatch was accepted")
	}
}

func TestActorInputWaitIdleTimeoutReadsFullImmutableManifest(t *testing.T) {
	idleTimeout, err := actorWaitIdleTimeout([]byte(`{
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
		if _, err := actorWaitIdleTimeout(raw); err == nil {
			t.Fatalf("invalid manifest accepted: %s", raw)
		}
	}
}

func TestParseRequestedRunWaitIdentity(t *testing.T) {
	request := workerapi.CreateRunWaitRequest{
		CorrelationID:  uuid.NewV7().String(),
		RunWaitID:      uuid.NewV7().String(),
		ResumeAttachID: uuid.NewV7().String(),
	}
	identity, err := parseRequestedRunWaitIdentity(request)
	if err != nil {
		t.Fatal(err)
	}
	if identity.correlationID.String() != request.CorrelationID ||
		identity.waitID.String() != request.RunWaitID ||
		identity.resumeAttachID.String() != request.ResumeAttachID {
		t.Fatalf("identity = %+v", identity)
	}
	for _, invalid := range []workerapi.CreateRunWaitRequest{
		{CorrelationID: uuid.New().String(), RunWaitID: request.RunWaitID, ResumeAttachID: request.ResumeAttachID},
		{CorrelationID: request.CorrelationID, RunWaitID: " " + request.RunWaitID, ResumeAttachID: request.ResumeAttachID},
		{CorrelationID: request.CorrelationID, RunWaitID: request.RunWaitID, ResumeAttachID: request.RunWaitID},
	} {
		if _, err := parseRequestedRunWaitIdentity(invalid); err == nil {
			t.Fatalf("invalid identity accepted: %+v", invalid)
		}
	}
}

func TestValidateRootRunWaitActorCursor(t *testing.T) {
	actorID := pgvalue.UUID(uuid.NewV7())
	runID := pgvalue.UUID(uuid.NewV7())
	authority := runLeaseClaimAuthority{
		run:       db.Run{ID: runID, EntrypointKind: "actor", SessionID: actorID},
		actor:     db.Session{ID: actorID, CurrentRunID: runID, Status: "open", CommittedInputSequence: 4, NextInputSequence: 6},
		attempt:   db.RunAttempt{SessionInputStartSequence: pgtype.Int8{Int64: 3, Valid: true}},
		workspace: db.LockRunLeaseClaimWorkspaceRow{OwnerSessionID: actorID},
	}
	if err := validateRunWaitActorCursor(authority, db.RunWait{
		Kind: db.WaitKindToken, ActorSpeculativeInputSequence: pgtype.Int8{Int64: 4, Valid: true},
	}); err != nil {
		t.Fatalf("committed cursor rejected outside a Turn: %v", err)
	}
	for _, cursor := range []pgtype.Int8{{}, {Int64: 3, Valid: true}, {Int64: 5, Valid: true}, {Int64: 6, Valid: true}} {
		if err := validateRunWaitActorCursor(authority, db.RunWait{Kind: db.WaitKindToken, ActorSpeculativeInputSequence: cursor}); err == nil {
			t.Fatalf("invalid unbound cursor %+v was accepted", cursor)
		}
	}
	authority.actor.ActiveTurnID = pgvalue.UUID(uuid.NewV7())
	authority.actor.RunGeneration = 9
	bound := db.RunWait{
		Kind: db.WaitKindToken, TurnID: authority.actor.ActiveTurnID,
		TurnSessionID: actorID, TurnRunGeneration: pgtype.Int8{Int64: 9, Valid: true},
		ActorSpeculativeInputSequence: pgtype.Int8{Int64: 5, Valid: true},
	}
	if err := validateRunWaitActorCursor(authority, bound); err != nil {
		t.Fatalf("active Turn cursor rejected: %v", err)
	}
	bound.ActorSpeculativeInputSequence.Int64 = 4
	if err := validateRunWaitActorCursor(authority, bound); err == nil {
		t.Fatal("active Turn accepted previous committed cursor")
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
	timeoutTooLong := maxRunWaitDuration.Milliseconds() + 1
	idleTooLong := maxRunWaitIdleTimeout.Milliseconds() + 1
	for _, test := range []struct {
		name    string
		request workerapi.CreateRunWaitRequest
	}{
		{name: "timeout", request: workerapi.CreateRunWaitRequest{TimeoutMS: &timeoutTooLong}},
		{name: "idle timeout", request: workerapi.CreateRunWaitRequest{IdleTimeoutMS: &idleTooLong}},
		{name: "timeout overflow", request: workerapi.CreateRunWaitRequest{TimeoutMS: new(int64(math.MaxInt64))}},
		{name: "idle timeout overflow", request: workerapi.CreateRunWaitRequest{IdleTimeoutMS: new(int64(math.MaxInt64))}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, _, err := runWaitDeadlines(test.request, defaultRunWaitIdleTimeout); err == nil {
				t.Fatal("out-of-range Token Wait deadline was accepted")
			}
		})
	}
}

func TestRunWaitIdleDeadlineIndependentOfResponseTimeout(t *testing.T) {
	for _, timeout := range []*int64{nil, new(int64(time.Second / time.Millisecond)), new(int64(time.Hour / time.Millisecond))} {
		idle := int64((5 * time.Minute).Milliseconds())
		before := time.Now()
		_, actual, due, err := runWaitDeadlines(workerapi.CreateRunWaitRequest{IdleTimeoutMS: &idle, TimeoutMS: timeout}, defaultRunWaitIdleTimeout)
		after := time.Now()
		if err != nil || actual.Int64 != idle || due.Time.Before(before.Add(5*time.Minute)) || due.Time.After(after.Add(5*time.Minute)) {
			t.Fatalf("idle clipped by response timeout %v: idle=%+v due=%s error=%v", timeout, actual, due.Time, err)
		}
	}
}
