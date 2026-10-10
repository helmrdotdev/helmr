package agent

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func initialSession(t *testing.T, f fixture, computer uuid.UUID) uuid.UUID {
	t.Helper()
	var session uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT id FROM sessions WHERE environment_id=$1 AND computer_id=$2 ORDER BY id LIMIT 1`, f.env, computer).Scan(&session); err != nil {
		t.Fatal(err)
	}
	return session
}
func TestInitialCohortContinuesAfterDrainButLaterSessionDoesNot(t *testing.T) {
	f, a, computer := freshComputerAllocationFixture(t)
	first := initialSession(t, f, computer)
	req := f.startRequest("initial-peer")
	req.ComputerID = computer
	peer, err := Start(t.Context(), f.pool, nil, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.AllocateFreshComputer(t.Context(), f.env, computer, 1, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	req.RetryKey = "later-peer"
	later, err := Start(t.Context(), f.pool, nil, f.caller(), req)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp(),drain_reason='admin' WHERE id=$1`, f.worker)
	identity := ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: computer, InstanceID: r.InstanceID, Epoch: r.Epoch}
	for _, session := range []uuid.UUID{first, peer.SessionID} {
		if _, err := a.AllocateSessionProcess(t.Context(), f.env, session, 1); err != nil {
			t.Fatalf("committed initial assignment unavailable: %v", err)
		}
		e := Execution{EnvironmentID: f.env, SessionID: session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.worker, WorkerEpoch: 1, AuthorityGeneration: 1}
		if _, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e); !errors.Is(err, ErrNotReady) {
			t.Fatalf("pre-ready attachment: %v", err)
		}
	}
	if _, err := a.DeliverComputer(t.Context(), *f.host(), identity); err != nil {
		t.Fatal(err)
	}
	if err := ObserveFreshComputerReady(t.Context(), f.pool, *f.host(), identity, r.Shape, r.BaseVersion); err != nil {
		t.Fatal(err)
	}
	for _, session := range []uuid.UUID{first, peer.SessionID} {
		e := Execution{EnvironmentID: f.env, SessionID: session, ProcessEpoch: 1, LeaseEpoch: 1, WorkerHostID: f.worker, WorkerEpoch: 1, AuthorityGeneration: 1}
		attachment, err := AcquireRuntimeAttachment(t.Context(), f.pool, *f.host(), e)
		if err != nil {
			t.Fatal(err)
		}
		if err := ObserveSessionReady(t.Context(), f.pool, *f.host(), e, attachment.Sequence); err != nil {
			t.Fatal(err)
		}
		if _, err := Dispatch(t.Context(), f.pool, e); err != nil {
			t.Fatalf("committed initial work refused on drain: %v", err)
		}
	}
	if _, err := a.AllocateSessionProcess(t.Context(), f.env, later.SessionID, 1); !errors.Is(err, ErrNotReady) {
		t.Fatalf("later independent admission bypassed drain: %v", err)
	}
}

func TestInitialCohortNeverReadyLossRetriesWithoutFailureHold(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "stop", true: "expiry_then_stop"}[expired], func(t *testing.T) {
			f, a, computer := freshComputerAllocationFixture(t)
			session := initialSession(t, f, computer)
			r, err := a.AllocateFreshComputer(t.Context(), f.env, computer, 1, f.worker)
			if err != nil {
				t.Fatal(err)
			}
			identity := ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: computer, InstanceID: r.InstanceID, Epoch: r.Epoch}
			if expired {
				if _, err := a.DeliverComputer(t.Context(), *f.host(), identity); err != nil {
					t.Fatal(err)
				}
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second' WHERE environment_id=$1 AND computer_id=$2 AND epoch=1`, f.env, computer)
				if err := expireComputerLease(t.Context(), f.pool, f.env, computer, 1); err != nil {
					t.Fatal(err)
				}
				var retained bool
				if err := f.pool.QueryRow(t.Context(), `SELECT fenced_at IS NULL AND failure_recorded_at IS NULL FROM session_processes WHERE environment_id=$1 AND session_id=$2 AND epoch=1`, f.env, session).Scan(&retained); err != nil || !retained {
					t.Fatalf("expiry lost physical custody: %v", err)
				}
				if _, err := a.AllocateFreshComputer(t.Context(), f.env, computer, 2, f.worker); err == nil {
					t.Fatal("replacement before physical absence")
				}
			}
			if err := ObserveComputerStopped(t.Context(), f.pool, *f.host(), identity, uuid.Nil()); err != nil {
				t.Fatal(err)
			}
			var clean bool
			if err := f.pool.QueryRow(t.Context(), `SELECT p.status='stopped' AND p.fenced_at IS NOT NULL AND p.failure_recorded_at IS NULL
   AND NOT EXISTS(SELECT 1 FROM session_holds h WHERE h.environment_id=p.environment_id AND h.session_id=p.session_id AND h.released_at IS NULL)
   AND EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=p.environment_id AND t.session_id=p.session_id AND t.status='queued' AND t.started_at IS NULL)
   FROM session_processes p WHERE p.environment_id=$1 AND p.session_id=$2 AND p.epoch=1`, f.env, session).Scan(&clean); err != nil || !clean {
				t.Fatalf("unexecuted assignment recorded execution failure: %v", err)
			}
			next, err := a.AllocateFreshComputer(t.Context(), f.env, computer, 2, f.worker)
			if err != nil {
				t.Fatal(err)
			}
			p, err := a.AllocateSessionProcess(t.Context(), f.env, session, 2)
			if err != nil || p.LeaseEpoch != 2 || next.InstanceID == r.InstanceID {
				t.Fatalf("fresh retry failed: %+v %v", p, err)
			}
		})
	}
}

