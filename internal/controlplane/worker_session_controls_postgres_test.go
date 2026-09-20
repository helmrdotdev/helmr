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
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

func secondWorkerControlActor(t *testing.T, first *actorCheckpointFixture) *actorCheckpointFixture {
	t.Helper()
	f := *first
	f.workspaceID, f.rootID = uuid.NewV7(), uuid.NewV7()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO workspaces(id,environment_id,region_id,sandbox_declared_id,deployment_definition_id,head_version_id) VALUES($1,$2,'us-east-1','test-workspace',$3,$4)`, f.workspaceID, f.EnvironmentID, f.WorkspaceDefinitionID, f.rootID)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO workspace_versions(id,environment_id,workspace_id,status,content_digest,size_bytes,entry_count,ownership_generation,writer_generation,published_at) VALUES($1,$2,$3,'committed',$4,0,0,0,0,now())`, f.rootID, f.EnvironmentID, f.workspaceID, workspace.CanonicalEmptyTreeDigest)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	started, err := f.server.startActor(t.Context(), actorStartRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, ActorDeclaredID: "frontier", WorkspaceID: f.workspaceID})
	if err != nil {
		t.Fatal(err)
	}
	f.sessionID, f.runID = started.SessionID, started.BootRunID
	if _, err = f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.EnqueueOnly, Data: json.RawMessage(`1`)}); err != nil {
		t.Fatal(err)
	}
	f.placeAndStart(t)
	return &f
}

