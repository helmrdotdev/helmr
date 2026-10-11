package runtimemcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTurnWaitSettlementAndRepeat(t *testing.T) {
	calls := 0
	invoke := func(_ context.Context, tool string, body json.RawMessage) (json.RawMessage, error) {
		if tool != "inspect_turn" || string(body) != `{"sessionId":"session","turnId":"turn"}` {
			t.Fatalf("unexpected call %s %s", tool, body)
		}
		calls++
		if calls == 1 {
			return json.RawMessage(`{"id":"turn","sessionId":"session","status":"finalizing"}`), nil
		}
		return json.RawMessage(`{"id":"turn","sessionId":"session","status":"completed","result":null,"response":[{"type":"text","text":"done"}]}`), nil
	}
	expected := `{"status":"settled","outcome":{"status":"completed","result":null,"response":[{"type":"text","text":"done"}]}}`
	for range 2 {
		actual, err := waitForTurn(t.Context(), invoke, waitInput{SessionID: "session", TurnID: "turn", TimeoutMS: 1000})
		if err != nil {
			t.Fatal(err)
		}
		var got, want any
		if err := json.Unmarshal(actual, &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(expected), &want); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("wait=%s", actual)
		}
	}
	if calls != 3 {
		t.Fatalf("reads=%d", calls)
	}
}

func TestTurnWaitTimeoutAndErrors(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		calls := 0
		actual, err := waitForTurn(t.Context(), func(ctx context.Context, tool string, _ json.RawMessage) (json.RawMessage, error) {
			if tool != "inspect_turn" {
				t.Fatalf("mutation %s", tool)
			}
			calls++
			if blocked {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return json.RawMessage(`{"id":"turn","sessionId":"session","status":"running"}`), nil
		}, waitInput{SessionID: "session", TurnID: "turn", TimeoutMS: 5})
		if err != nil || string(actual) != `{"status":"timeout"}` || calls != 1 {
			t.Fatalf("timeout %s %v %d", actual, err, calls)
		}
	}
	failure := errors.New("authority_changed")
	_, err := waitForTurn(t.Context(), func(context.Context, string, json.RawMessage) (json.RawMessage, error) { return nil, failure }, waitInput{SessionID: "session", TurnID: "turn", TimeoutMS: 1000})
	if !errors.Is(err, failure) {
		t.Fatalf("lost error: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	timer := time.AfterFunc(5*time.Millisecond, cancel)
	defer timer.Stop()
	_, err = waitForTurn(ctx, func(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, waitInput{SessionID: "session", TurnID: "turn", TimeoutMS: 1000})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation: %v", err)
	}
}

func TestTurnWaitValidatesSnapshotsAndPreservesExpiry(t *testing.T) {
	for _, raw := range []string{
		`{"id":"other","sessionId":"session","status":"completed"}`,
		`{"id":"turn","sessionId":"other","status":"completed"}`,
		`{"id":"turn","sessionId":"session","status":"finalizing","result":null}`,
		`{"id":"turn","sessionId":"session","status":"completed","payloadExpiredAt":"2026-10-07T00:00:00Z","result":null}`,
		`{"id":"turn","sessionId":"session","status":"unknown"}`,
	} {
		_, err := waitForTurn(t.Context(), func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(raw), nil
		}, waitInput{SessionID: "session", TurnID: "turn", TimeoutMS: 100})
		if err == nil {
			t.Fatalf("accepted invalid state %s", raw)
		}
	}
	actual, err := waitForTurn(t.Context(), func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{"id":"turn","sessionId":"session","status":"failed","error":{"code":"handler_failed"},"payloadExpiredAt":"2026-10-07T00:00:00Z"}`), nil
	}, waitInput{SessionID: "session", TurnID: "turn", TimeoutMS: 100})
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Status  string
		Outcome struct {
			Status           string
			Error            map[string]string
			PayloadExpiredAt time.Time
		}
	}
	if err := json.Unmarshal(actual, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "settled" || result.Outcome.Status != "failed" || result.Outcome.Error["code"] != "handler_failed" || len(result.Outcome.Error) != 1 || result.Outcome.PayloadExpiredAt.IsZero() {
		t.Fatalf("expired=%s", actual)
	}
}

func TestTurnWaitReportsKnownCapacityDependencyWithoutRetry(t *testing.T) {
	calls := 0
	_, err := waitForTurn(t.Context(), func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		calls++
		return json.RawMessage(`{"id":"turn","sessionId":"session","status":"queued","waitBlocked":"capacity_wait_blocked"}`), nil
	}, waitInput{SessionID: "session", TurnID: "turn", TimeoutMS: 1000})
	if err == nil || !strings.Contains(err.Error(), "capacity_wait_blocked") || calls != 1 {
		t.Fatalf("dependency calls=%d error=%v", calls, err)
	}
}
