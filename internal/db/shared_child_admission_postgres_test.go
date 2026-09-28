package db_test

import (
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestSharedChildAdmissionRetainsParentWriter(t *testing.T) {
	for _, same := range []bool{true, false} {
		for _, call := range []bool{true, false} {
			name := "different/start"
			if same {
				name = "same/start"
			}
			if call {
				name = name[:len(name)-5] + "call"
			}
			t.Run(name, func(t *testing.T) {
				f := runtest.New(t)
				parent := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now() WHERE id=$1`, parent.RunID)
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1`, parent.RunID)
				target := parent
				if !same {
					target = f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
				}
				var computer, head pgtype.UUID
				if err := f.Pool.QueryRow(t.Context(), `SELECT computer_id,base_computer_disk_version_id FROM runs WHERE id=$1`, target.RunID).Scan(&computer, &head); err != nil {
					t.Fatal(err)
				}
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(t.Context())
				fence := run.ExecutionFence{LeaseID: pgvalue.UUID(parent.LeaseID), LeaseSequence: 1, WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerEpoch: 1, GroupClaimVersion: 1, HostClaimVersion: 1}
				a, err := run.LockLiveExecutionForComputer(t.Context(), tx, fence, computer)
				if err != nil {
					t.Fatal(err)
				}
				claim := pgtype.UUID{}
				if call {
					claim = pgvalue.UUID(uuid.NewV7())
					dbtest.MustExec(t, t.Context(), tx, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'task.child.invoke',$3,$3,now())`, claim, f.EnvironmentID, dbtest.Hash(pgvalue.UUIDString(claim)))
				}
				q := db.New(tx)
				child, err := q.CreateChildRunFromParentDeployment(t.Context(), db.CreateChildRunFromParentDeploymentParams{
					EntrypointDeclaredID: "test-task", ComputerID: computer, BaseComputerDiskVersionID: head, ClaimID: claim, EnvironmentID: a.Run.EnvironmentID, ParentRunID: a.Run.ID, ID: pgvalue.UUID(uuid.NewV7()), ParentOwnsLifecycle: pgtype.Bool{Bool: call, Valid: true}, Payload: []byte(`{}`), Metadata: []byte(`{}`), Tags: []string{}, QueueName: "default", QueueOriginAt: a.Run.QueueOriginAt, QueueScoreAt: a.Run.QueueScoreAt, MaxActiveDurationMs: 300000, RetryPolicy: []byte(`{"enabled":false}`), TraceID: a.Run.TraceID, RootSpanID: "3333333333333333",
				})
				if err != nil {
					t.Fatal(err)
				}
				if child.ComputerID != computer || child.DeploymentID != a.Run.DeploymentID || child.ParentOwnsLifecycle.Bool != call {
					t.Fatalf("child binding=%+v", child)
				}
				if call {
					wait, err := q.RegisterChildCall(t.Context(), db.RegisterChildCallParams{ChildRunID: child.ID, EnvironmentID: a.Run.EnvironmentID, RunID: a.Run.ID, ChildComputerID: computer, ExpectedRunningRevision: a.Run.Revision, AttemptNumber: a.Attempt.Number, CurrentRunLeaseID: a.Lease.ID, ID: pgvalue.UUID(uuid.NewV7()), ChildTargetDeclaredID: pgvalue.Text("test-task"), ChildClaimID: claim, ChildRequest: []byte(`{}`), RegistrationRequestFingerprint: pgvalue.Text(dbtest.Digest("shared-call"))})
					if err != nil {
						t.Fatal(err)
					}
					if wait.SuspensionStatus != "hot" || wait.SuspendCheckpointID.Valid {
						t.Fatalf("wait changed physical suspension: %+v", wait)
					}
					replay, err := q.GetChildCallRunWaitReplay(t.Context(), db.GetChildCallRunWaitReplayParams{EnvironmentID: wait.EnvironmentID, RunID: wait.RunID, AttemptNumber: wait.AttemptNumber, ID: wait.ID, ChildRunID: child.ID, ChildClaimID: claim, RegistrationRequestFingerprint: wait.RegistrationRequestFingerprint})
					if err != nil || replay.ID != wait.ID {
						t.Fatalf("replay=%+v %v", replay, err)
					}
				}
				if _, err = run.LockLiveExecution(t.Context(), tx, fence); err != nil {
					t.Fatalf("parent lost live grant: %v", err)
				}
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				var status string
				var physical bool
				if err = f.Pool.QueryRow(t.Context(), `SELECT r.status,i.desired_state='ready' AND i.admission_state='open' AND i.capture_checkpoint_id IS NULL AND i.mount_state='mounted' AND i.reclaimed_at IS NULL AND l.status='running' AND c.head_disk_version_id=r.base_computer_disk_version_id FROM runs r JOIN run_leases l ON l.id=r.current_run_lease_id JOIN computer_instances i ON i.id=l.computer_instance_id JOIN computers c ON c.id=i.computer_id WHERE r.id=$1`, parent.RunID).Scan(&status, &physical); err != nil {
					t.Fatal(err)
				}
				want := "running"
				if call {
					want = "waiting"
				}
				if status != want || !physical {
					t.Fatalf("parent status=%s physical unchanged=%v", status, physical)
				}
			})
		}
	}
}
