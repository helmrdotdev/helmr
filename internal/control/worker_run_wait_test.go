package control

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
)

func TestWorkerRunWaitCreateScopeUsesTypedPhysicalFences(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := finalWorkerRunLease(workerID)
	actor := finalWorkerActor(workerID)

	params, err := workerRunWaitCreateScopeParams(actor, lease)
	if err != nil {
		t.Fatal(err)
	}
	if params.WorkerGroupID != actor.WorkerGroupID || params.WorkerEpoch != actor.WorkerEpoch ||
		params.NetworkSlotGeneration != lease.NetworkSlotGeneration {
		t.Fatalf("scope params = %+v", params)
	}
}

func TestWorkerRunWaitCreateScopeRejectsAnotherWorkerEpoch(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := finalWorkerRunLease(workerID)
	actor := finalWorkerActor(workerID)
	actor.WorkerEpoch++

	if _, err := workerRunWaitCreateScopeParams(actor, lease); err == nil {
		t.Fatal("expected worker epoch mismatch to fail")
	}
}

func TestSelectWorkerRunWaitPolicy(t *testing.T) {
	short := int32(3)
	long := int32(60)
	idle := int32(8)
	tests := []struct {
		name   string
		input  api.WorkerCreateRunWaitRequest
		delay  time.Duration
		reason workerRunWaitPolicyReason
	}{
		{name: "interactive hot window", input: api.WorkerCreateRunWaitRequest{Kind: api.WorkerRunWaitKindToken}, delay: interactiveLiveWaitDelay, reason: workerRunWaitPolicyInteractiveHotWindow},
		{name: "interactive idle timeout", input: api.WorkerCreateRunWaitRequest{Kind: api.WorkerRunWaitKindStream, IdleTimeoutSeconds: &idle}, delay: 8 * time.Second, reason: workerRunWaitPolicyInteractiveIdleTimeout},
		{name: "short timer", input: api.WorkerCreateRunWaitRequest{Kind: api.WorkerRunWaitKindTimer, TimeoutSeconds: &short}, delay: 4 * time.Second, reason: workerRunWaitPolicyShortTimerLiveUntilFire},
		{name: "long timer", input: api.WorkerCreateRunWaitRequest{Kind: api.WorkerRunWaitKindTimer, TimeoutSeconds: &long}, delay: defaultLiveWaitCheckpointDelay, reason: workerRunWaitPolicyLongTimerPark},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := selectWorkerRunWaitPolicy(tc.input)
			if got.CheckpointDelay != tc.delay || got.Reason != tc.reason {
				t.Fatalf("policy = %+v, want delay=%s reason=%s", got, tc.delay, tc.reason)
			}
		})
	}
}

func TestWorkerRunWaitResumeDecisionUsesTerminalWaitState(t *testing.T) {
	kind, payload, err := workerRunWaitResumeDecision(db.Wait{State: db.WaitStateCompleted, Result: []byte(`{"ok":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if kind != "completed" || string(payload) != `{"ok":true}` {
		t.Fatalf("decision = %q %s", kind, payload)
	}
	if _, _, err := workerRunWaitResumeDecision(db.Wait{State: db.WaitStatePending}); err == nil {
		t.Fatal("expected non-terminal wait to fail")
	}
}

func TestWorkerRunWaitPresentationRejectsInvalidMetadata(t *testing.T) {
	if _, _, err := workerRunWaitPresentation(json.RawMessage(`[]`), nil); err == nil {
		t.Fatal("expected non-object metadata to fail")
	}
}

func TestWorkerRunWaitPresentationNormalizesAbsentValues(t *testing.T) {
	metadata, tags, err := workerRunWaitPresentation(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(metadata) != `{}` {
		t.Fatalf("metadata = %s, want {}", metadata)
	}
	if tags == nil || len(tags) != 0 {
		t.Fatalf("tags = %#v, want non-nil empty slice", tags)
	}
}
