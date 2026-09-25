package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWaitForDrainCompletionAcceptsMatchingReceipt(t *testing.T) {
	dir := t.TempDir()
	result := make(chan error, 1)
	go func() {
		result <- waitForDrainCompleteMarker(t.Context(), dir, "worker-current", time.Second, time.Millisecond)
	}()
	// The waiter consumes the persisted receipt without a remote status request.
	if err := writeDrainCompleteMarker(dir, "worker-current"); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestWaitForDrainCompletionRejectsInvalidReceipts(t *testing.T) {
	for _, payload := range []string{"worker-previous\n", ""} {
		t.Run(payload, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, drainCompleteMarkerName), []byte(payload), 0o600); err != nil {
				t.Fatal(err)
			}
			err := waitForDrainCompleteMarker(t.Context(), dir, "worker-current", time.Second, time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), "different worker") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestWaitForDrainCompletionDoesNotInferSuccessWithoutReceipt(t *testing.T) {
	err := waitForDrainCompleteMarker(t.Context(), t.TempDir(), "worker-current", time.Millisecond, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitForDrainCompleteMarker(ctx, t.TempDir(), "worker-current", time.Second, time.Millisecond); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestWaitForDrainCompletionReportsUnreadableReceipt(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, drainCompleteMarkerName), 0o700); err != nil {
		t.Fatal(err)
	}
	err := waitForDrainCompleteMarker(t.Context(), dir, "worker-current", time.Second, time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "read drain completion marker") {
		t.Fatalf("got %v", err)
	}
}
