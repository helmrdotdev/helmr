package computer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// writerFixture is the principal of the lease's worker host and the writer
// of the lease's Instance.
func writerFixture(t *testing.T, f runtest.Fixture, work runtest.RunLease) (workergroup.HostPrincipal, WriterRef) {
	t.Helper()
	principal := workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1}
	writer := WriterRef{EnvironmentID: f.EnvironmentID}
	if err := f.Pool.QueryRow(t.Context(), `SELECT i.id,i.writer_generation,w.claim_version,g.claim_version FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id JOIN worker_hosts w ON w.id=i.worker_host_id JOIN worker_groups g ON g.id=w.worker_group_id WHERE l.id=$1`, work.LeaseID).Scan(&writer.InstanceID, &writer.WriterGeneration, &principal.HostClaimVersion, &principal.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	return principal, writer
}

func applyChannelClaim(t *testing.T, f runtest.Fixture, principal workergroup.HostPrincipal, writer WriterRef, token string) (db.ComputerInstance, error) {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		return db.ComputerInstance{}, err
	}
	defer tx.Rollback(context.Background())
	i, err := claimChannel(t.Context(), tx, principal, pgvalue.UUID(writer.InstanceID), pgvalue.UUID(writer.EnvironmentID), token)
	if err == nil {
		err = tx.Commit(t.Context())
	}
	return i, err
}

func TestInstanceChannelHasOneOwner(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now())
	principal, writer := writerFixture(t, f, work)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, token := range []string{"one", "two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			i, err := applyChannelClaim(t, f, principal, writer, token)
			if err == nil {
				hash := sha256.Sum256([]byte(token))
				if !bytes.Equal(i.GuestChannelTokenHash, hash[:]) || i.WriterGeneration != writer.WriterGeneration || !i.GuestChannelTokenExpiresAt.Time.Equal(i.WriterExpiresAt.Time) {
					results <- errors.New("channel changed writer authority")
					return
				}
			}
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
	}
	if accepted != 1 {
		t.Fatalf("channel owners=%d", accepted)
	}
	if _, err := applyChannelClaim(t, f, principal, writer, "replacement"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("live channel rotated: %v", err)
	}
}

func TestInstanceChannelRejectsStaleAdmission(t *testing.T) {
	for _, test := range []struct{ name, sql string }{
		{"expired writer", `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second'`},
		{"unobserved desired state", `UPDATE computer_instances SET desired_version=desired_version+1`},
		{"stale Worker observation", `UPDATE worker_hosts SET observed_at=clock_timestamp()-interval '1 hour'`},
		{"Worker epoch changed", `UPDATE worker_hosts SET current_epoch=current_epoch+1`},
		{"new writer", `UPDATE computers SET writer_generation=writer_generation+1`},
		{"closed admission", `UPDATE computer_instances SET admission_state='closed'`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := runtest.New(t)
			work := f.AddRunLease(t, "running", time.Now())
			principal, writer := writerFixture(t, f, work)
			dbtest.MustExec(t, t.Context(), f.Pool, test.sql)
			if _, err := applyChannelClaim(t, f, principal, writer, "token"); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("claim=%v", err)
			}
		})
	}
}

// ClaimInstance skips a candidate whose authority changed and reports no
// assignment once the only candidate is claimed.
func TestClaimInstanceAssignsOnePreparedInstance(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now())
	principal, writer := writerFixture(t, f, work)
	assignment, err := ClaimInstance(t.Context(), db.New(f.Pool), f.Pool, principal)
	if err != nil {
		t.Fatal(err)
	}
	if assignment == nil {
		t.Fatal("prepared Instance not assigned")
	}
	hash := sha256.Sum256([]byte(assignment.ChannelToken))
	if assignment.Instance.ID != pgvalue.UUID(writer.InstanceID) || !bytes.Equal(assignment.Instance.GuestChannelTokenHash, hash[:]) || assignment.Source.SeedDigest == "" {
		t.Fatalf("assignment=%+v", assignment)
	}
	if again, err := ClaimInstance(t.Context(), db.New(f.Pool), f.Pool, principal); err != nil || again != nil {
		t.Fatalf("second claim=%+v %v", again, err)
	}
}