func addWorkerControlSecret(t *testing.T, f *actorCheckpointFixture) {
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
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO workspace_secrets(workspace_id,environment_id,secret_id,placement_kind,placement_target,mode) VALUES($1,$2,$3,'env','TOKEN','raw')`, f.workspaceID, f.EnvironmentID, id)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO secret_resolutions(id,workspace_id,run_id,attempt_number,placement_kind,placement_target,secret_id,secret_version_id,revocation_generation) VALUES($1,$2,$3,1,'env','TOKEN',$4,$5,0)`, uuid.NewV7(), f.workspaceID, f.runID, id, version)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerSessionControlReciprocalInterruptPostgres(t *testing.T) {
	a := newActorCheckpointFixture(t)
	b := secondWorkerControlActor(t, a)
	at, bt := a.receiveTurn(t, 1), b.receiveTurn(t, 1)
	addWorkerControlSecret(t, a)
	addWorkerControlSecret(t, b)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, pair := range []struct {
		source, target *actorCheckpointFixture
		turn           uuid.UUID
	}{{a, b, bt.TurnID}, {b, a, at.TurnID}} {
		go func() {
			<-start
			results <- a.server.inTx(ctx, func(work *txWork) error {
				source, graph, _, err := lockWorkerSessionControl(ctx, work, pair.source.worker, pair.source.fence(), pgvalue.UUID(pair.target.sessionID), true)
				if err != nil {
					return err
				}
				receipt, err := session.InterruptTurn(ctx, work.q, pgvalue.MustUUIDValue(source.EnvironmentID), pair.target.sessionID, pair.turn, "", graph)
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
			f := newActorCheckpointFixture(t)
			scope := f.receiveTurn(t, 1)
			fence := f.fence()
			switch state {
			case "stale":
				fence.LeaseSequence++
			case "settling":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_turns SET settlement_started_at=now() WHERE id=$1`, scope.TurnID)
			case "held":
				if _, err := f.server.applySessionInterrupt(t.Context(), session.InterruptRequest{ControlRequest: session.ControlRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}}, TurnID: scope.TurnID}); err != nil {
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
	f := newActorCheckpointFixture(t)
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

func TestWorkerSessionControlLocksChildBeforeWorkerGroupPostgres(t *testing.T) {
	a := newActorCheckpointFixture(t)
	b := secondWorkerControlActor(t, a)
	bturn := b.receiveTurn(t, 1)
	manifest, digest, err := deployment.CanonicalManifestAndDigest([]byte(`{"payload":{"kind":"none"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), a.Pool, `UPDATE deployment_definitions SET manifest=$2,manifest_digest=$3 WHERE id=$1`, a.TaskDefinitionID, manifest, digest[:])
	ws, version := uuid.NewV7(), uuid.NewV7()
	setup, err := a.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer setup.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), setup, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), setup, `INSERT INTO workspaces(id,environment_id,region_id,sandbox_declared_id,deployment_definition_id,head_version_id) VALUES($1,$2,'us-east-1','test-workspace',$3,$4)`, ws, a.EnvironmentID, a.WorkspaceDefinitionID, version)
	dbtest.MustExec(t, t.Context(), setup, `INSERT INTO workspace_versions(id,environment_id,workspace_id,status,content_digest,size_bytes,entry_count,ownership_generation,writer_generation,published_at) VALUES($1,$2,$3,'committed',$4,0,0,0,0,now())`, version, a.EnvironmentID, ws, workspace.CanonicalEmptyTreeDigest)
	if err = setup.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	target, _ := json.Marshal(map[string]string{"id": ws.String()})
	turnID := bturn.TurnID.String()
	cursor := int64(1)
	request := workerapi.InvokeChildTaskRequest{Lease: b.fence(), CorrelationID: uuid.NewV7().String(), RunWaitID: uuid.NewV7().String(), ResumeAttachID: uuid.NewV7().String(), TaskDeclaredID: "test-task", Method: "call", Workspace: target, Options: json.RawMessage(`{}`), IdempotencyKey: "lock-order-child", TurnID: &turnID, RunGeneration: &bturn.RunGeneration, ActorSpeculativeInputSequence: &cursor}
	var invoked workerapi.InvokeChildTaskResponse
	b.workerCall(t, b.server.workerInvokeChildTask, request, &invoked)
	if invoked.Failed != nil || invoked.OpenedWait == nil {
		t.Fatalf("invoke=%+v", invoked)
	}
	var child uuid.UUID
	if err = a.Pool.QueryRow(t.Context(), `SELECT child_run_id FROM run_waits WHERE id=$1`, uuid.MustParse(request.RunWaitID)).Scan(&child); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	claim, err := a.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Rollback(context.Background())
	// This is the queued child's claim prefix, before acquiring its Worker Group.
	if _, err = db.New(claim).LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{ID: pgvalue.UUID(child), OrgID: pgvalue.UUID(a.OrgID), ProjectID: pgvalue.UUID(a.ProjectID), EnvironmentID: pgvalue.UUID(a.EnvironmentID), WorkspaceID: pgvalue.UUID(ws)}); err != nil {
		t.Fatal(err)
	}
	control, err := a.Pool.Begin(ctx)
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
		_, _, _, err := lockWorkerSessionControl(ctx, &txWork{q: db.New(control), tx: control}, a.worker, a.fence(), pgvalue.UUID(b.sessionID), true)
		done <- err
	}()
	waitForPostgresBlock(t, a.Pool, pid)
	// If control acquired physical authority before the graph, this deadlocks.
	if _, err = db.New(claim).LockRunLeaseClaimWorkerGroup(ctx, db.LockRunLeaseClaimWorkerGroupParams{ID: pgvalue.UUID(a.worker.WorkerGroupID), RegionID: "us-east-1"}); err != nil {
		t.Fatal(err)
	}
	if err = claim.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWorkerSessionControlResumeSettledTargetPostgres(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "revoked"}[revoked], func(t *testing.T) {
			a := newActorCheckpointFixture(t)
			b := secondWorkerControlActor(t, a)
			addWorkerControlSecret(t, b)
			capture := b.capture(t, "settled target")
			b.turn(t, 1, capture, true)
			b.suspend(t, capture)
			canceler, err := run.NewCanceler(b.Pool)
			if err != nil {
				t.Fatal(err)
			}
			_, err = canceler.Cancel(t.Context(), run.CancellationRequest{OrgID: b.OrgID, ProjectID: b.ProjectID, EnvironmentID: b.EnvironmentID, RunID: b.runID})
			if err != nil {
				t.Fatal(err)
			}
			var hold, head uuid.UUID
			if err = b.Pool.QueryRow(t.Context(), `SELECT s.dispatch_hold_id,w.head_version_id FROM sessions s JOIN workspaces w ON w.id=s.workspace_id WHERE s.id=$1`, b.sessionID).Scan(&hold, &head); err != nil {
				t.Fatal(err)
			}
			recovered, err := b.server.applySessionRecovery(t.Context(), session.RecoverRequest{ResumeRequest: session.ResumeRequest{ControlRequest: session.ControlRequest{Target: session.Target{EnvironmentID: b.EnvironmentID, SessionID: b.sessionID}}, HoldID: hold}, WorkspaceVersionID: head, ReconciliationRef: "parked execution excluded"})
			if err != nil {
				t.Fatal(err)
			}
			if revoked {
				dbtest.MustExec(t, t.Context(), b.Pool, `UPDATE secrets SET status='revoked',current_version_id=NULL,revoked_at=now(),revocation_generation=revocation_generation+1 WHERE id IN(SELECT secret_id FROM workspace_secrets WHERE workspace_id=$1)`, b.workspaceID)
			}
			var response workerapi.ResumeSessionResponse
			a.workerCall(t, a.server.workerResumeSession, workerapi.ResumeSessionRequest{SessionReferenceRequest: workerapi.SessionReferenceRequest{Lease: a.fence(), CorrelationID: uuid.NewV7().String(), SessionID: b.sessionID.String()}, HoldID: recovered.HoldID.String()}, &response)
			if revoked {
				if response.Failed == nil || response.Failed.Code != "not_settled" {
					t.Fatalf("revoked=%+v", response)
				}
			} else if response.Completed == nil || response.Completed.SessionID != b.sessionID.String() {
				t.Fatalf("resume=%+v", response)
			}
		})
	}
}

