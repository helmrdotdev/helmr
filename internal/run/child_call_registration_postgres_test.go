package run

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestChildCallRegistrationRetainsParentWriter(t *testing.T) {
	for _, same := range []bool{true, false} {
		for _, completed := range []bool{false, true} {
			name := "different/pending"
			if same {
				name = "same/pending"
			}
			if completed {
				name = name[:len(name)-7] + "completed"
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
				fence := ExecutionFence{LeaseID: pgvalue.UUID(parent.LeaseID), LeaseSequence: 1, WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerEpoch: 1, GroupClaimVersion: 1, HostClaimVersion: 1}
				a, err := LockLiveExecutionForComputer(t.Context(), tx, fence, computer)
				if err != nil {
					t.Fatal(err)
				}
				claim := pgvalue.UUID(uuid.NewV7())
				dbtest.MustExec(t, t.Context(), tx, `INSERT INTO idempotency_claims(id,environment_id,operation,slot_hash,request_fingerprint,accepted_at) VALUES($1,$2,'task.child.invoke',$3,$3,now())`, claim, f.EnvironmentID, dbtest.Hash(pgvalue.UUIDString(claim)))
				q := db.New(tx)
				child, err := q.CreateChildRunFromParentDeployment(t.Context(), db.CreateChildRunFromParentDeploymentParams{
					EntrypointDeclaredID: "test-task", ComputerID: computer, BaseComputerDiskVersionID: head, ClaimID: claim, EnvironmentID: a.Run().EnvironmentID, ParentRunID: a.Run().ID, ID: pgvalue.UUID(uuid.NewV7()), ParentOwnsLifecycle: pgtype.Bool{Bool: true, Valid: true}, Payload: []byte(`{}`), Metadata: []byte(`{}`), Tags: []string{}, QueueName: "default", QueueOriginAt: a.Run().QueueOriginAt, QueueScoreAt: a.Run().QueueScoreAt, MaxActiveDurationMs: 300000, RetryPolicy: []byte(`{"enabled":false}`), TraceID: a.Run().TraceID, RootSpanID: "3333333333333333",
				})
				if err != nil {
					t.Fatal(err)
				}
				if child.ComputerID != computer || child.DeploymentID != a.Run().DeploymentID || !child.ParentOwnsLifecycle.Bool {
					t.Fatalf("child binding=%+v", child)
				}

				if completed {
					dbtest.MustExec(t, t.Context(), tx, `UPDATE runs SET status='succeeded',output='{"value":42}',terminal_at=now() WHERE id=$1`, child.ID)
				}
				input := ChildInvoke{
					Method: "call", RunWaitID: uuid.NewV7(), ResumeAttachID: uuid.NewV7(),
					Task:        TaskStart{TaskDeclaredID: "test-task"},
					Fingerprint: idempotency.TaskChildInvokeFingerprint{Method: "call", Computer: []byte(`{}`), Metadata: []byte(`{}`), Tags: []string{}},
					ChildResult: func(r db.Run) (json.RawMessage, error) {
						return json.Marshal(map[string]any{"ok": true, "output": json.RawMessage(r.Output), "run": map[string]string{"id": pgvalue.UUIDString(r.ID)}})
					},
				}
				receipt := db.IdempotencyClaim{ID: claim, RequestFingerprint: dbtest.Hash(pgvalue.UUIDString(claim))}
				registered, err := registerChildCall(t.Context(), q, input, a, receipt, pgvalue.MustUUIDValue(child.ID), pgvalue.MustUUIDValue(computer))
				if err != nil {
					t.Fatal(err)
				}
				replay, err := registerChildCall(t.Context(), q, input, a, receipt, pgvalue.MustUUIDValue(child.ID), pgvalue.MustUUIDValue(computer))
				if err != nil || replay.RunWaitID != registered.RunWaitID || replay.ResumeAttachID != registered.ResumeAttachID {
					t.Fatalf("replay=%+v %v", replay, err)
				}
				if completed {
					first, err := jsoncanon.Transform(registered.Resolution)
					if err != nil {
						t.Fatal(err)
					}
					again, err := jsoncanon.Transform(replay.Resolution)
					if err != nil {
						t.Fatal(err)
					}
					if !registered.Completed || !replay.Completed || !bytes.Equal(first, again) {
						t.Fatalf("terminal receipt mismatch: %s / %s", first, again)
					}
				}

				altered := input
				altered.ResumeAttachID = uuid.NewV7()
				if _, err := registerChildCall(t.Context(), q, altered, a, receipt, pgvalue.MustUUIDValue(child.ID), pgvalue.MustUUIDValue(computer)); !errors.Is(err, ErrChildInvokeStale) {
					t.Fatalf("altered receipt accepted: %v", err)
				}
				wait, err := q.GetRunWait(t.Context(), db.GetRunWaitParams{RunID: a.Run().ID, AttemptNumber: a.Attempt().Number, ID: pgvalue.UUID(input.RunWaitID)})
				wantSuspension := "hot"
				if completed {
					wantSuspension = "released"
				}
				if err != nil || wait.SuspensionStatus != wantSuspension || wait.SuspendCheckpointID.Valid {
					t.Fatalf("wait=%+v %v", wait, err)
				}

				if _, err = LockLiveExecution(t.Context(), tx, fence); err != nil {
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
				if !completed {
					want = "waiting"
				}
				if status != want || !physical {
					t.Fatalf("parent status=%s physical unchanged=%v", status, physical)
				}
			})
		}
	}
}
