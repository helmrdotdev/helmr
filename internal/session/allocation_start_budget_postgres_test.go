package session_test

import (
	"bytes"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

func startBudgetFixture(t *testing.T) (fixture, *dispatch.Allocator, uuid.UUID, uuid.UUID) {
	t.Helper()
	f, a, leaseComputer := freshComputerAllocationFixture(t)
	// The fixture's published image has completed its independent physical stop.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds SET fenced_at=clock_timestamp(),fence_evidence='fixture build VM stopped' WHERE environment_id=$1 AND status='succeeded';
 UPDATE worker_hosts SET max_vm_starts=1,observed_at=clock_timestamp() WHERE id=$2`, pgx.QueryExecModeSimpleProtocol, f.env, f.worker)
	build := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO image_builds(environment_id,id,image_spec_id,idempotency_key,deadline_at) VALUES($1,$2,$3,'start-budget',clock_timestamp()+interval '10 minutes')`, f.env, build, f.deployment)
	spec, err := artifact.CanonicalComputerPreparationSpec(artifact.ComputerPreparationSpec{
		APIVersion: artifact.ComputerPreparationSpecVersion, TemplateDefinitionID: "fixture-computer",
		Program: artifact.ProgramDescriptor{Digest: "sha256:" + strings.Repeat("1", 64), SizeBytes: 1, MediaType: artifact.ProgramArtifactMediaType},
	})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_specs SET spec=$3 WHERE environment_id=$1 AND id=$2`, f.env, f.deployment, spec)
	return f, a, leaseComputer, build
}

func assertStartBudget(t *testing.T, f fixture, starts int64) {
	t.Helper()
	var starting int64
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FILTER (WHERE starting) FROM allocation_usage WHERE worker_host_id=$1`, f.worker).Scan(&starting); err != nil || starting != starts {
		t.Fatalf("physical starts=%d, want %d: %v", starting, starts, err)
	}
	bins, err := db.New(f.pool).ListWorkerCapacityBins(t.Context(), db.ListWorkerCapacityBinsParams{WorkerGroupID: pgvalue.UUID(f.group), ObservationFreshnessSeconds: 30, RowLimit: 10})
	if err != nil || len(bins) != 1 || bins[0].AvailableInstanceStarts != 1-starts {
		t.Fatalf("planner disagrees with physical starts: %+v: %v", bins, err)
	}
}

func TestAllocationLeaseDeliveryKeepsStartBudgetUntilReady(t *testing.T) {
	f, a, target, build := startBudgetFixture(t)
	receipt, err := a.AllocateFreshLease(t.Context(), f.env, target, 1, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	identity := computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: target, Epoch: receipt.Epoch, InstanceID: receipt.InstanceID}
	assertStartBudget(t, f, 1)
	if _, err = a.DeliverLease(t.Context(), *f.host(), identity); err != nil {
		t.Fatal(err)
	}
	assertStartBudget(t, f, 1)
	if _, err = a.AllocateImageBuild(t.Context(), f.env, build, f.worker); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("delivery freed physical start budget: %v", err)
	}
	if err = dispatch.ObserveFreshLeaseReady(t.Context(), f.pool, *f.host(), identity, receipt.Shape, receipt.BaseVersion); err != nil {
		t.Fatal(err)
	}
	assertStartBudget(t, f, 0)
	if _, err = a.AllocateImageBuild(t.Context(), f.env, build, f.worker); err != nil {
		t.Fatal(err)
	}
	assertStartBudget(t, f, 1)
}

