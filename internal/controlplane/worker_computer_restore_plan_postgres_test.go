package controlplane

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/dispatch/dispatchtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

func restorePlanFixture(t *testing.T, idle, committed bool, setup ...func(runtest.Fixture, runtest.RunLease)) (runtest.Fixture, workergroup.HostPrincipal, workerapi.ComputerRestorePlanRequest, disk.FencingKey) {
	t.Helper()
	f, authority, fence := dispatchtest.Restore(t, idle, setup...)
	key, err := disk.NewFencingKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{ID: pgvalue.UUID(fence.ID), EnvironmentID: pgvalue.UUID(f.EnvironmentID)})
	if err != nil {
		t.Fatal(err)
	}
	hash, err := computer.WriterTokenHash(key, fence.ID, pgvalue.MustUUIDValue(i.ComputerID), i.WriterGeneration)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_token_hash=$2 WHERE id=$1`, i.ID, hash)
	w := workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1}
	if err := f.Pool.QueryRow(t.Context(), `SELECT w.claim_version,g.claim_version FROM worker_hosts w JOIN worker_groups g ON g.id=w.worker_group_id WHERE w.id=$1`, f.WorkerID).Scan(&w.HostClaimVersion, &w.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	if committed {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		if _, err := authority.CommitRestore(t.Context(), tx, fence); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	return f, w, workerapi.ComputerRestorePlanRequest{EnvironmentID: f.EnvironmentID.String(), ComputerInstanceID: pgvalue.UUIDString(i.ID), WriterGeneration: i.WriterGeneration}, key
}

func readRestorePlan(t *testing.T, f runtest.Fixture, w workergroup.HostPrincipal, r workerapi.ComputerRestorePlanRequest, k disk.FencingKey) (*workerapi.ComputerRestorePlan, error) {
	t.Helper()
	plan, err := computer.ReadRestorePlan(t.Context(), f.Pool, k, w, computer.WriterRef{EnvironmentID: uuid.MustParse(r.EnvironmentID), InstanceID: uuid.MustParse(r.ComputerInstanceID), WriterGeneration: r.WriterGeneration})
	return workerRestorePlan(plan), err
}

func activateRestorePlanFixture(t *testing.T, f runtest.Fixture, w workergroup.HostPrincipal, plan *workerapi.ComputerRestorePlan) {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	grants := make([]dispatch.RestoreGrant, 0, len(plan.Members))
	for _, member := range plan.Members {
		grants = append(grants, dispatch.RestoreGrant{RunID: pgvalue.UUID(uuid.MustParse(member.RunID)), LeaseID: pgvalue.UUID(uuid.MustParse(member.Lease.ID)), LeaseSequence: member.Lease.LeaseSequence})
	}
	destination := computer.InstanceRef{Host: computer.Host{GroupID: w.GroupID, HostID: w.HostID, Epoch: w.Epoch}, ID: uuid.MustParse(plan.ComputerInstanceID), DesiredVersion: plan.DesiredVersion}
	_, err = dispatch.AcknowledgeRestore(t.Context(), tx, destination, pgvalue.UUID(uuid.MustParse(plan.CheckpointID)), plan.WriterGeneration, grants)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestRestoredClaimDiscoversAndAttachesWithoutStartingAnotherProgram(t *testing.T) {
	f, w, r, k := restorePlanFixture(t, false, true)
	plan, err := readRestorePlan(t, f, w, r, k)
	if err != nil {
		t.Fatal(err)
	}
	activateRestorePlanFixture(t, f, w, plan)
	work, err := db.New(f.Pool).DiscoverWorkerRunLeaseWork(t.Context(), db.DiscoverWorkerRunLeaseWorkParams{WorkerHostID: pgvalue.UUID(w.HostID), WorkerGroupID: pgvalue.UUID(w.GroupID), WorkerEpoch: w.Epoch, RowLimit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(work) != len(plan.Members) {
		t.Fatalf("restored leases discovered=%d", len(work))
	}
	server := &Server{tx: f.Pool}
	for _, member := range plan.Members {
		for range 2 {
			a, secrets, err := server.claimRunLease(t.Context(), w, pgvalue.UUID(uuid.MustParse(member.Lease.ID)), member.Lease.LeaseSequence)
			if err != nil {
				t.Fatal(err)
			}
			if a.resumeWait == nil || a.run.Status != "waiting" || a.runLease.Status != "running" || len(secrets) != 0 {
				t.Fatal("restored claim changed execution or reinjected secrets")
			}
			response, err := projectRestoredRunLeaseClaim(a, k)
			if err != nil {
				t.Fatal(err)
			}
			if response.ProgramResume == nil || response.ProgramResume.CheckpointID != plan.CheckpointID || len(response.ProgramStart) != 0 || len(response.Secrets) != 0 {
				t.Fatal("restore projection could start another Program")
			}
		}
	}
}
func TestRestoredClaimRejectsUnactivatedAndAlreadyAcknowledgedMembers(t *testing.T) {
	for _, activated := range []bool{false, true} {
		t.Run(map[bool]string{false: "not activated", true: "already attached"}[activated], func(t *testing.T) {
			f, w, r, k := restorePlanFixture(t, false, true)
			plan, err := readRestorePlan(t, f, w, r, k)
			if err != nil {
				t.Fatal(err)
			}
			member := plan.Members[0]
			if activated {
				activateRestorePlanFixture(t, f, w, plan)
				a, _, err := (&Server{tx: f.Pool}).claimRunLease(t.Context(), w, pgvalue.UUID(uuid.MustParse(member.Lease.ID)), member.Lease.LeaseSequence)
				if err != nil {
					t.Fatal(err)
				}
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(context.Background())
				_, err = run.AcknowledgeWaitResume(t.Context(), tx, run.ExecutionFence{LeaseID: a.runLease.ID, LeaseSequence: member.Lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(w.GroupID), WorkerHostID: pgvalue.UUID(w.HostID), WorkerEpoch: w.Epoch, GroupClaimVersion: w.GroupClaimVersion, HostClaimVersion: w.HostClaimVersion}, a.resumeWait.ID, a.runtime.SourceCheckpointID)
				if err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err = (&Server{tx: f.Pool}).claimRunLease(t.Context(), w, pgvalue.UUID(uuid.MustParse(member.Lease.ID)), member.Lease.LeaseSequence)
			if !errors.Is(err, errStaleRunLeaseClaim) {
				t.Fatalf("unsafe restore claim: %v", err)
			}
		})
	}
}

func TestRestoredClaimSurvivesDrain(t *testing.T) {
	f, w, r, k := restorePlanFixture(t, false, true)
	plan, err := readRestorePlan(t, f, w, r, k)
	if err != nil {
		t.Fatal(err)
	}
	activateRestorePlanFixture(t, f, w, plan)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='draining' WHERE id=$1`, r.ComputerInstanceID)
	_, _, err = (&Server{tx: f.Pool}).claimRunLease(t.Context(), w, pgvalue.UUID(uuid.MustParse(plan.Members[0].Lease.ID)), plan.Members[0].Lease.LeaseSequence)
	if err != nil {
		t.Fatal(err)
	}
}
func TestRestoredActorCanAcknowledgeStopBeforeAndAfterClaim(t *testing.T) {
	for _, activeTurn := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "active Turn"}[activeTurn], func(t *testing.T) {
			for _, stopBeforeClaim := range []bool{true, false} {
				t.Run(map[bool]string{true: "before claim", false: "before ack"}[stopBeforeClaim], func(t *testing.T) {
					var session uuid.UUID
					f, w, r, k := restorePlanFixture(t, false, true, func(f runtest.Fixture, work runtest.RunLease) {
						session = restoredActorFixture(t, f, work, activeTurn)
					})
					plan, err := readRestorePlan(t, f, w, r, k)
					if err != nil {
						t.Fatal(err)
					}
					activateRestorePlanFixture(t, f, w, plan)
					var leaseID uuid.UUID
					if err := f.Pool.QueryRow(t.Context(), `SELECT r.current_run_lease_id FROM runs r JOIN sessions s ON s.current_run_id=r.id WHERE s.id=$1`, session).Scan(&leaseID); err != nil {
						t.Fatal(err)
					}
					stop := func() {
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET dispatch_hold_id=$2,dispatch_hold_reason='interrupt_requested',dispatch_hold_run_id=current_run_id,dispatch_hold_attempt_number=1,dispatch_hold_run_generation=run_generation WHERE id=$1`, session, uuid.NewV7())
						dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_turns SET interrupt_requested_at=now() WHERE id=(SELECT active_turn_id FROM sessions WHERE id=$1)`, session)
					}
					if stopBeforeClaim {
						stop()
					}
					a, _, err := (&Server{tx: f.Pool}).claimRunLease(t.Context(), w, pgvalue.UUID(leaseID), 2)
					if err != nil {
						t.Fatal(err)
					}
					if !stopBeforeClaim {
						stop()
					}
					tx, err := f.Pool.Begin(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(context.Background())
					wait, err := run.AcknowledgeWaitResume(t.Context(), tx, run.ExecutionFence{LeaseID: a.runLease.ID, LeaseSequence: 2, WorkerGroupID: pgvalue.UUID(w.GroupID), WorkerHostID: pgvalue.UUID(w.HostID), WorkerEpoch: w.Epoch, GroupClaimVersion: w.GroupClaimVersion, HostClaimVersion: w.HostClaimVersion}, a.resumeWait.ID, a.runtime.SourceCheckpointID)
					if err != nil {
						t.Fatal(err)
					}
					if wait.SuspensionStatus != "released" || wait.ConditionReasonCode.String != "session_stopped" {
						t.Fatalf("stopped wait was not released: state=%s reason=%s", wait.SuspensionStatus, wait.ConditionReasonCode.String)
					}
					if err := tx.Commit(t.Context()); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func restoredActorFixture(t *testing.T, f runtest.Fixture, work runtest.RunLease, active bool) uuid.UUID {
	t.Helper()
	session := f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
	if active {
		turn := uuid.NewV7()
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_turns(id,environment_id,session_id,sequence,data,status,run_generation,run_id,attempt_number,ready_run_lease_id) SELECT $2,environment_id,id,committed_input_sequence+1,'{}','running',run_generation,current_run_id,1,$3 FROM sessions WHERE id=$1`, session, turn, work.LeaseID)
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET active_turn_id=$2 WHERE id=$1`, session, turn)
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_waits SET turn_session_id=$2,turn_id=$3,turn_run_generation=(SELECT run_generation FROM sessions WHERE id=$2) WHERE run_id=$1`, work.RunID, session, turn)
	}
	return session
}

func TestRestoredActorRejectsStaleStopScope(t *testing.T) {
	for _, mutation := range []struct{ name, sql string }{
		{"hold generation", `UPDATE sessions SET dispatch_hold_run_generation=run_generation+1 WHERE id=$1`},
		{"hold attempt", `UPDATE sessions SET dispatch_hold_attempt_number=2 WHERE id=$1`},
		{"active Turn", `UPDATE sessions SET active_turn_id=NULL WHERE id=$1`},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			var session uuid.UUID
			f, w, r, k := restorePlanFixture(t, false, true, func(f runtest.Fixture, work runtest.RunLease) { session = restoredActorFixture(t, f, work, true) })
			plan, err := readRestorePlan(t, f, w, r, k)
			if err != nil {
				t.Fatal(err)
			}
			activateRestorePlanFixture(t, f, w, plan)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET dispatch_hold_id=$2,dispatch_hold_reason='interrupt_requested',dispatch_hold_run_id=current_run_id,dispatch_hold_attempt_number=1,dispatch_hold_run_generation=run_generation WHERE id=$1`, session, uuid.NewV7())
			if mutation.name == "hold attempt" {
				dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO run_attempts(run_id,number,entrypoint_kind,computer_id,base_computer_disk_version_id,session_input_start_sequence) SELECT r.id,2,r.entrypoint_kind,r.computer_id,r.base_computer_disk_version_id,r.session_input_start_sequence FROM runs r JOIN sessions s ON s.current_run_id=r.id WHERE s.id=$1`, session)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, mutation.sql, session)
			var leaseID uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT r.current_run_lease_id FROM runs r JOIN sessions s ON s.current_run_id=r.id WHERE s.id=$1`, session).Scan(&leaseID); err != nil {
				t.Fatal(err)
			}
			_, _, err = (&Server{tx: f.Pool}).claimRunLease(t.Context(), w, pgvalue.UUID(leaseID), 2)
			if !errors.Is(err, errStaleRunLeaseClaim) {
				t.Fatalf("stale stop claim=%v", err)
			}
		})
	}
}
