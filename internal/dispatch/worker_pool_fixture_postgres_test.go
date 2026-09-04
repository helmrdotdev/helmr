package dispatch

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/runtimeid"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type dispatchWorkerPoolFixture struct {
	substrateFormat   string
	substrateContract string

	capacityCPUMillis               int64
	capacityMemoryBytes             int64
	capacityGuestEphemeralDiskBytes int64
	perVMCPUMillis                  int64
	perVMMemoryBytes                int64
	perVMGuestEphemeralDiskBytes    int64
	maxVMSlots                      int32
}

func seedDispatchWorkerPool(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	workerGroupID pgtype.UUID,
	spec dispatchWorkerPoolFixture,
) {
	t.Helper()
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO runtime_identities (
    id, runtime_arch, vm_runtime_contract, vm_runtime_descriptor_digest,
    firecracker_digest, firecracker_version, snapshot_format_version,
    host_kernel_release, cpu_template_kind,
    kernel_digest, initramfs_digest, rootfs_digest
) VALUES (
    $1, 'x86_64', $7, $2,
    $3, '1.16.1', '6.0.0', '6.8.0-test', 'none',
    $4, $5, $6
)`,
		dbtest.DefaultRuntimeID,
		dbtest.Digest("dispatch-vm-runtime-descriptor"),
		dbtest.Digest("dispatch-firecracker"),
		dbtest.Digest("dispatch-kernel"),
		dbtest.Digest("dispatch-initramfs"),
		dbtest.Digest("dispatch-rootfs"),
		runtimeid.Contract,
	)
	dbtest.MustExec(t, ctx, pool, `
INSERT INTO worker_pools (
    id, worker_group_id, name, state,
    runtime_identity_id, substrate_format, substrate_contract,
    capacity_cpu_millis, capacity_memory_bytes, capacity_guest_ephemeral_disk_bytes,
    per_vm_cpu_millis, per_vm_memory_bytes, per_vm_guest_ephemeral_disk_bytes,
    max_vm_slots, sealed_at
) VALUES (
    $1, $2, 'dispatch-test', 'active',
    $3, $4, $5,
    $6, $7, $8,
    $9, $10, $11,
    $12, now()
)`,
		dbtest.DefaultWorkerPoolID,
		workerGroupID,
		dbtest.DefaultRuntimeID,
		spec.substrateFormat,
		spec.substrateContract,
		spec.capacityCPUMillis,
		spec.capacityMemoryBytes,
		spec.capacityGuestEphemeralDiskBytes,
		spec.perVMCPUMillis,
		spec.perVMMemoryBytes,
		spec.perVMGuestEphemeralDiskBytes,
		spec.maxVMSlots,
	)
	maxVCPUs := int32((spec.perVMCPUMillis-1)/1000 + 1)
	for vcpu := int32(1); vcpu <= maxVCPUs; vcpu++ {
		dbtest.MustExec(t, ctx, pool, `
INSERT INTO worker_pool_cpu_shapes (worker_pool_id, vcpu_count, cpu_config_digest)
VALUES ($1, $2, $3)`,
			dbtest.DefaultWorkerPoolID,
			vcpu,
			dbtest.DefaultCPUConfigID,
		)
	}
	dbtest.MustExec(t, ctx, pool, `
UPDATE worker_groups
   SET primary_pool_id = $2
 WHERE id = $1`,
		workerGroupID,
		dbtest.DefaultWorkerPoolID,
	)
}

func dispatchCPUEnvironment(t *testing.T) ([]byte, string) {
	t.Helper()
	environment := workerapi.CPUEnvironment{
		FirecrackerVersion: "1.16.1",
		HostKernelRelease:  "6.8.0-test",
		MicrocodeVersion:   "0x00000001",
		BIOSVersion:        "dispatch-test",
		BIOSRevision:       "dispatch-test",
	}
	digest, err := environment.ExpectedDigest()
	if err != nil {
		t.Fatal(err)
	}
	environment.Digest = digest
	payload, err := json.Marshal(environment)
	if err != nil {
		t.Fatal(err)
	}
	return payload, digest
}
