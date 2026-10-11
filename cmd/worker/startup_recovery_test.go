package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/firecracker"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/worker"
)

func TestWorkerStartupRecoveryOrderingAndEvidence(t *testing.T) {
	owner := vm.Owner{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()}
	original := worker.RecoveryEvidence{
		ObservedAt: time.Unix(1234, 0), Reclaimed: []string{uuid.NewV7().String()},
		Quarantined: []string{owner.ID}, QuarantinedOwners: []vm.Owner{owner}, QuarantineErrors: []string{"retained cgroup"},
	}
	failure := errors.New("injected failure")
	for _, tc := range []struct {
		name      string
		failureAt string
		want      []string
	}{
		{"success", "", []string{"recover", "attachments", "qualify"}},
		{"inventory failure", "recover", []string{"recover"}},
		{"ownerless", "ownerless", []string{"recover"}},
		{"non-instance", "non-instance", []string{"recover"}},
		{"attachment custody", "attachments", []string{"recover", "attachments"}},
		{"qualification failure", "qualify", []string{"recover", "attachments", "qualify"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			evidence := original
			if tc.failureAt == "ownerless" {
				evidence.QuarantinedOwners = nil
			}
			if tc.failureAt == "non-instance" {
				evidence.QuarantinedOwners = []vm.Owner{{Kind: "unknown", ID: owner.ID}}
			}
			expectedRuntime := &firecracker.QualifiedRuntime{}
			var logs bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&logs, nil))
			got, runtime, err := recoverAndQualifyWorkerRuntime(t.Context(), log, func(context.Context) (worker.RecoveryEvidence, error) {
				calls = append(calls, "recover")
				if tc.failureAt == "recover" {
					return evidence, failure
				}
				return evidence, nil
			}, func() error {
				calls = append(calls, "attachments")
				if tc.failureAt == "attachments" {
					return failure
				}
				return nil
			}, func(_ context.Context, recovered worker.RecoveryEvidence) (*firecracker.QualifiedRuntime, error) {
				calls = append(calls, "qualify")
				if !reflect.DeepEqual(recovered, original) {
					t.Fatal("qualification lost original recovery evidence")
				}
				if tc.failureAt == "qualify" {
					return nil, failure
				}
				return expectedRuntime, nil
			})
			var diagnostic struct {
				Owners         []string `json:"owners"`
				RecoveryErrors []string `json:"recovery_errors"`
			}
			if decodeErr := json.Unmarshal(logs.Bytes(), &diagnostic); decodeErr != nil {
				t.Fatalf("startup lost local diagnostic: %v: %s", decodeErr, &logs)
			}
			if !reflect.DeepEqual(diagnostic.Owners, evidence.Quarantined) || !reflect.DeepEqual(diagnostic.RecoveryErrors, evidence.QuarantineErrors) {
				t.Fatalf("startup lost quarantine reason: %+v", diagnostic)
			}
			if !reflect.DeepEqual(calls, tc.want) {
				t.Fatalf("calls = %v, want %v", calls, tc.want)
			}
			if !reflect.DeepEqual(got, evidence) {
				t.Fatalf("startup replaced recovery evidence: %#v", got)
			}
			if tc.failureAt == "" {
				if err != nil || runtime != expectedRuntime {
					t.Fatalf("runtime=%p err=%v", runtime, err)
				}
			} else if err == nil || runtime != nil {
				t.Fatalf("failure allowed runtime: %p %v", runtime, err)
			}
		})
	}
}

func TestWorkerStartupProbeRespectsQuarantineCapacity(t *testing.T) {
	owner := vm.Owner{Kind: vm.OwnerInstance, ID: uuid.NewV7().String()}
	evidence := worker.RecoveryEvidence{Quarantined: []string{owner.ID}, QuarantinedOwners: []vm.Owner{owner}}
	perVM := reservation.Vector{CPUMillis: 1000, MemoryBytes: 1024, HostDiskBytes: 4096, VMSlots: 1}
	full := reservation.Vector{CPUMillis: 2000, MemoryBytes: 2048, HostDiskBytes: 8192, VMSlots: 2}
	for _, dimension := range []string{"cpu", "memory", "disk", "slots", "success", "probe failure"} {
		t.Run(dimension, func(t *testing.T) {
			capacity := full
			switch dimension {
			case "cpu":
				capacity.CPUMillis--
			case "memory":
				capacity.MemoryBytes--
			case "disk":
				capacity.HostDiskBytes--
			case "slots":
				capacity.VMSlots--
			}
			ledger, err := reservation.New(capacity)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			failure := errors.New("probe cleanup failed")
			_, err = qualifyRecoveredRuntime(t.Context(), evidence, ledger, perVM, func(context.Context) (*firecracker.QualifiedRuntime, error) {
				calls++
				snapshot := ledger.Snapshot()
				if snapshot.Used != full || len(snapshot.Reservations) != 2 {
					t.Fatalf("probe not fully reserved: %#v", snapshot)
				}
				if dimension == "probe failure" {
					return nil, failure
				}
				return &firecracker.QualifiedRuntime{}, nil
			})
			snapshot := ledger.Snapshot()
			switch dimension {
			case "success":
				if err != nil || calls != 1 || snapshot.Used != perVM || len(snapshot.Reservations) != 1 {
					t.Fatalf("successful probe did not release only itself: %#v calls=%d err=%v", snapshot, calls, err)
				}
			case "probe failure":
				if !errors.Is(err, failure) || calls != 1 || snapshot.Used != full {
					t.Fatalf("failed probe released capacity: %#v calls=%d err=%v", snapshot, calls, err)
				}
			default:
				if !errors.Is(err, reservation.ErrCapacityExceeded) || calls != 0 || snapshot.Used != perVM {
					t.Fatalf("underfunded probe ran: %#v calls=%d err=%v", snapshot, calls, err)
				}
			}
			if snapshot.Reservations[reservation.Key{Kind: "quarantine", Epoch: 1, ID: owner.ID}] != perVM {
				t.Fatal("quarantine charge was lost")
			}
		})
	}
}
