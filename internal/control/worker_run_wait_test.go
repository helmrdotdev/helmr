package control

import (
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
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
