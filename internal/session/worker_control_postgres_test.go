package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer/computerdbtest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func secondControlSession(t *testing.T, first *sessiontest.Execution) *sessiontest.Execution {
	t.Helper()
	return executionOn(t, first.Fixture, json.RawMessage(`1`), true)
}

func addControlSecret(t *testing.T, f *sessiontest.Execution) {
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
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computer_secrets(computer_id,environment_id,secret_id,placement_kind,placement_target,mode) VALUES($1,$2,$3,'env','TOKEN','raw')`, f.ComputerID, f.EnvironmentID, id)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO secret_resolutions(id,computer_id,run_id,attempt_number,placement_kind,placement_target,secret_id,secret_version_id,revocation_generation) VALUES($1,$2,$3,1,'env','TOKEN',$4,$5,0)`, uuid.NewV7(), f.ComputerID, f.RunID, id, version)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// interruptInTx authorizes the source's control of the target Session and
// interrupts the Turn in the transaction, returning a committed rejection as
// an *OperationError.
func interruptInTx(ctx context.Context, tx pgx.Tx, source, target *sessiontest.Execution, turnID uuid.UUID) error {
	locked, graph, _, err := lockControlFromRun(ctx, tx, source.Fence(), target.SessionID, true)
	if err != nil {
		return err
	}
	receipt, err := InterruptTurn(ctx, tx, pgvalue.MustUUIDValue(locked.EnvironmentID()), target.SessionID, turnID, "", graph)
	if err == nil && receipt.Code != "" {
		return &OperationError{Code: receipt.Code}
	}
	return err
}