type workerControlSecretRaceQueries struct {
	db.Querier
	afterUnion func() error
}

func (q workerControlSecretRaceQueries) LockWorkerControlSecrets(ctx context.Context, ids []pgtype.UUID) ([]db.LockWorkerControlSecretsRow, error) {
	rows, err := q.Querier.LockWorkerControlSecrets(ctx, ids)
	if err == nil {
		err = q.afterUnion()
	}
	return rows, err
}

func TestWorkerSessionControlNewBindingDoesNotAcquireLateSecretPostgres(t *testing.T) {
	f := newActorCheckpointFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	blocker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	q := workerControlSecretRaceQueries{Querier: db.New(tx), afterUnion: func() error {
		addWorkerControlSecret(t, f)
		_, err := blocker.Exec(ctx, `SELECT id FROM secrets WHERE id IN(SELECT secret_id FROM workspace_secrets WHERE workspace_id=$1) FOR UPDATE`, f.workspaceID)
		return err
	}}
	_, _, _, err = lockWorkerSessionControl(ctx, &txWork{q: q, tx: tx}, f.worker, f.fence(), pgvalue.UUID(f.sessionID), true)
	if !errors.Is(err, secret.ErrDeliveryUnavailable) {
		t.Fatalf("changed binding must reject without waiting for new Secret: %v", err)
	}
}