func TestInitialCohortInitializedLossStillRequiresResume(t *testing.T) {
	f, _, e := startingSessionFixture(t)
	var identity ComputerLeaseIdentity
	identity.EnvironmentID = f.env
	identity.Epoch = e.LeaseEpoch
	if err := f.pool.QueryRow(t.Context(), `SELECT l.computer_id,l.computer_instance_id FROM computer_leases l JOIN sessions s ON (s.environment_id,s.computer_id)=(l.environment_id,l.computer_id) WHERE s.environment_id=$1 AND s.id=$2 AND l.epoch=$3`, f.env, e.SessionID, e.LeaseEpoch).Scan(&identity.ComputerID, &identity.InstanceID); err != nil {
		t.Fatal(err)
	}
	if err := ObserveComputerStopped(t.Context(), f.pool, *f.host(), identity, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	var held bool
	if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM session_holds WHERE environment_id=$1 AND session_id=$2 AND released_at IS NULL)`, f.env, e.SessionID).Scan(&held); err != nil || !held {
		t.Fatalf("initialized loss silently retried: %v", err)
	}
}

type cohortCommitBarrier struct {
	db.TxDB
	reached, release chan struct{}
}
type cohortCommitTx struct {
	pgx.Tx
	reached, release chan struct{}
}

func (b *cohortCommitBarrier) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxDB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &cohortCommitTx{Tx: tx, reached: b.reached, release: b.release}, nil
}
func (tx *cohortCommitTx) Commit(ctx context.Context) error {
	close(tx.reached)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-tx.release:
		return tx.Tx.Commit(ctx)
	}
}

func TestInitialCohortAllocationAndDrainSerialize(t *testing.T) {
	for _, allocationFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "allocation_first", false: "drain_first"}[allocationFirst], func(t *testing.T) {
			f, a, computer := freshComputerAllocationFixture(t)
			allocateDone := make(chan error, 1)
			if allocationFirst {
				barrier := &cohortCommitBarrier{TxDB: f.pool, reached: make(chan struct{}), release: make(chan struct{})}
				candidate := *a
				candidate.database = barrier
				go func() {
					_, err := candidate.AllocateFreshComputer(t.Context(), f.env, computer, 1, f.worker)
					allocateDone <- err
				}()
				<-barrier.reached
				drainDone := make(chan error, 1)
				go func() {
					_, err := f.pool.Exec(t.Context(), `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp(),drain_reason='admin' WHERE id=$1`, f.worker)
					drainDone <- err
				}()
				waitSessionLifecycleLock(t, f)
				close(barrier.release)
				if err := <-allocateDone; err != nil {
					t.Fatal(err)
				}
				if err := <-drainDone; err != nil {
					t.Fatal(err)
				}
			} else {
				tx, err := f.pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(t.Context())
				if _, err := tx.Exec(t.Context(), `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp(),drain_reason='admin' WHERE id=$1`, f.worker); err != nil {
					t.Fatal(err)
				}
				go func() {
					_, err := a.AllocateFreshComputer(t.Context(), f.env, computer, 1, f.worker)
					allocateDone <- err
				}()
				waitSessionLifecycleLock(t, f)
				if err := tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := <-allocateDone; err == nil {
					t.Fatal("allocation crossed committed drain")
				}
			}
			var assigned bool
			if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM session_processes WHERE environment_id=$1 AND computer_id=$2)`, f.env, computer).Scan(&assigned); err != nil || assigned != allocationFirst {
				t.Fatalf("initial cohort not atomic with allocation: %v %v", assigned, err)
			}
		})
	}
}

