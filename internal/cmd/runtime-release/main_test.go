package main

import (
	"context"
	"testing"
)

func TestRuntimeReleaseCommandDispatchesPrivateVerifierBeforeCommands(t *testing.T) {
	handled, err := dispatchVerifier(
		[]string{"runtime-release", "__helmr-verifier"},
	)
	if !handled || err == nil {
		t.Fatalf("private verifier dispatch = (%t, %v)", handled, err)
	}
	handled, err = dispatchVerifier(
		[]string{"runtime-release", "verify-worker"},
	)
	if handled || err != nil {
		t.Fatalf("ordinary command dispatch = (%t, %v)", handled, err)
	}
}

func TestRuntimeReleaseCommandRequiresClosedArguments(t *testing.T) {
	tests := [][]string{
		nil,
		{"unknown"},
		{"compose"},
		{"archive"},
		{"worker"},
		{"verify-worker"},
		{"verify-archive"},
		{"verify-worker", "--input", "package.tar", "extra"},
	}
	for _, args := range tests {
		if err := run(context.Background(), args); err == nil {
			t.Fatalf("run(%q) returned nil error", args)
		}
	}
}
