package workergroup_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

type retirementFixture struct {
	agenttest.Fixture
	poolID uuid.UUID
	host   workergroup.HostPrincipal
}

func newRetirementFixture(t *testing.T) retirementFixture {
	t.Helper()
	f := retirementFixture{Fixture: agenttest.New(t)}
	if err := f.Pool.QueryRow(t.Context(), `SELECT worker_pool_id FROM worker_hosts WHERE id=$1`, f.Worker).Scan(&f.poolID); err != nil {
		t.Fatal(err)
	}
	f.host = workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	return f
}
func (f retirementFixture) capture() agent.ComputerCaptureRequest {
	return agent.ComputerCaptureRequest{EnvironmentID: f.Environment, ComputerID: f.Computer, CheckpointID: uuid.NewV7(), LeaseEpoch: 1, ChannelCredential: agenttest.ChannelCredential}
}
func cloneRestorePool(t *testing.T, f retirementFixture) uuid.UUID {
	t.Helper()
	id := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_pools SELECT (jsonb_populate_record(NULL::worker_pools,to_jsonb(p)||jsonb_build_object('id',$2::text,'name',$2::text))).* FROM worker_pools p WHERE p.id=$1;
 INSERT INTO worker_pool_cpu_shapes(worker_pool_id,vcpu_count,cpu_config_digest) SELECT $2,vcpu_count,cpu_config_digest FROM worker_pool_cpu_shapes WHERE worker_pool_id=$1`, pgx.QueryExecModeSimpleProtocol, f.poolID, id)
	return id
}

func TestPoolRetirementPostgresConcurrentLastRestorePath(t *testing.T) {
	f := newRetirementFixture(t)
	alternative := cloneRestorePool(t, f)
	start := make(chan struct{})
	results := make([]error, 2)
	var wait sync.WaitGroup
	for index, id := range []uuid.UUID{f.poolID, alternative} {
		wait.Go(func() { <-start; _, _, results[index] = workergroup.DrainPool(t.Context(), f.Pool, f.Group, id, 1) })
	}
	close(start)
	wait.Wait()
	successes := 0
	for _, err := range results {
		if err == nil {
			successes++
			continue
		}
		var conflict workergroup.ConflictError
		if !errors.As(err, &conflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("retirements=%v, want exactly one", results)
	}
	var active int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM worker_pools WHERE worker_group_id=$1 AND status='active'`, f.Group).Scan(&active); err != nil || active != 1 {
		t.Fatalf("remaining pools=%d: %v", active, err)
	}
}

func TestPoolRetirementPostgresWaitsForCaptureFence(t *testing.T) {
	f := newRetirementFixture(t)
	cloneRestorePool(t, f)
	hold, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Rollback(t.Context())
	if _, err = agent.BeginComputerCapture(t.Context(), hold, f.host, f.capture()); err != nil {
		t.Fatal(err)
	}
	var blocker int32
	if err = hold.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := workergroup.DrainPool(ctx, f.Pool, f.Group, f.poolID, 1); done <- err }()
	for {
		var blocked bool
		if err = f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("retirement did not wait: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err = hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	profiles, err := workergroup.ResolvePool(ctx, db.New(f.Pool), f.Group, "pool")
	if err != nil || !profiles.RetainedProfiles.Complete || len(profiles.RetainedProfiles.Profiles) != 1 {
		t.Fatalf("profiles=%+v: %v", profiles, err)
	}
	profile := profiles.RetainedProfiles.Profiles[0]
	if profile.LiveInstances != 1 || profile.CapturingCheckpoints != 1 || profile.EligiblePools != 1 {
		t.Fatalf("capture dependency=%+v", profile)
	}
}

func TestPoolRetirementPostgresParkedQueuedWorkRetainsProfile(t *testing.T) {
	f := newRetirementFixture(t)
	request := f.capture()
	if _, err := agent.BeginComputerCapture(t.Context(), f.Pool, f.host, request); err != nil {
		t.Fatal(err)
	}
	// This fixture supplies an already-certified checkpoint; this test exercises
	// supplier retirement, not VM capture or manifest certification.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_checkpoints SET status='ready',manifest=decode('01','hex'),vm_platform_id=(SELECT vm_platform_id FROM worker_hosts WHERE id=$3),ready_at=clock_timestamp() WHERE environment_id=$1 AND id=$2;
 UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='fixture VM stopped' WHERE environment_id=$1`, pgx.QueryExecModeSimpleProtocol, f.Environment, request.CheckpointID, f.Worker)
	if _, err := agent.Enqueue(t.Context(), f.Pool, agent.Caller{Kind: "user", ID: f.User}, agent.EnqueueRequest{EnvironmentID: f.Environment, SessionID: f.Session, RetryKey: "parked-input", Input: []byte(`[]`)}); err != nil {
		t.Fatal(err)
	}
	if demand, err := workergroup.HasQueuedDemand(t.Context(), f.Pool, f.Group); err != nil || !demand {
		t.Fatalf("parked Session restore demand=%v: %v", demand, err)
	}
	if _, err := workergroup.DrainHost(t.Context(), f.Pool, f.Worker, workergroup.DrainWorkerHostRequest{ExpectedEpoch: 1, ExpectedClaimVersion: 1, Reason: workergroup.DrainReasonIdleScaleIn}); !errors.Is(err, workergroup.ErrQueuedDemand) {
		t.Fatalf("idle drain with parked input=%v, want queued demand", err)
	}
	var conflict workergroup.ConflictError
	if _, _, err := workergroup.DrainPool(t.Context(), f.Pool, f.Group, f.poolID, 1); !errors.As(err, &conflict) {
		t.Fatalf("last parked profile retired: %v", err)
	}
	alternative := cloneRestorePool(t, f)
	if _, _, err := workergroup.DrainPool(t.Context(), f.Pool, f.Group, f.poolID, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := workergroup.DrainPool(t.Context(), f.Pool, f.Group, alternative, 1); !errors.As(err, &conflict) {
		t.Fatalf("last alternative retired: %v", err)
	}
	profiles, err := workergroup.ResolvePool(t.Context(), db.New(f.Pool), f.Group, "pool")
	if err != nil || len(profiles.RetainedProfiles.Profiles) != 1 || profiles.RetainedProfiles.Profiles[0].LiveInstances != 0 || profiles.RetainedProfiles.Profiles[0].ParkedCheckpoints != 1 {
		t.Fatalf("parked profile=%+v: %v", profiles, err)
	}
}

func TestPoolRetirementPostgresCaptureRequiresRemainingSupplier(t *testing.T) {
	f := newRetirementFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_pools SET status='draining',claim_version=claim_version+1 WHERE id=$1`, f.poolID)
	if _, err := agent.BeginComputerCapture(t.Context(), f.Pool, f.host, f.capture()); !errors.Is(err, agent.ErrNotReady) {
		t.Fatalf("capture without supplier=%v", err)
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoints WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected capture committed %d rows: %v", count, err)
	}
}

