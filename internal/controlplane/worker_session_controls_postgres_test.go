package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgxpool"
)

func secondWorkerControlActor(t *testing.T, first *actorExecutionFixture) *actorExecutionFixture {
	t.Helper()
	return actorExecutionOnFixture(t, first.Fixture, json.RawMessage(`1`), true)
}

func addWorkerControlSecret(t *testing.T, f *actorExecutionFixture) {
	t.Helper()
	id, version := uuid.NewV7(), uuid.NewV7()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO secrets(id,environment_id,name,current_version_id) VALUES($1,$2,$3,$4)`, id, f.EnvironmentID, "secret-"+id.String(), version)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO secret_versions(id,secret_id,version,nonce,ciphertext) VALUES($1,$2,1,decode(repeat('01',12),'hex'),decode(repeat('02',16),'hex'))`, version, id)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_secrets(computer_id,environment_id,secret_id,placement_kind,placement_target,mode) VALUES($1,$2,$3,'env','TOKEN','raw')`, f.computerID, f.EnvironmentID, id)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO secret_resolutions(id,computer_id,run_id,attempt_number,placement_kind,placement_target,secret_id,secret_version_id,revocation_generation) VALUES($1,$2,$3,1,'env','TOKEN',$4,$5,0)`, uuid.NewV7(), f.computerID, f.runID, id, version)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerSessionControlReciprocalInterruptPostgres(t *testing.T) {
	a := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
	b := secondWorkerControlActor(t, a)
	at, bt := a.receiveTurn(t, 1), b.receiveTurn(t, 1)
	addWorkerControlSecret(t, a)
	addWorkerControlSecret(t, b)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, pair := range []struct {
		source, target *actorExecutionFixture
		turn           uuid.UUID
	}{{a, b, bt.TurnID}, {b, a, at.TurnID}} {
		go func() {
			<-start
			results <- a.server.inTx(ctx, func(work *txWork) error {
				source, graph, _, err := lockWorkerSessionControl(ctx, work, pair.source.worker, pair.source.fence(), pgvalue.UUID(pair.target.sessionID), true)
				if err != nil {
					return err
				}
				receipt, err := session.InterruptTurn(ctx, work.tx, pgvalue.MustUUIDValue(source.EnvironmentID()), pair.target.sessionID, pair.turn, "", graph)
				if err == nil && receipt.Code != "" {
					return &session.OperationError{Code: receipt.Code}
				}
				return err
			})
		}()
	}
	close(start)
	accepted, held := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			accepted++
			continue
		}
		var op *session.OperationError
		if errors.As(err, &op) && op.Code == "session_held" {
			held++
		} else {
			t.Fatalf("reciprocal operation: %v", err)
		}
	}
	if accepted != 1 || held != 1 {
		t.Fatalf("accepted=%d held=%d", accepted, held)
	}
}