func TestInitialCohortExcludesHeldSessionUntilLaterAdmission(t *testing.T) {
	f, a, computer := freshComputerAllocationFixture(t)
	session := initialSession(t, f, computer)
	hold, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: session, Kind: "interrupt", RetryKey: "before-allocation", Reason: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.AllocateFreshComputer(t.Context(), f.env, computer, 1, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	var assigned bool
	if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM session_processes WHERE environment_id=$1 AND session_id=$2)`, f.env, session).Scan(&assigned); err != nil || assigned {
		t.Fatalf("held session joined initial cohort: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE worker_hosts SET status='draining',draining_at=clock_timestamp(),drain_reason='admin' WHERE id=$1`, f.worker)
	identity := ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: computer, InstanceID: r.InstanceID, Epoch: r.Epoch}
	if _, err := a.DeliverComputer(t.Context(), *f.host(), identity); err != nil {
		t.Fatal(err)
	}
	if err := ObserveFreshComputerReady(t.Context(), f.pool, *f.host(), identity, r.Shape, r.BaseVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: session, Kind: "resume", RetryKey: "after-allocation", HoldID: hold.HoldID}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AllocateSessionProcess(t.Context(), f.env, session, 1); !errors.Is(err, ErrNotReady) {
		t.Fatalf("post-cut resume bypassed new admission: %v", err)
	}
}

func TestInitialCohortCancellationBeforeReadyPreservesCancelledOutcome(t *testing.T) {
	f, a, computer := freshComputerAllocationFixture(t)
	session := initialSession(t, f, computer)
	r, err := a.AllocateFreshComputer(t.Context(), f.env, computer, 1, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: session, Kind: "cancel", RetryKey: "before-ready"}); err != nil {
		t.Fatal(err)
	}
	identity := ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: computer, InstanceID: r.InstanceID, Epoch: r.Epoch}
	if err := ObserveComputerStopped(t.Context(), f.pool, *f.host(), identity, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	var cancelled bool
	if err := f.pool.QueryRow(t.Context(), `SELECT s.status='cancelled' AND NOT EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.status<>'cancelled') AND NOT EXISTS(SELECT 1 FROM session_holds h WHERE h.environment_id=s.environment_id AND h.session_id=s.id AND h.released_at IS NULL) FROM sessions s WHERE s.environment_id=$1 AND s.id=$2`, f.env, session).Scan(&cancelled); err != nil || !cancelled {
		t.Fatalf("physical loss replaced explicit cancellation: %v", err)
	}
}

func TestInitialCohortInterruptBeforeReadyDoesNotAddFailureHold(t *testing.T) {
	f, a, computer := freshComputerAllocationFixture(t)
	session := initialSession(t, f, computer)
	r, err := a.AllocateFreshComputer(t.Context(), f.env, computer, 1, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	hold, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: session, Kind: "interrupt", RetryKey: "after-cohort", Reason: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	identity := ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: computer, InstanceID: r.InstanceID, Epoch: r.Epoch}
	if err := ObserveComputerStopped(t.Context(), f.pool, *f.host(), identity, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	if _, err := ControlSession(t.Context(), f.pool, f.caller(), SessionControlRequest{EnvironmentID: f.env, SessionID: session, Kind: "resume", RetryKey: "after-stop", HoldID: hold.HoldID}); err != nil {
		t.Fatal(err)
	}
	var holds int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM session_holds WHERE environment_id=$1 AND session_id=$2 AND released_at IS NULL`, f.env, session).Scan(&holds); err != nil || holds != 0 {
		t.Fatalf("unexecuted interrupted process acquired failure hold: %d %v", holds, err)
	}
	if _, err := a.AllocateFreshComputer(t.Context(), f.env, computer, 2, f.worker); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AllocateSessionProcess(t.Context(), f.env, session, 2); err != nil {
		t.Fatalf("explicit resume blocked by false execution loss: %v", err)
	}
}