func TestWorkerSessionControlReciprocalInterruptPostgres(t *testing.T) {
	a := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
	b := secondControlSession(t, a)
	at, bt := receiveTurn(t, a, 1), receiveTurn(t, b, 1)
	addControlSecret(t, a)
	addControlSecret(t, b)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, pair := range []struct {
		source, target *sessiontest.Execution
		turn           uuid.UUID
	}{{a, b, bt.TurnID}, {b, a, at.TurnID}} {
		go func() {
			<-start
			results <- db.RunTx(ctx, a.Pool, func(tx pgx.Tx) error {
				return interruptInTx(ctx, tx, pair.source, pair.target, pair.turn)
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
		var op *OperationError
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
			f := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
			scope := receiveTurn(t, f, 1)
			fence := f.Fence()
			switch state {
			case "stale":
				fence.LeaseSequence++
			case "settling":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_turns SET settlement_started_at=now() WHERE id=$1`, scope.TurnID)
			case "held":
				if _, err := ApplyInterrupt(t.Context(), f.Pool, InterruptRequest{ControlRequest: ControlRequest{Target: executionTarget(f)}, TurnID: scope.TurnID}); err != nil {
					t.Fatal(err)
				}
			}
			for _, interrupt := range []bool{false, true} {
				err := db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
					_, _, _, err := lockControlFromRun(t.Context(), tx, fence, f.SessionID, interrupt)
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
	f := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
	turn := receiveTurn(t, f, 1)
	interrupted, err := InterruptFromRun(t.Context(), f.Pool, f.Fence(), InterruptRequest{ControlRequest: ControlRequest{Target: Target{SessionID: f.SessionID}}, TurnID: turn.TurnID})
	if err != nil || interrupted.Status != "accepted" || interrupted.SessionID != f.SessionID {
		t.Fatalf("interrupt=%+v err=%v", interrupted, err)
	}
	var rejection *OperationError
	if _, err = ResumeFromRun(t.Context(), f.Pool, f.Fence(), ResumeRequest{ControlRequest: ControlRequest{Target: Target{SessionID: f.SessionID}}, HoldID: interrupted.HoldID}); !errors.As(err, &rejection) || rejection.Code != "session_held" {
		t.Fatalf("resume=%v", err)
	}
	var hold uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT dispatch_hold_id FROM sessions WHERE id=$1`, f.SessionID).Scan(&hold); err != nil || hold != interrupted.HoldID {
		t.Fatalf("hold=%s err=%v", hold, err)
	}
}

// The control re-reads the Secret union through run's Recheck: a binding
// added while the control waits on the execution host is rejected as
// unavailable instead of being locked after the fence.
func TestWorkerSessionControlRejectsBindingAddedMidControlPostgres(t *testing.T) {
	f := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	hostBlocker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer hostBlocker.Rollback(context.Background())
	dbtest.MustExec(t, ctx, hostBlocker, `SELECT id FROM worker_hosts WHERE id=$1 FOR UPDATE`, f.Worker.HostID)
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
		_, _, _, err := lockControlFromRun(ctx, control, f.Fence(), f.SessionID, true)
		done <- err
	}()
	// The control holds the Secret union and waits on the execution host.
	waitForPostgresBlock(t, f.Pool, pid)
	addControlSecret(t, f)
	secretBlocker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer secretBlocker.Rollback(context.Background())
	dbtest.MustExec(t, ctx, secretBlocker, `SELECT id FROM secrets WHERE id IN(SELECT secret_id FROM computer_secrets WHERE computer_id=$1) FOR UPDATE`, f.ComputerID)
	if err = hostBlocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, secret.ErrDeliveryUnavailable) {
		t.Fatalf("changed binding must reject without waiting for the new Secret: %v", err)
	}
}

// declareNoPayloadTask makes the deployment's Task take no payload.
func declareNoPayloadTask(t *testing.T, f *sessiontest.Execution) {
	t.Helper()
	manifest, digest, err := definition.CanonicalManifestAndDigest([]byte(`{"payload":{"kind":"none"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployment_definitions SET manifest=$2,manifest_digest=$3 WHERE id=$1`, f.TaskDefinitionID, manifest, digest[:])
}

// invokeChild invokes the payloadless Task from the parent's Turn on the
// Computer, as a call that the parent waits for or as a detached start.
func invokeChild(t *testing.T, parent *sessiontest.Execution, scope run.TurnScope, computerID uuid.UUID, detached bool, key string) run.ChildInvoked {
	t.Helper()
	method := "call"
	if detached {
		method = "start"
	}
	target := json.RawMessage(`{"id":"` + computerID.String() + `"}`)
	cursor := int64(1)
	invoke := run.ChildInvoke{
		Fence: parent.Fence(), Method: method, SourceComputerID: parent.ComputerID,
		Task: run.TaskStart{
			OrgID: parent.OrgID, ProjectID: parent.ProjectID, EnvironmentID: parent.EnvironmentID,
			TaskDeclaredID: "test-task", ComputerID: computerID, Metadata: json.RawMessage(`{}`), Tags: []string{},
		},
		IdempotencyKey: key,
		Fingerprint:    idempotency.TaskChildInvokeFingerprint{Method: method, Computer: target, Metadata: json.RawMessage(`{}`), Tags: []string{}},
		Cursor:         &cursor, TurnID: pgvalue.UUID(scope.TurnID), RunGeneration: pgtype.Int8{Int64: scope.RunGeneration, Valid: true},
		ChildResult: func(db.Run) (json.RawMessage, error) { return nil, errors.New("child is not terminal") },
	}
	if !detached {
		invoke.RunWaitID, invoke.ResumeAttachID = uuid.NewV7(), uuid.NewV7()
	}
	invoked, err := run.InvokeChild(t.Context(), parent.Pool, invoke)
	if err != nil {
		t.Fatalf("child invoke: %v", err)
	}
	return invoked
}

// controlChild invokes and starts a real child in its own Computer; the
// parent's Actor remains hot in a child wait for owned calls and stays
// running for starts.
func controlChild(t *testing.T, parent *sessiontest.Execution, detached bool) *sessiontest.Execution {
	t.Helper()
	scope := receiveTurn(t, parent, 1)
	declareNoPayloadTask(t, parent)
	dbtest.MustExec(t, t.Context(), parent.Pool, `UPDATE deployments SET queue_config='{"formatVersion":0,"queues":[{"concurrencyLimit":8,"name":"default"},{"name":"priority"}]}' WHERE id=$1`, parent.DeploymentID)
	dbtest.MustExec(t, t.Context(), parent.Pool, `UPDATE runs SET queue_concurrency_limit=8 WHERE environment_id=$1`, parent.EnvironmentID)
	f := *parent
	f.ComputerID, f.RootID = uuid.NewV7(), uuid.NewV7()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO computers(id,environment_id,region_id,sandbox_declared_id,head_disk_version_id, computer_spec_id, creation_deployment_id) VALUES($1,$2,'us-east-1','test-computer',$4, (SELECT computer_spec_id FROM deployment_definitions WHERE environment_id=$2 AND id=$3), (SELECT deployment_id FROM deployment_definitions WHERE environment_id=$2 AND id=$3))`, f.ComputerID, f.EnvironmentID, f.ComputerDefinitionID, f.RootID)
	computerdbtest.InsertCommittedComputerRoot(t, t.Context(), tx, f.RootID, f.EnvironmentID, f.ComputerID)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	invokeChild(t, parent, scope, f.ComputerID, detached, uuid.NewV7().String())
	if err = f.Pool.QueryRow(t.Context(), `SELECT id FROM runs WHERE computer_id=$1`, f.ComputerID).Scan(&f.RunID); err != nil {
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
	if err = f.Pool.QueryRow(t.Context(), `SELECT revision FROM runs WHERE id=$1`, f.RunID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	candidate := dispatch.RunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(f.RunID), ExpectedRunRevision: revision}
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
	f.LeaseID = pgvalue.MustUUIDValue(assigned.Lease.ID)
	f.ClaimLease(t)
	f.StartLease(t, "task", "test-task")
	return &f
}

func TestWorkerSessionControlOwnedChildrenReciprocalPostgres(t *testing.T) {
	a := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
	b := secondControlSession(t, a)
	at, bt := receiveTurn(t, a, 1), receiveTurn(t, b, 1)
	ac, bc := controlChild(t, a, false), controlChild(t, b, false)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	roots, err := a.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Rollback(context.Background())
	if _, err = roots.Exec(ctx, `SELECT id FROM runs WHERE id=ANY($1::uuid[]) ORDER BY id FOR UPDATE`, []uuid.UUID{a.RunID, b.RunID}); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var pids []int32
	for _, pair := range []struct {
		source, target *sessiontest.Execution
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
			err := interruptInTx(ctx, tx, pair.source, pair.target, pair.turn)
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
			a := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
			b := secondControlSession(t, a)
			scope := receiveTurn(t, a, 1)
			child := controlChild(t, a, state == "detached")
			if state == "settling" {
				dbtest.MustExec(t, t.Context(), a.Pool, `UPDATE session_turns SET settlement_started_at=now() WHERE id=$1`, scope.TurnID)
			} else {
				if _, err := ApplyInterrupt(t.Context(), a.Pool, InterruptRequest{ControlRequest: ControlRequest{Target: executionTarget(a)}, TurnID: scope.TurnID}); err != nil {
					t.Fatal(err)
				}
			}
			err := db.RunTx(t.Context(), a.Pool, func(tx pgx.Tx) error {
				_, _, _, err := lockControlFromRun(t.Context(), tx, child.Fence(), b.SessionID, true)
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
	parent := newExecution(t, json.RawMessage(`{"sequence":1}`), true)
	child := controlChild(t, parent, false)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	finalizer, err := parent.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer finalizer.Rollback(context.Background())
	// A different-Computer child's finalization locks its parent Run before
	// its own Run and does not first acquire the ancestor Actor's Session.
	if _, err = finalizer.Exec(ctx, `SELECT id FROM runs WHERE id=$1 FOR UPDATE`, parent.RunID); err != nil {
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
		_, _, _, err := lockControlFromRun(ctx, control, child.Fence(), parent.SessionID, true)
		done <- err
	}()
	waitForPostgresBlock(t, parent.Pool, pid)
	if _, err = finalizer.Exec(ctx, `SELECT id FROM runs WHERE id=$1 FOR UPDATE`, child.RunID); err != nil {
		t.Fatal(err)
	}
	if err = finalizer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

// Mutate after the target's first read, before the authority prologue reads it
// again. The caller's request never names this server-observed generation.
type changedControlTarget struct {
	pgx.Tx
	after     func()
	reads     int
	afterRead int
}

func (tx *changedControlTarget) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := tx.Tx.QueryRow(ctx, sql, args...)
	if strings.HasPrefix(sql, "-- name: GetSession :one") && tx.after != nil {
		return changedControlTargetRow{Row: row, tx: tx}
	}
	return row
}

type changedControlTargetRow struct {
	pgx.Row
	tx *changedControlTarget
}

func (r changedControlTargetRow) Scan(dest ...any) error {
	if err := r.Row.Scan(dest...); err != nil {
		return err
	}
	r.tx.reads++
	if r.tx.afterRead == 0 || r.tx.reads == r.tx.afterRead {
		after := r.tx.after
		r.tx.after = nil
		after()
	}
	return nil
}

func TestWorkerSessionControlTargetSnapshotChangePostgres(t *testing.T) {
	for _, tc := range []struct {
		name      string
		interrupt bool
		afterRead int
		idle      bool
	}{
		{"ordinary first read", false, 1, false},
		{"interruption first read", true, 1, false},
		{"interruption graph read", true, 2, false},
		{"interruption idle read", true, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := newExecution(t, nil, true)
			target := executionOn(t, source.Fixture, nil, true)
			if tc.idle {
				dbtest.MustExec(t, t.Context(), source.Pool, `UPDATE sessions SET current_run_id=NULL WHERE id=$1`, target.SessionID)
			}
			tx, err := source.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			wrapped := &changedControlTarget{Tx: tx, afterRead: tc.afterRead, after: func() {
				dbtest.MustExec(t, t.Context(), source.Pool, `UPDATE sessions SET run_generation=run_generation+1 WHERE id=$1`, target.SessionID)
			}}
			_, _, _, err = lockControlFromRun(t.Context(), wrapped, source.Fence(), target.SessionID, tc.interrupt)
			if !errors.Is(err, run.ErrExecutionTargetChanged) {
				t.Fatalf("snapshot change: %v", err)
			}
			if err = tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			tx, err = source.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, _, _, err = lockControlFromRun(t.Context(), tx, source.Fence(), target.SessionID, tc.interrupt); err != nil {
				t.Fatalf("fresh snapshot: %v", err)
			}
			var claims int
			if err = source.Pool.QueryRow(t.Context(), `SELECT count(*) FROM idempotency_claims WHERE operation IN ('session.cancel','session.resume','session.interrupt')`).Scan(&claims); err != nil || claims != 0 {
				t.Fatalf("pre-admission claims=%d err=%v", claims, err)
			}
		})
	}
}

func TestWorkerSessionControlRejectsObsoleteSourceOwnerPostgres(t *testing.T) {
	parent := newExecution(t, json.RawMessage(`1`), true)
	target := secondControlSession(t, parent)
	child := controlChild(t, parent, false)
	dbtest.MustExec(t, t.Context(), parent.Pool, `UPDATE sessions SET current_run_id=NULL,active_turn_id=NULL WHERE id=$1`, parent.SessionID)
	err := db.RunTx(t.Context(), parent.Pool, func(tx pgx.Tx) error {
		_, _, _, err := lockControlFromRun(t.Context(), tx, child.Fence(), target.SessionID, false)
		return err
	})
	if !errors.Is(err, run.ErrStaleSource) {
		t.Fatalf("obsolete source owner: %v", err)
	}
}

func TestWorkerSessionControlDeliveryRejectionRollsBackPostgres(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprint(corrupt), func(t *testing.T) {
			source := newExecution(t, nil, true)
			target := executionOn(t, source.Fixture, nil, true)
			addControlSecret(t, source)
			if corrupt {
				dbtest.MustExec(t, t.Context(), source.Pool, `DELETE FROM secret_resolutions WHERE run_id=$1`, source.RunID)
			} else {
				dbtest.MustExec(t, t.Context(), source.Pool, `UPDATE secrets SET status='revoked',current_version_id=NULL,revoked_at=clock_timestamp(),revocation_generation=revocation_generation+1 WHERE id IN (SELECT secret_id FROM computer_secrets WHERE computer_id=$1)`, source.ComputerID)
			}
			_, err := CancelFromRun(t.Context(), source.Pool, source.Fence(), ControlRequest{Target: Target{SessionID: target.SessionID}, IdempotencyKey: "denied-cancel"})
			if !errors.Is(err, secret.ErrDeliveryUnavailable) || errors.Is(err, secret.ErrDeliveryRevoked) == corrupt {
				t.Fatalf("delivery rejection classification: %v corrupt=%v", err, corrupt)
			}
			var mutated bool
			var claims int
			if err = source.Pool.QueryRow(t.Context(), `SELECT cancel_requested_at IS NOT NULL OR dispatch_hold_id IS NOT NULL,(SELECT count(*) FROM idempotency_claims WHERE operation='session.cancel') FROM sessions WHERE id=$1`, target.SessionID).Scan(&mutated, &claims); err != nil || mutated || claims != 0 {
				t.Fatalf("denied control committed: mutated=%v claims=%d err=%v", mutated, claims, err)
			}
		})
	}
}

func TestWorkerSessionControlTargetRevocationKeepsSourceAuthorityPostgres(t *testing.T) {
	source := newExecution(t, nil, true)
	target := executionOn(t, source.Fixture, nil, true)
	addControlSecret(t, target)
	dbtest.MustExec(t, t.Context(), source.Pool, `UPDATE secrets SET status='revoked',current_version_id=NULL,revoked_at=clock_timestamp(),revocation_generation=revocation_generation+1 WHERE id IN (SELECT secret_id FROM computer_secrets WHERE computer_id=$1)`, target.ComputerID)
	_, err := CancelFromRun(t.Context(), source.Pool, source.Fence(), ControlRequest{Target: Target{SessionID: target.SessionID}, IdempotencyKey: "target-revoked"})
	if !errors.Is(err, run.ErrExecutionTargetChanged) || errors.Is(err, secret.ErrDeliveryRevoked) || errors.Is(err, run.ErrStaleSource) {
		t.Fatalf("target-only revocation: %v", err)
	}
	var sourceLive, targetMutated bool
	var claims int
	if err = source.Pool.QueryRow(t.Context(), `SELECT l.status='running' AND r.current_run_lease_id=l.id,s.cancel_requested_at IS NOT NULL OR s.dispatch_hold_id IS NOT NULL,(SELECT count(*) FROM idempotency_claims WHERE operation='session.cancel') FROM run_leases l JOIN runs r ON r.id=l.run_id CROSS JOIN sessions s WHERE l.id=$1 AND s.id=$2`, source.LeaseID, target.SessionID).Scan(&sourceLive, &targetMutated, &claims); err != nil || !sourceLive || targetMutated || claims != 0 {
		t.Fatalf("rejected target control: sourceLive=%v targetMutated=%v claims=%d err=%v", sourceLive, targetMutated, claims, err)
	}
}