func TestInstanceWriterRenewalDoesNotRenewMembers(t *testing.T) {
	for _, state := range []string{"open", "draining", "checkpointing", "closed"} {
		t.Run(state, func(t *testing.T) {
			f := runtest.New(t)
			work := f.AddRunLease(t, "running", time.Now())
			principal, writer := writerFixture(t, f, work)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET guest_channel_token_hash=decode(repeat('ab',32),'hex'),guest_channel_token_expires_at=clock_timestamp()+interval '30 seconds',writer_expires_at=clock_timestamp()+interval '30 seconds',admission_state=$2,desired_version=desired_version+CASE WHEN $2='closed' THEN 1 ELSE 0 END,desired_state=CASE WHEN $2='closed' THEN 'closed' ELSE 'ready' END WHERE id=$1`, writer.InstanceID, state)
			var before, leaseBefore, channelBefore time.Time
			if err := f.Pool.QueryRow(t.Context(), `SELECT i.writer_expires_at,l.expires_at,i.guest_channel_token_expires_at FROM computer_instances i JOIN run_leases l ON l.computer_instance_id=i.id WHERE l.id=$1`, work.LeaseID).Scan(&before, &leaseBefore, &channelBefore); err != nil {
				t.Fatal(err)
			}
			result, err := RenewInstance(t.Context(), f.Pool, principal, writer)
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
		{"Worker epoch", `UPDATE worker_hosts SET current_epoch=current_epoch+1`},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := runtest.New(t)
			work := f.AddRunLease(t, "running", time.Now())
			principal, writer := writerFixture(t, f, work)
			dbtest.MustExec(t, t.Context(), f.Pool, test.sql)
			if _, err := RenewInstance(t.Context(), f.Pool, principal, writer); !errors.Is(err, ErrAuthorityChanged) {
				t.Fatalf("stale renewal=%v", err)
			}
		})
	}
}

func TestInstanceWriterRenewalRechecksExpiryAfterLockWait(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now())
	principal, writer := writerFixture(t, f, work)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err := blocker.Exec(ctx, `SELECT id FROM worker_groups WHERE id=$1 FOR UPDATE`, principal.GroupID); err != nil {
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
		_, renewalErr = renewWriter(ctx, tx, principal, writer)
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
	if _, err := blocker.Exec(ctx, `UPDATE computer_instances SET writer_expires_at=clock_timestamp() WHERE id=$1`, writer.InstanceID); err != nil {
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

func TestRunCleanupRequiresTerminalScopeAndCurrentPhysicalOwner(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now())
	peer := f.AddRunLease(t, "assigned", time.Now())
	principal, writer := writerFixture(t, f, work)
	process := RunProcess{RunID: work.RunID, RunLeaseID: work.LeaseID, AttemptNumber: 1}
	result, err := RunCleanup(t.Context(), f.Pool, principal, writer)
	if err != nil || result != nil {
		t.Fatalf("active Run selected: %+v %v", result, err)
	}
	if err = ReconcileRun(t.Context(), f.Pool, principal, writer, process); err == nil {
		t.Fatal("active Run reconciled")
	}
	if _, err = f.Pool.Exec(t.Context(), `UPDATE run_leases SET status='cancelled',terminal_at=clock_timestamp(),terminal_reason_code='run_cancelled' WHERE id=$1`, work.LeaseID); err != nil {
		t.Fatal(err)
	}
	result, err = RunCleanup(t.Context(), f.Pool, principal, writer)
	if err != nil || result == nil || *result != process {
		t.Fatalf("cancelled Run missing: %+v %v", result, err)
	}
	for _, alter := range []func(*workergroup.HostPrincipal, *WriterRef, *RunProcess){
		func(p *workergroup.HostPrincipal, w *WriterRef, r *RunProcess) { p.Epoch++ },
		func(p *workergroup.HostPrincipal, w *WriterRef, r *RunProcess) { p.HostClaimVersion++ },
		func(p *workergroup.HostPrincipal, w *WriterRef, r *RunProcess) { p.GroupClaimVersion++ },
		func(p *workergroup.HostPrincipal, w *WriterRef, r *RunProcess) { w.WriterGeneration++ },
		func(p *workergroup.HostPrincipal, w *WriterRef, r *RunProcess) { r.AttemptNumber++ },
		func(p *workergroup.HostPrincipal, w *WriterRef, r *RunProcess) { r.RunID = peer.RunID },
	} {
		p, w, r := principal, writer, process
		alter(&p, &w, &r)
		if err = ReconcileRun(t.Context(), f.Pool, p, w, r); err == nil {
			t.Fatal("stale proof accepted")
		}
	}
	var first time.Time
	for n := 0; n < 2; n++ {
		if err = ReconcileRun(t.Context(), f.Pool, principal, writer, process); err != nil {
			t.Fatal(err)
		}
		var at time.Time
		if err = f.Pool.QueryRow(t.Context(), `SELECT process_reconciled_at FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&at); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			first = at
		} else if !at.Equal(first) {
			t.Fatal("replay changed proof timestamp")
		}
	}
	result, err = RunCleanup(t.Context(), f.Pool, principal, writer)
	if err != nil || result != nil {
		t.Fatalf("reconciled Run selected: %+v %v", result, err)
	}
	var untouched bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT process_reconciled_at IS NULL AND status='assigned' FROM run_leases WHERE id=$1`, peer.LeaseID).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("peer changed: %v %v", untouched, err)
	}
}

// claimBumps advance only a claim version of the lease's worker host or
// group, which signals credential freshness and is never a fence.
var claimBumps = []struct{ name, sql string }{
	{"host claims", `UPDATE worker_hosts SET claim_version=claim_version+1`},
	{"group claims", `UPDATE worker_groups SET claim_version=claim_version+1`},
}

// Instance operations authenticated by a host principal compare its claim
// versions: a bump reports stale claims so the host re-authenticates.
func TestWriterOperationsReportStaleClaims(t *testing.T) {
	for _, bump := range claimBumps {
		for _, operation := range []struct {
			name string
			call func(context.Context, runtest.Fixture, workergroup.HostPrincipal, WriterRef, RunProcess) error
		}{
			{"claim", func(ctx context.Context, f runtest.Fixture, p workergroup.HostPrincipal, _ WriterRef, _ RunProcess) error {
				_, err := ClaimInstance(ctx, db.New(f.Pool), f.Pool, p)
				return err
			}},
			{"renewal", func(ctx context.Context, f runtest.Fixture, p workergroup.HostPrincipal, w WriterRef, _ RunProcess) error {
				_, err := RenewInstance(ctx, f.Pool, p, w)
				return err
			}},
			{"run cleanup", func(ctx context.Context, f runtest.Fixture, p workergroup.HostPrincipal, w WriterRef, _ RunProcess) error {
				_, err := RunCleanup(ctx, f.Pool, p, w)
				return err
			}},
			{"run reconciliation", func(ctx context.Context, f runtest.Fixture, p workergroup.HostPrincipal, w WriterRef, r RunProcess) error {
				return ReconcileRun(ctx, f.Pool, p, w, r)
			}},
		} {
			t.Run(bump.name+"/"+operation.name, func(t *testing.T) {
				f := runtest.New(t)
				work := f.AddRunLease(t, "running", time.Now())
				principal, writer := writerFixture(t, f, work)
				dbtest.MustExec(t, t.Context(), f.Pool, bump.sql)
				err := operation.call(t.Context(), f, principal, writer, RunProcess{RunID: work.RunID, RunLeaseID: work.LeaseID, AttemptNumber: 1})
				if !errors.Is(err, workergroup.ErrStaleClaims) {
					t.Fatalf("%s after %s = %v", operation.name, bump.name, err)
				}
			})
		}
	}
}

// Instance observations carry no claim versions: the locked host epoch and
// status are their worker authority, so a claim-only bump is ignored.
func TestObservationsIgnoreClaimBumps(t *testing.T) {
	for _, bump := range claimBumps {
		t.Run(bump.name, func(t *testing.T) {
			f, _, i := runningInstance(t)
			dbtest.MustExec(t, t.Context(), f.Pool, bump.sql)
			ready := Readiness{Observation: observationOf(i), VCPUCount: i.VMVCPUCount, CPUConfigDigest: i.CPUConfigDigest}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_version=desired_version+1 WHERE id=$1`, i.ID)
			ready.Instance.DesiredVersion++
			row, err := RecordInstanceReady(t.Context(), f.Pool, ready)
			if err != nil {
				t.Fatalf("readiness after %s: %v", bump.name, err)
			}
			failed, err := RecordInstanceFailure(t.Context(), f.Pool, Failure{Observation: observationOf(row), Kind: FailureRuntime, Reason: "runtime_reconcile_failed", Error: []byte(`{}`)})
			if err != nil || failed.ObservedState != "failed" {
				t.Fatalf("failure after %s: %v %v", bump.name, failed.ObservedState, err)
			}
		})
	}
}
