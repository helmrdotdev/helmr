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
