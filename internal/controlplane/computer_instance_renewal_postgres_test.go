package controlplane

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func TestInstanceWriterRenewalDoesNotRenewMembers(t *testing.T) {
	for _, state := range []string{"open", "draining", "checkpointing", "closed"} {
		t.Run(state, func(t *testing.T) {
			f := runtest.New(t)
			work := f.AddRunLease(t, "running", time.Now())
			w, r := instanceRenewalFixture(t, f, work)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET guest_channel_token_hash=decode(repeat('ab',32),'hex'),guest_channel_token_expires_at=clock_timestamp()+interval '30 seconds',writer_expires_at=clock_timestamp()+interval '30 seconds',admission_state=$2,desired_version=desired_version+CASE WHEN $2='closed' THEN 1 ELSE 0 END,desired_state=CASE WHEN $2='closed' THEN 'closed' ELSE 'ready' END WHERE id=$1`, r.ComputerInstanceID, state)
			var before, leaseBefore, channelBefore time.Time
			if err := f.Pool.QueryRow(t.Context(), `SELECT i.writer_expires_at,l.expires_at,i.guest_channel_token_expires_at FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id WHERE l.id=$1`, work.LeaseID).Scan(&before, &leaseBefore, &channelBefore); err != nil {
				t.Fatal(err)
			}
			result, err := applyInstanceRenewal(t, f, w, r)
			if err != nil {
				t.Fatal(err)
			}
			if state != "closed" && (!result.GuestChannelTokenExpiresAt.Valid || !result.GuestChannelTokenExpiresAt.Time.After(before)) {
				t.Fatal("live Instance channel expired independently of renewed writer")
			}
			if result.AdmissionState != state {
				t.Fatal("renewal changed admission")
			}
			if state == "closed" {
				if !result.WriterExpiresAt.Time.Equal(before) || !result.GuestChannelTokenExpiresAt.Time.Equal(channelBefore) {
					t.Fatal("closed writer extended")
				}
			} else if !result.WriterExpiresAt.Time.After(before) {
				t.Fatal("writer not extended")
			}
			var leaseAfter time.Time
			if err := f.Pool.QueryRow(t.Context(), `SELECT expires_at FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&leaseAfter); err != nil {
				t.Fatal(err)
			}
			if !leaseBefore.Equal(leaseAfter) {
				t.Fatal("member lease extended")
			}
		})
	}
}
func TestInstanceWriterRenewalRejectsStaleAuthority(t *testing.T) {
	for _, test := range []struct{ name, sql string }{
		{"expired", `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second'`},
		{"closed admission", `UPDATE computer_instances SET admission_state='closed'`},
		{"writer", `UPDATE computers SET writer_generation=writer_generation+1`},
		{"Worker claims", `UPDATE worker_hosts SET claim_version=claim_version+1`},
		{"Worker epoch", `UPDATE worker_hosts SET current_epoch=current_epoch+1`},
		{"Group claims", `UPDATE worker_groups SET claim_version=claim_version+1`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := runtest.New(t)
			work := f.AddRunLease(t, "running", time.Now())
			w, r := instanceRenewalFixture(t, f, work)
			dbtest.MustExec(t, t.Context(), f.Pool, test.sql)
			_, err := applyInstanceRenewal(t, f, w, r)
			if !errors.Is(err, pgx.ErrNoRows) && !errors.Is(err, errStaleWorkerClaims) {
				t.Fatalf("stale renewal=%v", err)
			}
		})
	}
}

func TestInstanceWriterRenewalRechecksExpiryAfterLockWait(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now())
	w, request := instanceRenewalFixture(t, f, work)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT id FROM worker_groups WHERE id=$1 FOR UPDATE`, w.WorkerGroupID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var pid int32
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var renewalErr error
	go func() {
		defer close(done)
		_, renewalErr = renewComputerInstance(ctx, tx, w, request)
	}()
	// Join the request before rolling back its connection, including on failure.
	defer func() { cancel(); <-done }()
	for {
		var blocked bool
		if err := f.Pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	// This deadline is after the waiting transaction's start but before it can
	// acquire authority. Transaction-start time must not resurrect the writer.
	if _, err := blocker.Exec(ctx, `UPDATE computer_instances SET writer_expires_at=clock_timestamp() WHERE id=$1`, request.ComputerInstanceID); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if !errors.Is(renewalErr, pgx.ErrNoRows) {
			t.Fatalf("expired waiting renewal=%v", renewalErr)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func instanceRenewalFixture(t *testing.T, f runtest.Fixture, work runtest.RunLease) (workerActor, workerapi.ComputerInstanceRenewRequest) {
	t.Helper()
	w := workerActor{WorkerHostID: f.WorkerID, WorkerGroupID: runtest.WorkerGroupID, WorkerEpoch: 1}
	r := workerapi.ComputerInstanceRenewRequest{EnvironmentID: f.EnvironmentID.String()}
	var instance uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT i.id,i.writer_generation,w.claim_version,g.claim_version FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id JOIN worker_hosts w ON w.id=i.worker_host_id JOIN worker_groups g ON g.id=w.worker_group_id WHERE l.id=$1`, work.LeaseID).Scan(&instance, &r.WriterGeneration, &w.ClaimVersion, &w.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	r.ComputerInstanceID = instance.String()
	return w, r
}
func applyInstanceRenewal(t *testing.T, f runtest.Fixture, w workerActor, r workerapi.ComputerInstanceRenewRequest) (db.ComputerInstance, error) {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		return db.ComputerInstance{}, err
	}
	defer tx.Rollback(context.Background())
	result, err := renewComputerInstance(t.Context(), tx, w, r)
	if err == nil {
		err = tx.Commit(t.Context())
	}
	return result, err
}