func TestPoolRetirementPostgresDisableWaitsForPhysicalStop(t *testing.T) {
	f := newRetirementFixture(t)
	cloneRestorePool(t, f)
	lost, err := workergroup.MarkHostLost(t.Context(), f.Pool, f.Group, "host", 1)
	if err != nil || lost.Status != "lost" {
		t.Fatalf("mark lost=%+v %v", lost, err)
	}
	_, drained, err := workergroup.DrainPool(t.Context(), f.Pool, f.Group, f.poolID, 1)
	if err != nil {
		t.Fatal(err)
	}
	var conflict workergroup.ConflictError
	if _, _, err = workergroup.DisablePool(t.Context(), f.Pool, f.Group, f.poolID, drained.ClaimVersion); !errors.As(err, &conflict) {
		t.Fatalf("disabled while VM custody remains: %v", err)
	}
	err = db.RunTx(t.Context(), f.Pool, func(tx pgx.Tx) error {
		absence, err := workergroup.ConfirmHostProviderAbsent(t.Context(), tx, f.Worker)
		if err != nil {
			return err
		}
		return agent.ObserveProviderAbsentHostComputers(t.Context(), absence)
	})
	if err != nil {
		t.Fatal(err)
	}
	_, receipt, err := workergroup.DisablePool(t.Context(), f.Pool, f.Group, f.poolID, drained.ClaimVersion)
	if err != nil || receipt.Status != "disabled" {
		t.Fatalf("disable receipt=%+v: %v", receipt, err)
	}
	_, replay, err := workergroup.DisablePool(t.Context(), f.Pool, f.Group, f.poolID, drained.ClaimVersion)
	if err != nil || replay.ClaimVersion != receipt.ClaimVersion {
		t.Fatalf("disable replay=%+v: %v", replay, err)
	}
	if _, _, err = workergroup.DrainPool(t.Context(), f.Pool, f.Group, f.poolID, receipt.ClaimVersion); !errors.As(err, &conflict) {
		t.Fatalf("disabled pool revived: %v", err)
	}
}

func TestCaptureRequiresRestoreAdmittingGroup(t *testing.T) {
	for _, status := range []string{"paused", "draining"} {
		t.Run(status, func(t *testing.T) {
			f := newRetirementFixture(t)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_groups SET status=$2 WHERE id=$1`, f.Group, status)
			if _, err := agent.BeginComputerCapture(t.Context(), f.Pool, f.host, f.capture()); !errors.Is(err, agent.ErrNotReady) {
				t.Fatalf("capture in %s Group: %v; want not ready", status, err)
			}
			var count int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoints WHERE environment_id=$1`, f.Environment).Scan(&count); err != nil || count != 0 {
				t.Fatalf("checkpoints=%d: %v", count, err)
			}
		})
	}
}
