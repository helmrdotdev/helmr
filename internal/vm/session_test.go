package vm

import (
	"context"
	"errors"
	"testing"
)

func TestRuntimeErrorClass(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", want: ""},
		{name: "canceled", err: context.Canceled, want: "context_canceled"},
		{name: "deadline", err: context.DeadlineExceeded, want: "context_deadline_exceeded"},
		{name: "health", err: errors.New("guest health probe timed out after 5m0s"), want: "guest_health"},
		{name: "validation", err: errors.New("firecracker filepack logical size 1 does not match expected 2"), want: "validation"},
		{name: "io", err: errors.New("unsupported firecracker filepack format"), want: "io"},
		{name: "firecracker", err: errors.New("start firecracker machine: failed"), want: "firecracker"},
		{name: "unknown", err: errors.New("unexpected state"), want: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RuntimeErrorClass(tt.err); got != tt.want {
				t.Fatalf("RuntimeErrorClass() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOwnerValidation(t *testing.T) {
	valid := Owner{Kind: OwnerBuild, ID: "019c10d5-a6f7-7af1-8f5f-000000000001"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []Owner{
		{Kind: "other", ID: valid.ID},
		{Kind: OwnerRuntime, ID: "not-a-uuid"},
		{Kind: OwnerRuntime, ID: "019c10d5-a6f7-7af1-8f5f-000000000ABC"},
	} {
		if err := owner.Validate(); err == nil {
			t.Fatalf("Owner.Validate() accepted %+v", owner)
		}
	}
}

func TestCleanupUnprovenErrorPreservesOwnerAndCause(t *testing.T) {
	cause := errors.New("marker mismatch")
	owner := Owner{Kind: OwnerRuntime, ID: "019c10d5-a6f7-7af1-8f5f-000000000002"}
	err := &CleanupUnprovenError{Owner: owner, Cause: cause}
	var got *CleanupUnprovenError
	if !errors.As(err, &got) || got.Owner != owner || !errors.Is(err, cause) {
		t.Fatalf("CleanupUnprovenError = %#v", err)
	}
}
