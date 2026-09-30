package dispatch

import (
	"errors"
	"math"
	"testing"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestInstanceAssignmentUsesSpecResources(t *testing.T) {
	for _, test := range []struct {
		name        string
		cpu, memory int64
		valid       bool
	}{
		{"valid", 1500, 2048, true}, {"zero CPU", 0, 1, false}, {"zero memory", 1, 0, false}, {"memory overflow", 1, math.MaxInt64, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, work, _ := commandAssignmentFixture(t)
			var computerID pgtype.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM runs WHERE id=$1`, work.RunID).Scan(&computerID); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_specs SET config=jsonb_set(config,'{resources}',jsonb_build_object('milliCpu',$2::bigint,'memoryMiB',$3::bigint)) WHERE id=(SELECT computer_spec_id FROM computers WHERE id=$1)`, computerID, test.cpu, test.memory)
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			assigned, err := discoverInstanceAssignment(t.Context(), tx, pgtype.UUID{Bytes: f.EnvironmentID, Valid: true}, computerID)
			if !test.valid {
				if err == nil {
					t.Fatalf("invalid resources accepted: %+v", assigned)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if assigned.cpu != 1500 || assigned.memory != 2048*1024*1024 || assigned.disk != compute.ComputerGuestEphemeralDiskMiB*1024*1024 {
				t.Fatalf("assignment resources cpu=%d memory=%d disk=%d", assigned.cpu, assigned.memory, assigned.disk)
			}
		})
	}
}

func TestInstanceAssignmentRejectsPerVMLimits(t *testing.T) {
	for _, limit := range []string{"per_vm_cpu_millis", "per_vm_memory_bytes", "per_vm_guest_ephemeral_disk_bytes"} {
		for _, member := range []string{"Run", "Command"} {
			t.Run(limit+"/"+member, func(t *testing.T) {
				f, work, a := commandAssignmentFixture(t)
				candidate := queuedSharedRun(t, f, work)
				command := pendingSharedCommand(t, f, work)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=2,observed_state='closed',observed_desired_version=2,terminal_at=now(),terminal_reason_code='test_exclusion',reclaimed_at=now(),reclaim_evidence='{"method":"host_reconciled"}',admission_state='closed',mount_state='unmounted',unmounted_at=now() WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET epoch_guest_ephemeral_disk_bytes=68719476736,per_vm_guest_ephemeral_disk_bytes=34359738368 WHERE id=$1`, f.WorkerID)
				var previous int64
				if err := f.Pool.QueryRow(t.Context(), `SELECT `+limit+` FROM worker_hosts WHERE id=$1`, f.WorkerID).Scan(&previous); err != nil {
					t.Fatal(err)
				}
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET `+limit+`=1 WHERE id=$1`, f.WorkerID)
				snapshot := func() string {
					t.Helper()
					var value string
					if err := f.Pool.QueryRow(t.Context(), `SELECT jsonb_build_object('computer',to_jsonb(c),'instances',(SELECT jsonb_agg(to_jsonb(i) ORDER BY id) FROM computer_instances i WHERE i.computer_id=c.id))::text FROM computers c JOIN runs r ON r.computer_id=c.id WHERE r.id=$1`, candidate.RunID).Scan(&value); err != nil {
						t.Fatal(err)
					}
					return value
				}
				assign := func() error {
					if member == "Run" {
						_, e := a.AssignRun(t.Context(), candidate)
						return e
					}
					_, e := a.AssignCommand(t.Context(), command)
					return e
				}
				before := snapshot()
				if err := assign(); !errors.Is(err, ErrCapacityUnavailable) {
					t.Fatalf("incompatible capacity accepted: %v", err)
				}
				if after := snapshot(); after != before {
					t.Fatal("rejected assignment changed Computer or allocated an Instance")
				}
				// Prove the same candidate is otherwise eligible once this limit is restored.
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET `+limit+`=$2 WHERE id=$1`, f.WorkerID, previous)
				if err := assign(); err != nil {
					t.Fatalf("compatible capacity rejected: %v", err)
				}
			})
		}
	}
}