// Invoke and start a real child in its own Workspace; the parent's Actor
// remains hot in a child wait for owned calls and stays running for starts.
func workerControlChild(t *testing.T, parent *actorCheckpointFixture, detached bool) *actorCheckpointFixture {
	t.Helper()
	scope := parent.receiveTurn(t, 1)
	manifest, digest, err := deployment.CanonicalManifestAndDigest([]byte(`{"payload":{"kind":"none"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), parent.Pool, `UPDATE deployment_definitions SET manifest=$2,manifest_digest=$3 WHERE id=$1`, parent.TaskDefinitionID, manifest, digest[:])
	dbtest.MustExec(t, t.Context(), parent.Pool, `UPDATE deployments SET queue_config='{"formatVersion":0,"queues":[{"concurrencyLimit":8,"name":"default"},{"name":"priority"}]}' WHERE id=$1`, parent.DeploymentID)
	dbtest.MustExec(t, t.Context(), parent.Pool, `UPDATE runs SET queue_concurrency_limit=8 WHERE environment_id=$1`, parent.EnvironmentID)
	f := *parent
	f.workspaceID, f.rootID = uuid.NewV7(), uuid.NewV7()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), tx, `SET CONSTRAINTS ALL DEFERRED`)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO workspaces(id,environment_id,region_id,sandbox_declared_id,deployment_definition_id,head_version_id) VALUES($1,$2,'us-east-1','test-workspace',$3,$4)`, f.workspaceID, f.EnvironmentID, f.WorkspaceDefinitionID, f.rootID)
	dbtest.MustExec(t, t.Context(), tx, `INSERT INTO workspace_versions(id,environment_id,workspace_id,status,content_digest,size_bytes,entry_count,ownership_generation,writer_generation,published_at) VALUES($1,$2,$3,'committed',$4,0,0,0,0,now())`, f.rootID, f.EnvironmentID, f.workspaceID, workspace.CanonicalEmptyTreeDigest)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	target, _ := json.Marshal(map[string]string{"id": f.workspaceID.String()})
	turnID := scope.TurnID.String()
	cursor := int64(1)
	request := workerapi.InvokeChildTaskRequest{Lease: parent.fence(), CorrelationID: uuid.NewV7().String(), RunWaitID: uuid.NewV7().String(), ResumeAttachID: uuid.NewV7().String(), TaskDeclaredID: "test-task", Method: "call", Workspace: target, Options: json.RawMessage(`{}`), IdempotencyKey: uuid.NewV7().String(), TurnID: &turnID, RunGeneration: &scope.RunGeneration, ActorSpeculativeInputSequence: &cursor}
	if detached {
		request.Method = "start"
		request.RunWaitID = ""
		request.ResumeAttachID = ""
		request.ActorSpeculativeInputSequence = nil
	}
	var invoked workerapi.InvokeChildTaskResponse
	parent.workerCall(t, parent.server.workerInvokeChildTask, request, &invoked)
	if invoked.Failed != nil {
		t.Fatalf("child invoke=%+v", invoked)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT id FROM runs WHERE workspace_id=$1`, f.workspaceID).Scan(&f.runID); err != nil {
		t.Fatal(err)
	}
	f.placeAndClaim(t)
	f.workerCall(t, f.server.workerStart, workerapi.RunStartRequest{Lease: f.fence(), Fresh: &workerapi.RunStartFresh{}}, nil)
	f.workerCall(t, f.server.workerEnterRunEntrypoint, workerapi.RunEntrypointRequest{Lease: f.fence(), EntrypointKind: "task", EntrypointDeclaredID: "test-task"}, nil)
	return &f
}

func TestWorkerSessionControlOwnedChildrenReciprocalPostgres(t *testing.T) {
	a := newActorCheckpointFixture(t)
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
		source, target *actorCheckpointFixture
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
				receipt, applyErr := session.InterruptTurn(ctx, q, pgvalue.MustUUIDValue(source.EnvironmentID), pair.target.sessionID, pair.turn, "", graph)
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
	// Old ordering lets both callers acquire their own child before blocking on
	// the opposite root. Correct ordering blocks the second caller on Session.
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
		var op *session.OperationError
		if errors.As(err, &op) && op.Code == "session_held" {
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
			a := newActorCheckpointFixture(t)
			b := secondWorkerControlActor(t, a)
			scope := a.receiveTurn(t, 1)
			child := workerControlChild(t, a, state == "detached")
			if state == "settling" {
				dbtest.MustExec(t, t.Context(), a.Pool, `UPDATE session_turns SET settlement_started_at=now() WHERE id=$1`, scope.TurnID)
			} else {
				if _, err := a.server.applySessionInterrupt(t.Context(), session.InterruptRequest{ControlRequest: session.ControlRequest{Target: session.Target{EnvironmentID: a.EnvironmentID, SessionID: a.sessionID}}, TurnID: scope.TurnID}); err != nil {
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
	parent := newActorCheckpointFixture(t)
	child := workerControlChild(t, parent, false)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	finalizer, err := parent.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer finalizer.Rollback(context.Background())
	// A different-Workspace child's finalization locks its parent Run before
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
