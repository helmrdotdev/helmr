package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

func computerStorageFixture(t *testing.T) (agenttest.Fixture, computer.Scope) {
	t.Helper()
	f := agenttest.New(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_preparation_specs SET seed=jsonb_build_object('profile',$3::text) WHERE environment_id=$1 AND id=$2;
 UPDATE computer_definitions SET resources='{"milliCpu":1000,"memoryMiB":512}' WHERE environment_id=$1;
 UPDATE computers SET preparation_spec_id=$2,origin_deployment_id=$2,origin_definition_key='fixture-computer',resources='{"milliCpu":1000,"memoryMiB":512}',storage_reservation_bytes=$4 WHERE environment_id=$1;
 UPDATE environments SET max_reserved_storage_bytes=$4 WHERE id=$1`, pgx.QueryExecModeSimpleProtocol, f.Environment, f.Deployment, definition.ComputerSeedProfile, disk.SeedCapacity)
	scope := computer.Scope{EnvironmentID: f.Environment}
	if err := f.Pool.QueryRow(t.Context(), `SELECT org_id,project_id FROM environments WHERE id=$1`, f.Environment).Scan(&scope.OrgID, &scope.ProjectID); err != nil {
		t.Fatal(err)
	}
	return f, scope
}

func TestComputerDeletionReleasesStorageForFreshAdmission(t *testing.T) {
	f, scope := computerStorageFixture(t)
	start := func(key string) error {
		_, err := agent.Start(t.Context(), f.Pool, nil, agent.Caller{Kind: "user", ID: f.User}, agent.StartRequest{EnvironmentID: f.Environment, Agent: "agent", RetryKey: key, Input: json.RawMessage(`[{"type":"text","text":"{\"work\":1}"}]`)})
		return err
	}
	assertStatus := func(want computer.Status) {
		t.Helper()
		snapshot, err := computer.Read(t.Context(), f.Pool, scope, f.Computer)
		if err != nil || snapshot.Status != want {
			t.Fatalf("Computer status=%s want=%s error=%v", snapshot.Status, want, err)
		}
	}
	deletion := computer.Deletion{Scope: scope, ComputerID: f.Computer, IdempotencyKey: "delete"}
	if _, err := computer.Delete(t.Context(), f.Pool, deletion); !errors.Is(err, computer.ErrBusy) {
		t.Fatalf("live Session allowed deletion: %v", err)
	}
	if err := start("before-delete"); !errors.Is(err, agent.ErrNotReady) {
		t.Fatalf("full environment admitted new Computer: %v", err)
	}
	// Logical work has ended; the Worker has not yet confirmed physical stop.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1;
 UPDATE sessions SET status='closed' WHERE environment_id=$1`, pgx.QueryExecModeSimpleProtocol, f.Environment)
	if _, err := computer.Delete(t.Context(), f.Pool, deletion); err != nil {
		t.Fatal(err)
	}
	if err := agent.ReconcileComputerDiskRetention(t.Context(), f.Pool); err != nil {
		t.Fatal(err)
	}
	assertStatus(computer.StatusDeleting)
	if err := start("before-stop"); !errors.Is(err, agent.ErrNotReady) {
		t.Fatalf("unfenced deletion released capacity: %v", err)
	}
	host := workergroup.HostPrincipal{HostID: f.Worker, GroupID: f.Group, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	if err := agent.ObserveComputerStopped(t.Context(), f.Pool, host, agent.ComputerLeaseIdentity{EnvironmentID: f.Environment, ComputerID: f.Computer, InstanceID: f.Computer, Epoch: 1}, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	assertStatus(computer.StatusDeleting)
	// Concurrent/repeated collectors must converge without returning more than
	// the single reservation. Fresh admissions still serialize at the quota.
	var wg sync.WaitGroup
	cleanupErrors := make(chan error, 4)
	for range 4 {
		wg.Go(func() { cleanupErrors <- agent.ReconcileComputerDiskRetention(t.Context(), f.Pool) })
	}
	wg.Wait()
	close(cleanupErrors)
	for err := range cleanupErrors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := agent.ReconcileComputerDiskRetention(t.Context(), f.Pool); err != nil {
		t.Fatal(err)
	}
	assertStatus(computer.StatusDeleted)
	var history bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT c.storage_reservation_bytes IS NULL AND c.initial_root_id IS NULL AND s.status='closed' FROM computers c JOIN sessions s ON s.environment_id=c.environment_id AND s.computer_id=c.id WHERE c.id=$1`, f.Computer).Scan(&history); err != nil || !history {
		t.Fatalf("storage release or history invalid: %v %v", history, err)
	}
	if result, err := computer.Delete(t.Context(), f.Pool, deletion); err != nil || !result.Replayed {
		t.Fatalf("deletion retry lost receipt: %+v %v", result, err)
	}
	admissions := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() { admissions <- start(fmt.Sprintf("after-delete-%d", i)) })
	}
	wg.Wait()
	close(admissions)
	accepted := 0
	for err := range admissions {
		if err == nil {
			accepted++
		} else if !errors.Is(err, agent.ErrNotReady) {
			t.Fatal(err)
		}
	}
	if accepted != 1 {
		t.Fatalf("released one Computer reservation, admitted %d", accepted)
	}
}

func TestComputerDeletionReleasesUninitializedStorage(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%t", failed), func(t *testing.T) {
			f, scope := computerStorageFixture(t)
			// Leave the original live Computer in place and make one free slot.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE environments SET max_reserved_storage_bytes=$2 WHERE id=$1`, f.Environment, 2*disk.SeedCapacity)
			creator := computer.NewCreator(nil)
			created, err := creator.Create(t.Context(), f.Pool, computer.Request{Scope: scope, DefinitionKey: "fixture-computer", IdempotencyKey: "first"})
			if err != nil {
				t.Fatal(err)
			}
			if failed {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET preparation_failed_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, f.Environment, created.ComputerID)
			}
			if _, err := computer.Delete(t.Context(), f.Pool, computer.Deletion{Scope: scope, ComputerID: created.ComputerID, IdempotencyKey: "delete"}); err != nil {
				t.Fatal(err)
			}
			if err := agent.ReconcileComputerDiskRetention(t.Context(), f.Pool); err != nil {
				t.Fatal(err)
			}
			snapshot, err := computer.Read(t.Context(), f.Pool, scope, created.ComputerID)
			if err != nil || snapshot.Status != computer.StatusDeleted {
				t.Fatalf("uninitialized deletion: %+v %v", snapshot, err)
			}
			if _, err := creator.Create(t.Context(), f.Pool, computer.Request{Scope: scope, DefinitionKey: "fixture-computer", IdempotencyKey: "second"}); err != nil {
				t.Fatalf("uninitialized Computer kept reservation: %v", err)
			}
			var originalRetained bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT storage_reservation_bytes=$2 AND initial_root_id IS NOT NULL FROM computers WHERE id=$1`, f.Computer, disk.SeedCapacity).Scan(&originalRetained); err != nil || !originalRetained {
				t.Fatalf("live Computer lost storage: %v %v", originalRetained, err)
			}
		})
	}
}