func TestWorkerSessionControlSourceFencePostgres(t *testing.T) {
	for _, state := range []string{"stale", "settling", "held"} {
		t.Run(state, func(t *testing.T) {
			f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
			scope := f.receiveTurn(t, 1)
			fence := f.fence()
			switch state {
			case "stale":
				fence.LeaseSequence++
			case "settling":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_turns SET settlement_started_at=now() WHERE id=$1`, scope.TurnID)
			case "held":
				if _, err := session.ApplyInterrupt(t.Context(), f.server.tx, session.InterruptRequest{ControlRequest: session.ControlRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}}, TurnID: scope.TurnID}); err != nil {
					t.Fatal(err)
				}
			}
			for _, interrupt := range []bool{false, true} {
				err := f.server.inTx(t.Context(), func(work *txWork) error {
					_, _, _, err := lockWorkerSessionControl(t.Context(), work, f.worker, fence, pgvalue.UUID(f.sessionID), interrupt)
					return err
				})
				if err == nil {
					t.Fatalf("%s source accepted interrupt=%v", state, interrupt)
				}
			}
		})
	}
}

func TestWorkerSessionControlSelfInterruptRejectsResumePostgres(t *testing.T) {
	f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
	turn := f.receiveTurn(t, 1)
	var interrupted workerapi.InterruptSessionTurnResponse
	f.workerCall(t, f.server.workerInterruptSessionTurn, workerapi.InterruptSessionTurnRequest{TurnReferenceRequest: workerapi.TurnReferenceRequest{SessionReferenceRequest: workerapi.SessionReferenceRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), SessionID: f.sessionID.String()}, TurnID: turn.TurnID.String()}}, &interrupted)
	if interrupted.Completed == nil || interrupted.Completed.SessionID != f.sessionID.String() {
		t.Fatalf("interrupt=%+v", interrupted)
	}
	var resumed workerapi.ResumeSessionResponse
	f.workerCall(t, f.server.workerResumeSession, workerapi.ResumeSessionRequest{SessionReferenceRequest: workerapi.SessionReferenceRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), SessionID: f.sessionID.String()}, HoldID: interrupted.Completed.HoldID}, &resumed)
	if resumed.Failed == nil || resumed.Failed.Code != "session_held" {
		t.Fatalf("resume=%+v", resumed)
	}
	var hold string
	if err := f.Pool.QueryRow(t.Context(), `SELECT dispatch_hold_id::text FROM sessions WHERE id=$1`, f.sessionID).Scan(&hold); err != nil || hold != interrupted.Completed.HoldID {
		t.Fatalf("hold=%s err=%v", hold, err)
	}
}

// lockWorkerSessionControl re-reads the Secret union through run's Recheck:
// a binding added while the control waits on the execution host is rejected
// as unavailable instead of being locked after the fence.
func TestWorkerSessionControlRejectsBindingAddedMidControlPostgres(t *testing.T) {
	f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	hostBlocker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hostBlocker.Rollback(context.Background())
	dbtest.MustExec(t, ctx, hostBlocker, `SELECT id FROM worker_hosts WHERE id=$1 FOR UPDATE`, f.worker.HostID)
	control, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Rollback(context.Background())
	var pid int32
	if err = control.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, _, err := lockWorkerSessionControl(ctx, &txWork{q: db.New(control), tx: control}, f.worker, f.fence(), pgvalue.UUID(f.sessionID), true)
		done <- err
	}()
	// The control holds the Secret union and waits on the execution host.
	waitForPostgresBlock(t, f.Pool, pid)
	addWorkerControlSecret(t, f)
	secretBlocker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer secretBlocker.Rollback(context.Background())
	dbtest.MustExec(t, ctx, secretBlocker, `SELECT id FROM secrets WHERE id IN(SELECT secret_id FROM computer_secrets WHERE computer_id=$1) FOR UPDATE`, f.computerID)
	if err = hostBlocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, secret.ErrDeliveryUnavailable) {
		t.Fatalf("changed binding must reject without waiting for the new Secret: %v", err)
	}
}

// Invoke and start a real child in its own Computer; the parent's Actor
// remains hot in a child wait for owned calls and stays running for starts.
func workerControlChild(t *testing.T, parent *actorExecutionFixture, detached bool) *actorExecutionFixture {
	t.Helper()
	scope := parent.receiveTurn(t, 1)
	manifest, digest, err := definition.CanonicalManifestAndDigest([]byte(`{"payload":{"kind":"none"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), parent.Pool, `UPDATE deployment_definitions SET manifest=$2,manifest_digest=$3 WHERE id=$1`, parent.TaskDefinitionID, manifest, digest[:])
	dbtest.MustExec(t, t.Context(), parent.Pool, `UPDATE deployments SET queue_config='{"formatVersion":0,"queues":[{"concurrencyLimit":8,"name":"default"},{"name":"priority"}]}' WHERE id=$1`, parent.DeploymentID)
	dbtest.MustExec(t, t.Context(), parent.Pool, `UPDATE runs SET queue_concurrency_limit=8 WHERE environment_id=$1`, parent.EnvironmentID)
	f := *parent
	f.computerID, f.rootID = uuid.NewV7(), uuid.NewV7()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computers(id,environment_id,region_id,sandbox_declared_id,head_disk_version_id, computer_spec_id, creation_deployment_id) VALUES($1,$2,'us-east-1','test-computer',$4, (SELECT computer_spec_id FROM deployment_definitions WHERE environment_id=$2 AND id=$3), (SELECT deployment_id FROM deployment_definitions WHERE environment_id=$2 AND id=$3))`, f.computerID, f.EnvironmentID, f.ComputerDefinitionID, f.rootID)
	dbtest.InsertCommittedComputerRoot(t, t.Context(), tx, f.rootID, f.EnvironmentID, f.computerID)
	dbtest.InsertComputerGeneration(t, t.Context(), tx, f.EnvironmentID, f.computerID, f.rootID)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	target, _ := json.Marshal(map[string]string{"id": f.computerID.String()})
	turnID := scope.TurnID.String()
	cursor := int64(1)
	request := workerapi.InvokeChildTaskRequest{Lease: parent.fence(), CorrelationID: uuid.NewV7().String(), RunWaitID: uuid.NewV7().String(), ResumeAttachID: uuid.NewV7().String(), TaskDeclaredID: "test-task", Method: "call", Computer: target, Options: json.RawMessage(`{}`), IdempotencyKey: uuid.NewV7().String(), TurnID: &turnID, RunGeneration: &scope.RunGeneration, ActorSpeculativeInputSequence: &cursor}
	if detached {
		request.Method = "start"
		request.RunWaitID = ""
		request.ResumeAttachID = ""
	}
	var invoked workerapi.InvokeChildTaskResponse
	parent.workerCall(t, parent.server.workerInvokeChildTask, request, &invoked)
	if invoked.Failed != nil {
		t.Fatalf("child invoke=%+v", invoked)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT id FROM runs WHERE computer_id=$1`, f.computerID).Scan(&f.runID); err != nil {
		t.Fatal(err)
	}
	key, err := disk.NewFencingKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := dispatch.NewRunAuthority(f.Pool, key)
	if err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err = f.Pool.QueryRow(t.Context(), `SELECT revision FROM runs WHERE id=$1`, f.runID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	candidate := dispatch.RunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: revision}
	assigned, err := authority.AssignRun(t.Context(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	if !assigned.LeaseCreated {
		// Supply a mounted Instance for this database-only control test.
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='ready',observed_version=observed_version+1,observed_desired_version=desired_version,ready_at=clock_timestamp(),mount_state='mounted',mounted_at=clock_timestamp() WHERE id=$1`, assigned.ComputerInstanceID)
		assigned, err = authority.AssignRun(t.Context(), candidate)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !assigned.LeaseCreated {
		t.Fatal("child execution was not assigned")
	}
	f.leaseID = pgvalue.MustUUIDValue(assigned.Lease.ID)
	f.claimLease(t)
	f.startLease(t, "task", "test-task")
	return &f
}

func TestWorkerSessionControlOwnedChildrenReciprocalPostgres(t *testing.T) {
	a := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
	b := secondWorkerControlActor(t, a)
	at, bt := a.receiveTurn(t, 1), b.receiveTurn(t, 1)
	ac, bc := workerControlChild(t, a, false), workerControlChild(t, b, false)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	roots, err := a.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Rollback(context.Background())
	if _, err = roots.Exec(ctx, `SELECT id FROM runs WHERE id=ANY($1::uuid[]) ORDER BY id FOR UPDATE`, []uuid.UUID{a.runID, b.runID}); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var pids []int32
	for _, pair := range []struct {
		source, target *actorExecutionFixture
		turn           uuid.UUID
	}{{ac, b, bt.TurnID}, {bc, a, at.TurnID}} {
		tx, err := a.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		var pid int32
		if err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		pids = append(pids, pid)
		go func() {
			q := db.New(tx)
			source, graph, _, err := lockWorkerSessionControl(ctx, &txWork{q: q, tx: tx}, pair.source.worker, pair.source.fence(), pgvalue.UUID(pair.target.sessionID), true)
			if err == nil {
				receipt, applyErr := session.InterruptTurn(ctx, tx, pgvalue.MustUUIDValue(source.EnvironmentID()), pair.target.sessionID, pair.turn, "", graph)
				err = applyErr
				if err == nil && receipt.Code != "" {
					err = &session.OperationError{Code: receipt.Code}
				}
			}
			if err == nil {
				err = tx.Commit(ctx)
			} else {
				_ = tx.Rollback(context.Background())
			}
			results <- err
		}()
	}
	// Both requests reach the held authority before either may commit.
	for _, pid := range pids {
		waitForPostgresBlock(t, a.Pool, pid)
	}
	if err = roots.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	accepted, rejected := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			accepted++
			continue
		}
		if errors.Is(err, run.ErrStaleSource) {
			rejected++
		} else {
			t.Fatalf("reciprocal owned child: %v", err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("accepted=%d rejected=%d", accepted, rejected)
	}
}

func TestWorkerSessionControlChildAncestorFencePostgres(t *testing.T) {
	for _, state := range []string{"held", "settling", "detached"} {
		t.Run(state, func(t *testing.T) {
			a := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
			b := secondWorkerControlActor(t, a)
			scope := a.receiveTurn(t, 1)
			child := workerControlChild(t, a, state == "detached")
			if state == "settling" {
				dbtest.MustExec(t, t.Context(), a.Pool, `UPDATE session_turns SET settlement_started_at=now() WHERE id=$1`, scope.TurnID)
			} else {
				if _, err := session.ApplyInterrupt(t.Context(), a.server.tx, session.InterruptRequest{ControlRequest: session.ControlRequest{Target: session.Target{EnvironmentID: a.EnvironmentID, SessionID: a.sessionID}}, TurnID: scope.TurnID}); err != nil {
					t.Fatal(err)
				}
			}
			err := a.server.inTx(t.Context(), func(work *txWork) error {
				_, _, _, err := lockWorkerSessionControl(t.Context(), work, child.worker, child.fence(), pgvalue.UUID(b.sessionID), true)
				return err
			})
			if state == "detached" {
				if err != nil {
					t.Fatalf("detached child inherited parent fence: %v", err)
				}
			} else if err == nil {
				t.Fatalf("%s ancestor accepted child control", state)
			}
		})
	}
}

func TestWorkerSessionControlChildToParentFinalizationOrderPostgres(t *testing.T) {
	parent := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
	child := workerControlChild(t, parent, false)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	finalizer, err := parent.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer finalizer.Rollback(context.Background())
	// A different-Computer child's finalization locks its parent Run before
	// its own Run and does not first acquire the ancestor Actor's Session.
	if _, err = finalizer.Exec(ctx, `SELECT id FROM runs WHERE id=$1 FOR UPDATE`, parent.runID); err != nil {
		t.Fatal(err)
	}
	control, err := parent.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Rollback(context.Background())
	var pid int32
	if err = control.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, _, err := lockWorkerSessionControl(ctx, &txWork{q: db.New(control), tx: control}, child.worker, child.fence(), pgvalue.UUID(parent.sessionID), true)
		done <- err
	}()
	waitForPostgresBlock(t, parent.Pool, pid)
	if _, err = finalizer.Exec(ctx, `SELECT id FROM runs WHERE id=$1 FOR UPDATE`, child.runID); err != nil {
		t.Fatal(err)
	}
	if err = finalizer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func waitForPostgresBlock(t *testing.T, pool *pgxpool.Pool, backendPID int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		if err := pool.QueryRow(t.Context(), `
SELECT cardinality(pg_blocking_pids($1)) > 0`, backendPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the concurrent transaction to block")
}
