package firecracker

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestWithSDKInitTimeoutScopesRoundedValue(t *testing.T) {
	t.Setenv(sdkInitTimeoutEnvironment, "7")
	wantErr := errors.New("construction failed")
	err := withSDKInitTimeout(1500*time.Millisecond, func() error {
		if got := os.Getenv(sdkInitTimeoutEnvironment); got != "2" {
			t.Fatalf("SDK initialization timeout = %q, want 2", got)
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if got := os.Getenv(sdkInitTimeoutEnvironment); got != "7" {
		t.Fatalf("restored SDK initialization timeout = %q, want 7", got)
	}
}

func TestWithSDKInitTimeoutRestoresAbsence(t *testing.T) {
	if err := os.Unsetenv(sdkInitTimeoutEnvironment); err != nil {
		t.Fatal(err)
	}
	if err := withSDKInitTimeout(30*time.Second, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, exists := os.LookupEnv(sdkInitTimeoutEnvironment); exists {
		t.Fatal("SDK initialization timeout environment remains set")
	}
}
