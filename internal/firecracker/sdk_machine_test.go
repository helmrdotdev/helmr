package firecracker

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestWithSDKClientTimeoutsScopesRoundedValue(t *testing.T) {
	t.Setenv(sdkInitTimeoutEnvironment, "7")
	t.Setenv(sdkRequestTimeoutEnvironment, "23")
	wantErr := errors.New("construction failed")
	err := withSDKClientTimeouts(1500*time.Millisecond, func() error {
		if got := os.Getenv(sdkInitTimeoutEnvironment); got != "2" {
			t.Fatalf("SDK initialization timeout = %q, want 2", got)
		}
		if got := os.Getenv(sdkRequestTimeoutEnvironment); got != "1500" {
			t.Fatalf("SDK request timeout = %q, want 1500", got)
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if got := os.Getenv(sdkInitTimeoutEnvironment); got != "7" {
		t.Fatalf("restored SDK initialization timeout = %q, want 7", got)
	}
	if got := os.Getenv(sdkRequestTimeoutEnvironment); got != "23" {
		t.Fatalf("restored request timeout=%q", got)
	}
}

func TestWithSDKClientTimeoutsRestoresAbsence(t *testing.T) {
	t.Setenv(sdkRequestTimeoutEnvironment, "")
	if err := os.Unsetenv(sdkRequestTimeoutEnvironment); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv(sdkInitTimeoutEnvironment); err != nil {
		t.Fatal(err)
	}
	if err := withSDKClientTimeouts(30*time.Second, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, exists := os.LookupEnv(sdkRequestTimeoutEnvironment); exists {
		t.Fatal("SDK request timeout environment remains set")
	}
	if _, exists := os.LookupEnv(sdkInitTimeoutEnvironment); exists {
		t.Fatal("SDK initialization timeout environment remains set")
	}
}
