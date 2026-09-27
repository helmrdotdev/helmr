package dispatch

import (
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgtype"
)

func commandPlacementFixture(t *testing.T) (runtest.Fixture, runtest.RunLease, *Authority) {
	t.Helper()
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	key, err := computer.NewFencingKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewRunAuthority(f.Pool, key)
	if err != nil {
		t.Fatal(err)
	}
	return f, work, a
}
func pendingSharedCommand(t *testing.T, f runtest.Fixture, work runtest.RunLease) ReadyComputerCommandCandidate {
	t.Helper()
	id, claim := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'computer.command.start',$3,$3,now())`, claim, f.EnvironmentID, dbtest.Hash(claim.String()))
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_commands(id,environment_id,computer_id,claim_id,argv,env,stdin,timeout_ms,created_by_subject_type,created_by_subject_id)
 SELECT $2,environment_id,computer_id,$3,ARRAY['true'],'{}',''::bytea,60000,'api_key',run_id::text FROM run_leases WHERE id=$1`, work.LeaseID, id, claim)
	return ReadyComputerCommandCandidate{OrgID: pgvalue.UUID(f.OrgID), CommandID: pgvalue.UUID(id), ExpectedRevision: 1}
}
func TestCommandsJoinRunningComputerWithoutNewWriter(t *testing.T) {
	f, work, a := commandPlacementFixture(t)
	first, second := pendingSharedCommand(t, f, work), pendingSharedCommand(t, f, work)
	// No physical slots remain, but joining the resident Instance needs no new VM.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET max_vm_slots=1 WHERE id=$1`, f.WorkerID)
	for _, candidate := range []ReadyComputerCommandCandidate{first, second} {
		p, err := a.PlaceComputerCommand(t.Context(), candidate)
		if err != nil {
			t.Fatal(err)
		}
		if !p.ProcessBound {
			t.Fatalf("Command was not bound: %+v", p)
		}
	}
	var members int
	var generation int64
	var distinct int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*),min(writer_generation),count(DISTINCT computer_instance_id) FROM computer_commands WHERE id=ANY($1::uuid[])`, []uuid.UUID{uuid.UUID(first.CommandID.Bytes), uuid.UUID(second.CommandID.Bytes)}).Scan(&members, &generation, &distinct); err != nil {
		t.Fatal(err)
	}
	if members != 2 || distinct != 1 || generation != 2 {
		t.Fatalf("members=%d instances=%d writer=%d", members, distinct, generation)
	}
	var revision int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT membership_revision FROM computer_instances WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != 2 {
		t.Fatalf("membership revision=%d", revision)
	}
	if _, err := a.PlaceComputerCommand(t.Context(), first); !errors.Is(err, ErrCandidateChanged) {
		t.Fatalf("duplicate placement=%v", err)
	}
}
func TestCommandPlacementRejectsExpiredWriterAndDrainingInstance(t *testing.T) {
	for _, expired := range []bool{true, false} {
		t.Run(map[bool]string{true: "expired", false: "draining"}[expired], func(t *testing.T) {
			f, work, a := commandPlacementFixture(t)
			candidate := pendingSharedCommand(t, f, work)
			if expired {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=now()-interval '1 second' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
			} else {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='draining' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
			}
			p, err := a.PlaceComputerCommand(t.Context(), candidate)
			if p.ProcessBound || (err != nil && !errors.Is(err, ErrCandidateChanged)) {
				t.Fatalf("placement=%+v err=%v", p, err)
			}
			var count int
			if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_commands WHERE id=$1 AND computer_instance_id IS NULL AND status='pending'`, candidate.CommandID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("pending=%d err=%v", count, err)
			}
		})
	}
}
func TestConcurrentCommandPlacementBindsOnce(t *testing.T) {
	f, work, a := commandPlacementFixture(t)
	candidate := pendingSharedCommand(t, f, work)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() { _, err := a.PlaceComputerCommand(t.Context(), candidate); results <- err })
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, ErrCandidateChanged) {
			t.Fatal(err)
		}
	}
	if success != 1 {
		t.Fatalf("successful placements=%d", success)
	}
	var membership int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT membership_revision FROM computer_instances WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID).Scan(&membership); err != nil {
		t.Fatal(err)
	}
	if membership != 1 {
		t.Fatalf("membership=%d", membership)
	}
}
func TestCommandPlacementAllocatesOneComputerInstance(t *testing.T) {
	f, work, a := commandPlacementFixture(t)
	candidates := []ReadyComputerCommandCandidate{pendingSharedCommand(t, f, work), pendingSharedCommand(t, f, work)}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=2,observed_state='closed',observed_desired_version=2,terminal_at=now(),terminal_reason_code='test_exclusion',reclaimed_at=now(),reclaim_evidence='{"method":"host_reconciled"}',admission_state='closed',mount_state='unmounted',unmounted_at=now() WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
	// Advertise the Computer's configured disk reservation in the test supply.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET epoch_guest_ephemeral_disk_bytes=68719476736,per_vm_guest_ephemeral_disk_bytes=34359738368 WHERE id=$1`, f.WorkerID)
	type outcome struct {
		placement ComputerCommandPlacement
		err       error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, candidate := range candidates {
		wg.Go(func() { p, err := a.PlaceComputerCommand(t.Context(), candidate); results <- outcome{p, err} })
	}
	wg.Wait()
	close(results)
	var instanceID pgtype.UUID
	for result := range results {
		if result.err != nil && !errors.Is(result.err, ErrCandidateChanged) {
			t.Fatal(result.err)
		}
		if result.err == nil {
			if result.placement.ProcessBound {
				t.Fatal("unprepared Command was bound")
			}
			if instanceID.Valid && instanceID != result.placement.ComputerInstanceID {
				t.Fatal("allocated competing Instances")
			}
			instanceID = result.placement.ComputerInstanceID
		}
	}
	if !instanceID.Valid {
		t.Fatal("no physical allocation succeeded")
	}
	for _, candidate := range candidates {
		p, err := a.PlaceComputerCommand(t.Context(), candidate)
		if err != nil || p.ComputerInstanceID != instanceID || p.ProcessBound {
			t.Fatalf("reused preparation=%+v err=%v", p, err)
		}
	}
	instance, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: instanceID})
	if err != nil {
		t.Fatal(err)
	}
	var count int
	var preparation pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT preparation_attempt_count,preparation_instance_id FROM computers WHERE id=$1`, instance.ComputerID).Scan(&count, &preparation); err != nil {
		t.Fatal(err)
	}
	if count != 1 || preparation != instanceID {
		t.Fatalf("concurrent admission charged %d attempts for %v", count, preparation)
	}
	if instance.WriterGeneration != 3 || instance.ProgramDeploymentID.Valid || instance.MembershipRevision != 0 || len(instance.WriterTokenHash) != 32 {
		t.Fatalf("allocation=%+v", instance)
	}
}

func TestCommandPlacementCannotCrossOrganization(t *testing.T) {
	f, work, a := commandPlacementFixture(t)
	candidate := pendingSharedCommand(t, f, work)
	candidate.OrgID = pgvalue.UUID(uuid.NewV7())
	if _, err := a.PlaceComputerCommand(t.Context(), candidate); !errors.Is(err, ErrCandidateChanged) {
		t.Fatalf("foreign organization placement=%v", err)
	}
	if err := a.FailPendingComputerCommand(t.Context(), candidate, "test_failure"); !errors.Is(err, ErrCandidateChanged) {
		t.Fatalf("foreign organization failure=%v", err)
	}
	var pending bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='pending' AND computer_instance_id IS NULL FROM computer_commands WHERE id=$1`, candidate.CommandID).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if !pending {
		t.Fatal("foreign organization mutated Command")
	}
}
