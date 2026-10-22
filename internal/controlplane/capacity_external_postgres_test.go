package controlplane

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
)

// External consumers run their own executable against the real HTTP router and
// a disposable Product-owned database. No consumer imports Product packages.
func TestCapacityExternalConsumer(t *testing.T) {
	executable := os.Getenv("HELMR_CAPACITY_CONSUMER_TEST")
	if executable == "" {
		t.Skip("set HELMR_CAPACITY_CONSUMER_TEST to a consumer contract executable")
	}
	f := sessiontest.New(t)
	var poolID uuid.UUID
	var poolName string
	if err := f.Pool.QueryRow(t.Context(), "SELECT p.id,p.name FROM worker_hosts h JOIN worker_pools p ON p.id=h.worker_pool_id WHERE h.id=$1", f.Worker).Scan(&poolID, &poolName); err != nil {
		t.Fatal(err)
	}
	// A capacity consumer selects only hosts with a fresh observation.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET observed_at=clock_timestamp(),resource_id=id::text WHERE id=$1`, f.Worker)
	emptyHost := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO worker_hosts (
 id,resource_id,worker_group_id,worker_pool_id,status,current_epoch,current_service_id,vm_platform_id,
 epoch_cpu_millis,epoch_memory_bytes,epoch_guest_ephemeral_disk_bytes,
 per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,
 max_vm_slots,max_vm_starts,cpu_environment,cpu_environment_digest,observed_at,epoch_started_at,activated_at)
 SELECT $2,$3,worker_group_id,worker_pool_id,status,current_epoch,$4,vm_platform_id,
 epoch_cpu_millis,epoch_memory_bytes,epoch_guest_ephemeral_disk_bytes,
 per_vm_cpu_millis,per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,
 max_vm_slots,max_vm_starts,cpu_environment,cpu_environment_digest,observed_at,epoch_started_at,activated_at
 FROM worker_hosts WHERE id=$1`, f.Worker, emptyHost, emptyHost.String(), uuid.NewV7())
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET resource_observation_sequence=1,resource_policy_revision=1,resource_observed_at=clock_timestamp(),local_optional_cpu_millis=0,local_optional_memory_bytes=0,optional_disk_bytes=0,local_optional_vm_slots=0,local_optional_tasks=0,optional_reclaim_pending=false,disk_slots_for_real_work=max_vm_slots,ready_leaf_keys='[]' WHERE worker_group_id=$1`, f.Group)
	server := httptest.NewServer(newPostgresServer(t, f.Pool, func(c *ServerConfig) { c.CapacityToken = capacityTestToken() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable)
	cmd.Env = append(os.Environ(),
		"CAPACITY_TEST_URL="+server.URL,
		"CAPACITY_TEST_TOKEN="+capacityTestToken(),
		"CAPACITY_TEST_REGION="+"test",
		"CAPACITY_TEST_GROUP="+"group",
		"CAPACITY_TEST_GROUP_ID="+f.Group.String(),
		"CAPACITY_TEST_POOL_ID="+poolID.String(),
		"CAPACITY_TEST_POOL_NAME="+poolName,
		"CAPACITY_TEST_HOST_ID="+f.Worker.String(),
		"CAPACITY_TEST_EMPTY_HOST_ID="+emptyHost.String())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("external capacity consumer: %v\n%s", err, output)
	}
	var status string
	var fenced bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status, lost_at IS NOT NULL FROM worker_hosts WHERE id=$1`, f.Worker).Scan(&status, &fenced); err != nil {
		t.Fatal(err)
	}
	if status != "lost" || !fenced {
		t.Fatalf("consumer did not persist provider absence: status=%s fenced=%v", status, fenced)
	}
}
