package worker

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestImageOperationRevocationsCancelOnlyMatchingLiveAttempt(t *testing.T) {
	registry := newImageOperationRevocations()
	first := uuid.Must(uuid.NewV7()).String()
	second := uuid.Must(uuid.NewV7()).String()
	if second < first {
		first, second = second, first
	}
	var firstCancelled atomic.Bool
	var secondCancelled atomic.Bool
	unregisterFirst, err := registry.RegisterImageOperation(first, func() {
		firstCancelled.Store(true)
	})
	if err != nil {
		t.Fatalf("register first: %v", err)
	}
	defer unregisterFirst()
	unregisterSecond, err := registry.RegisterImageOperation(second, func() {
		secondCancelled.Store(true)
	})
	if err != nil {
		t.Fatalf("register second: %v", err)
	}
	defer unregisterSecond()

	if err := registry.apply([]string{first}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !firstCancelled.Load() || secondCancelled.Load() {
		t.Fatalf("cancelled first=%v second=%v", firstCancelled.Load(), secondCancelled.Load())
	}
	if _, err := registry.RegisterImageOperation(first, func() {}); !errors.Is(err, errImageOperationRevoked) {
		t.Fatalf("register revoked operation error = %v", err)
	}
}

func TestImageOperationRevocationsAcceptRepeatedRenewalAndStoppedAttempt(t *testing.T) {
	registry := newImageOperationRevocations()
	operationID := uuid.Must(uuid.NewV7()).String()
	var cancellations atomic.Int32
	unregister, err := registry.RegisterImageOperation(operationID, func() {
		cancellations.Add(1)
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	unregister()
	if err := registry.apply([]string{operationID}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := registry.apply([]string{operationID}); err != nil {
		t.Fatalf("repeated apply: %v", err)
	}
	if cancellations.Load() != 0 {
		t.Fatalf("stopped attempt cancellation count = %d", cancellations.Load())
	}
}

func TestImageOperationRevocationsRejectMalformedResponse(t *testing.T) {
	first := uuid.Must(uuid.NewV7()).String()
	second := uuid.Must(uuid.NewV7()).String()
	if second < first {
		first, second = second, first
	}
	tests := []struct {
		name string
		ids  []string
	}{
		{name: "unsorted", ids: []string{second, first}},
		{name: "duplicate", ids: []string{first, first}},
		{name: "invalid", ids: []string{"not-a-uuid"}},
		{name: "over limit", ids: make([]string, maxRevokedImageOperationIDs+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := newImageOperationRevocations().apply(test.ids); err == nil {
				t.Fatal("apply returned nil error")
			}
		})
	}
}

func TestImageOperationRevocationsUnregisterIsGenerationSafe(t *testing.T) {
	registry := newImageOperationRevocations()
	operationID := uuid.Must(uuid.NewV7()).String()
	unregister, err := registry.RegisterImageOperation(operationID, func() {})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	unregister()
	var cancelled atomic.Bool
	newUnregister, err := registry.RegisterImageOperation(operationID, func() {
		cancelled.Store(true)
	})
	if err != nil {
		t.Fatalf("register replacement: %v", err)
	}
	defer newUnregister()
	unregister()
	if err := registry.apply([]string{operationID}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !cancelled.Load() {
		t.Fatal("stale unregister removed the replacement attempt")
	}
}

func TestImageOperationRevocationsRejectNilCancellation(t *testing.T) {
	operationID := uuid.Must(uuid.NewV7()).String()
	_, err := newImageOperationRevocations().RegisterImageOperation(operationID, nil)
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("register nil cancellation error = %v", err)
	}
}
