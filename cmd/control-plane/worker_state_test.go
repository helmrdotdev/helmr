package main

import (
	"context"
	"io"
	"testing"
)

func TestWorkerStateCommandsRejectMalformedGroupBeforePersistence(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "group status", run: func() error {
			return runWorkerGroupStateCommand(context.Background(), io.Discard, []string{"status", "--group-id", "not-a-group"})
		}},
		{name: "group mutation", run: func() error {
			return runWorkerGroupStateCommand(context.Background(), io.Discard, []string{"pause", "--group-id", "not-a-group", "--expected-claim-version", "1"})
		}},
		{name: "instance status", run: func() error {
			return runWorkerInstanceStateCommand(context.Background(), io.Discard, []string{"status", "--group-id", "not-a-group", "--resource-id", "host-1"})
		}},
		{name: "instance mutation", run: func() error {
			return runWorkerInstanceStateCommand(context.Background(), io.Discard, []string{"lose", "--group-id", "not-a-group", "--resource-id", "host-1", "--expected-claim-version", "1"})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run()
			if err == nil || err.Error() != "worker group id must be a canonical UUIDv7" {
				t.Fatalf("error = %v, want canonical UUIDv7 rejection", err)
			}
		})
	}
}