func TestAllocationImageBuildDeliveryKeepsStartBudgetUntilAuthenticatedStart(t *testing.T) {
	f, a, target, build := startBudgetFixture(t)
	receipt, err := a.AllocateImageBuild(t.Context(), f.env, build, f.worker)
	if err != nil {
		t.Fatal(err)
	}
	identity := computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: build, Epoch: receipt.Epoch, InstanceID: receipt.InstanceID}
	delivered, err := a.DeliverImageBuild(t.Context(), *f.host(), identity)
	if err != nil {
		t.Fatal(err)
	}
	assertStartBudget(t, f, 1)
	if _, err = a.AllocateFreshLease(t.Context(), f.env, target, 1, f.worker); !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("delivery freed physical start budget: %v", err)
	}
	if _, err = computer.ReadImageBuildStart(t.Context(), f.pool, *f.host(), delivered.Executor); err != nil {
		t.Fatal(err)
	}
	assertStartBudget(t, f, 1)
	wrong := delivered.Executor
	wrong.ChannelCredential = bytes.Repeat([]byte{99}, 32)
	if err = computer.StartImageBuild(t.Context(), f.pool, *f.host(), wrong); !errors.Is(err, computer.ErrDenied) {
		t.Fatalf("unauthenticated start admitted: %v", err)
	}
	assertStartBudget(t, f, 1)
	if err = computer.StartImageBuild(t.Context(), f.pool, *f.host(), delivered.Executor); err != nil {
		t.Fatal(err)
	}
	var first time.Time
	if err = f.pool.QueryRow(t.Context(), `SELECT started_at FROM image_builds WHERE environment_id=$1 AND id=$2`, f.env, build).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err = computer.StartImageBuild(t.Context(), f.pool, *f.host(), delivered.Executor); err != nil {
		t.Fatal(err)
	}
	var replay time.Time
	if err = f.pool.QueryRow(t.Context(), `SELECT started_at FROM image_builds WHERE environment_id=$1 AND id=$2`, f.env, build).Scan(&replay); err != nil || !replay.Equal(first) {
		t.Fatalf("start receipt changed: %v %v: %v", first, replay, err)
	}
	assertStartBudget(t, f, 0)
	if _, err = a.AllocateFreshLease(t.Context(), f.env, target, 1, f.worker); err != nil {
		t.Fatal(err)
	}
	assertStartBudget(t, f, 1)
}

func TestAllocationFailedStartsKeepBudgetUntilPhysicalStop(t *testing.T) {
	for _, kind := range []string{"computer", "preparation"} {
		t.Run(kind, func(t *testing.T) {
			f, a, target, build := startBudgetFixture(t)
			var stop func() error
			var retry func() error
			if kind == "computer" {
				r, err := a.AllocateFreshLease(t.Context(), f.env, target, 1, f.worker)
				if err != nil {
					t.Fatal(err)
				}
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='lost' WHERE environment_id=$1 AND computer_id=$2`, f.env, target)
				stop = func() error {
					return dispatch.ObserveLeaseStopped(t.Context(), auditpolicy.Policy{
						RetentionSeconds: 3600,
					}, f.pool, *f.host(), computer.LeaseIdentity{EnvironmentID: f.env, ComputerID: target, InstanceID: r.InstanceID, Epoch: r.Epoch}, uuid.Nil(), uuid.Nil())
				}
				retry = func() error { _, err := a.AllocateImageBuild(t.Context(), f.env, build, f.worker); return err }
			} else {
				r, err := a.AllocateImageBuild(t.Context(), f.env, build, f.worker)
				if err != nil {
					t.Fatal(err)
				}
				dbtest.MustExec(t, t.Context(), f.pool, `UPDATE image_builds SET status='failed',error_code='start_failed' WHERE environment_id=$1 AND id=$2`, f.env, build)
				stop = func() error {
					return computer.ObserveImageBuildStopped(t.Context(), f.pool, *f.host(), computer.ImageBuildIdentity{EnvironmentID: f.env, ImageBuildID: build, InstanceID: r.InstanceID, Epoch: r.Epoch})
				}
				retry = func() error { _, err := a.AllocateFreshLease(t.Context(), f.env, target, 1, f.worker); return err }
			}
			assertStartBudget(t, f, 1)
			if err := retry(); !errors.Is(err, dispatch.ErrNotReady) {
				t.Fatalf("failed start released capacity without physical stop: %v", err)
			}
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			assertStartBudget(t, f, 0)
			if err := retry(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAllocationConcurrentKindsShareStartBudget(t *testing.T) {
	f, a, target, build := startBudgetFixture(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, err := a.AllocateFreshLease(t.Context(), f.env, target, 1, f.worker)
		results <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, err := a.AllocateImageBuild(t.Context(), f.env, build, f.worker)
		results <- err
	}()
	close(start)
	wg.Wait()
	close(results)
	admitted := 0
	for err := range results {
		if err == nil {
			admitted++
		} else if !errors.Is(err, dispatch.ErrNotReady) {
			t.Fatal(err)
		}
	}
	if admitted != 1 {
		t.Fatalf("admitted %d physical starts with one start slot", admitted)
	}
	assertStartBudget(t, f, 1)
	// A lock timeout may reject the contender first; after the winner commits,
	// the allocator must still reject it for the shared physical start budget.
	var leaseExists bool
	if err := f.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2)`, f.env, target).Scan(&leaseExists); err != nil {
		t.Fatal(err)
	}
	var err error
	if leaseExists {
		_, err = a.AllocateImageBuild(t.Context(), f.env, build, f.worker)
	} else {
		_, err = a.AllocateFreshLease(t.Context(), f.env, target, 1, f.worker)
	}
	if !errors.Is(err, dispatch.ErrNotReady) {
		t.Fatalf("contender exceeded committed start budget: %v", err)
	}
}
