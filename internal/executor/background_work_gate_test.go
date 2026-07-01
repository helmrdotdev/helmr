package executor

import (
	"context"
	"testing"
)

func TestBackgroundWorkGateRejectsBackgroundWhileForegroundActive(t *testing.T) {
	gate := NewBackgroundWorkGate()
	endForeground := gate.BeginForeground()
	defer endForeground()

	_, finish, ok := gate.BeginBackground(context.Background())
	defer finish()
	if ok {
		t.Fatalf("background work started while foreground workspaceMount was active")
	}
}

func TestBackgroundWorkGateCancelsBackgroundWhenForegroundStarts(t *testing.T) {
	gate := NewBackgroundWorkGate()
	ctx, finish, ok := gate.BeginBackground(context.Background())
	defer finish()
	if !ok {
		t.Fatalf("background work did not start while worker was idle")
	}

	endForeground := gate.BeginForeground()
	defer endForeground()

	select {
	case <-ctx.Done():
	default:
		t.Fatalf("background work was not cancelled when foreground workspaceMount started")
	}
}

func TestBackgroundWorkGateAllowsBackgroundAfterForegroundEnds(t *testing.T) {
	gate := NewBackgroundWorkGate()
	endForeground := gate.BeginForeground()
	endForeground()

	_, finish, ok := gate.BeginBackground(context.Background())
	defer finish()
	if !ok {
		t.Fatalf("background work did not start after foreground workspaceMount ended")
	}
}
