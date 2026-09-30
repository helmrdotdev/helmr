package workergroup

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/db/schema"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const fixtureRegionID = "us-east-1"

// supplyFixture is a test database with one region, one active worker group
// created by CreateGroup and a VM platform for sealing pools.
type supplyFixture struct {
	pool            *pgxpool.Pool
	q               *db.Queries
	group           db.WorkerGroup
	enrollmentToken string
	vmPlatformID    string
	cpuConfigDigest string
}

func newSupplyFixture(t *testing.T) supplyFixture {
	t.Helper()
	database := dbtest.Open(t)
	if err := schema.Up(t.Context(), database.DSN); err != nil {
		t.Fatal(err)
	}
	q := db.New(database.Pool)
	dbtest.MustExec(t, t.Context(), database.Pool, `INSERT INTO regions (id, display_name) VALUES ($1, 'Worker Supply Test')`, fixtureRegionID)
	created, err := CreateGroup(t.Context(), database.Pool, GroupInput{RegionID: fixtureRegionID, Name: "supply-test"})
	if err != nil {
		t.Fatal(err)
	}
	fixture := supplyFixture{
		pool: database.Pool, q: q, group: created.Group, enrollmentToken: created.EnrollmentToken,
		vmPlatformID:    dbtest.Digest("worker-pool-runtime"),
		cpuConfigDigest: dbtest.Digest("worker-pool-cpu"),
	}
	if _, err := q.UpsertVMPlatform(t.Context(), db.UpsertVMPlatformParams{
		ID:                    fixture.vmPlatformID,
		Arch:                  "x86_64",
		Contract:              vmplatform.Contract,
		DescriptorDigest:      dbtest.Digest("worker-pool-runtime-descriptor"),
		FirecrackerDigest:     dbtest.Digest("worker-pool-firecracker"),
		FirecrackerVersion:    "1.12.0",
		SnapshotFormatVersion: "1.0.0",
		HostKernelRelease:     "6.12.0",
		CPUTemplateKind:       "none",
		KernelDigest:          dbtest.Digest("worker-pool-kernel"),
		InitramfsDigest:       dbtest.Digest("worker-pool-initramfs"),
		RootfsDigest:          dbtest.Digest("worker-pool-rootfs"),
	}); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f supplyFixture) groupID() uuid.UUID {
	return pgvalue.MustUUIDValue(f.group.ID)
}

func (f supplyFixture) currentGroup(t *testing.T) db.WorkerGroup {
	t.Helper()
	group, err := GetGroup(t.Context(), f.q, f.groupID())
	if err != nil {
		t.Fatal(err)
	}
	return group
}

func (f supplyFixture) pendingPool(t *testing.T, name string) db.WorkerPool {
	t.Helper()
	_, pending, err := CreatePool(t.Context(), f.pool, f.groupID(), name, f.currentGroup(t).ClaimVersion)
	if err != nil {
		t.Fatal(err)
	}
	return pending
}

// activePool creates a pool and seals it the way the first activating host
// does.
func (f supplyFixture) activePool(t *testing.T, name string) db.WorkerPool {
	t.Helper()
	pending := f.pendingPool(t, name)
	rows, err := f.q.InsertWorkerPoolCPUShape(t.Context(), db.InsertWorkerPoolCPUShapeParams{
		VCPUCount: 1, CPUConfigDigest: f.cpuConfigDigest, WorkerPoolID: pending.ID,
	})
	if err != nil || rows != 1 {
		t.Fatalf("insert CPU shape rows = %d, err = %v", rows, err)
	}
	sealed, err := f.q.SealWorkerPool(t.Context(), db.SealWorkerPoolParams{
		VMPlatformID:                    pgvalue.Text(f.vmPlatformID),
		CapacityCPUMillis:               pgtype.Int8{Int64: 4_000, Valid: true},
		CapacityMemoryBytes:             pgtype.Int8{Int64: 8 << 30, Valid: true},
		CapacityGuestEphemeralDiskBytes: pgtype.Int8{Int64: 32 << 30, Valid: true},
		PerVMCPUMillis:                  pgtype.Int8{Int64: 1_000, Valid: true},
		PerVMMemoryBytes:                pgtype.Int8{Int64: 1 << 30, Valid: true},
		PerVMGuestEphemeralDiskBytes:    pgtype.Int8{Int64: 4 << 30, Valid: true},
		MaxVMSlots:                      pgtype.Int4{Int32: 4, Valid: true},
		WorkerPoolID:                    pending.ID,
		WorkerGroupID:                   f.group.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// activeHost inserts an active host of the pool at epoch 1 and claim
// version 1.
func (f supplyFixture) activeHost(t *testing.T, pool db.WorkerPool, resourceID string) uuid.UUID {
	t.Helper()
	hostID := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `
INSERT INTO worker_hosts (
    id, resource_id, worker_group_id, worker_pool_id, status,
    current_epoch, current_service_id, vm_platform_id,
    epoch_cpu_millis, epoch_memory_bytes, epoch_guest_ephemeral_disk_bytes,
    per_vm_cpu_millis, per_vm_memory_bytes, per_vm_guest_ephemeral_disk_bytes,
    max_vm_slots, max_vm_starts,
    cpu_environment, cpu_environment_digest, observed_at,
    epoch_started_at, activated_at
) VALUES (
    $1, $2, $3, $4, 'active',
    1, $5, $6,
    4000, 8589934592, 34359738368,
    1000, 1073741824, 4294967296,
    4, 4, '{"vendor":"test"}'::jsonb, $7, now(), now(), now()
)`, hostID, resourceID, f.group.ID, pool.ID, uuid.NewV7(), f.vmPlatformID, dbtest.Digest("supply-cpu-environment"))
	return hostID
}
