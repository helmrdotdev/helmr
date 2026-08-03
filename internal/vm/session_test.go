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
		{name: "validation", err: errors.New("the Firecracker filepack logical size 1 does not match expected 2"), want: "validation"},
		{name: "io", err: errors.New("unsupported Firecracker filepack format"), want: "io"},
		{name: "firecracker", err: errors.New("start Firecracker machine: failed"), want: "firecracker"},
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
	if err := (Owner{Kind: OwnerImageBuild, ID: valid.ID}).Validate(); err != nil {
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

func TestWorkloadBindingValidation(t *testing.T) {
	runtimeOwner := Owner{Kind: OwnerRuntime, ID: "019c10d5-a6f7-7af1-8f5f-000000000010"}
	runtimeBinding := WorkloadBinding{
		WorkerEpoch:       4,
		OwnerID:           runtimeOwner.ID,
		Generation:        1,
		RuntimeInstanceID: runtimeOwner.ID,
		RuntimeIdentityID: "runtime-identity",
	}
	if err := runtimeBinding.Validate(runtimeOwner); err != nil {
		t.Fatal(err)
	}
	buildOwner := Owner{Kind: OwnerBuild, ID: "019c10d5-a6f7-7af1-8f5f-000000000011"}
	buildBinding := WorkloadBinding{
		WorkerEpoch:       4,
		OwnerID:           buildOwner.ID,
		Generation:        3,
		RuntimeIdentityID: "runtime-identity",
	}
	if err := buildBinding.Validate(buildOwner); err != nil {
		t.Fatal(err)
	}
	imageOwner := Owner{Kind: OwnerImageBuild, ID: "019c10d5-a6f7-7af1-8f5f-000000000012"}
	imageBinding := WorkloadBinding{
		WorkerEpoch:       4,
		OwnerID:           imageOwner.ID,
		Generation:        1,
		RuntimeIdentityID: "runtime-identity",
	}
	if err := imageBinding.Validate(imageOwner); err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct {
		owner   Owner
		binding WorkloadBinding
	}{
		"owner mismatch": {runtimeOwner, func() WorkloadBinding { value := runtimeBinding; value.OwnerID = buildOwner.ID; return value }()},
		"runtime generation is not canonical": {runtimeOwner, func() WorkloadBinding {
			value := runtimeBinding
			value.Generation++
			return value
		}()},
		"runtime authority on build": {buildOwner, func() WorkloadBinding {
			value := buildBinding
			value.RuntimeInstanceID = runtimeOwner.ID
			return value
		}()},
		"missing runtime identity": {buildOwner, func() WorkloadBinding {
			value := buildBinding
			value.RuntimeIdentityID = ""
			return value
		}()},
		"image generation is not canonical": {imageOwner, func() WorkloadBinding {
			value := imageBinding
			value.Generation++
			return value
		}()},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if err := test.binding.Validate(test.owner); err == nil {
				t.Fatal("invalid workload binding was accepted")
			}
		})
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
