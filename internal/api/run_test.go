package api

import "testing"

func TestRunStatusIsTerminal(t *testing.T) {
	for _, status := range []string{
		RunStatusSucceeded,
		RunStatusFailed,
		RunStatusCancelled,
		RunStatusExpired,
		RunStatusSystemFailed,
	} {
		if !RunStatusIsTerminal(status) {
			t.Fatalf("%q must be terminal", status)
		}
	}
	for _, status := range []string{
		RunStatusQueued,
		RunStatusRunning,
		RunStatusWaiting,
		RunStatusRetryDelayed,
		RunStatusCancelRequested,
	} {
		if RunStatusIsTerminal(status) {
			t.Fatalf("%q must not be terminal", status)
		}
	}
}
