package dispatch

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// CP bounds assigned seeds together with observed local construction tasks.
// The Worker rechecks the shared budget before physical admission.
func requireBootSeedCapacity(ctx context.Context, tx pgx.Tx, supply allocationSupply, shape computer.AllocationShape) (int64, error) {
	var revision, cpu, memory, slots, tasks, disk, rate, headroom, perCPU, perMemory, perScratch, localTasks int64
	err := tx.QueryRow(ctx, `SELECT p.resource_policy_revision,p.optional_cpu_millis,p.optional_memory_bytes,p.optional_vm_slots,
 p.optional_max_tasks,p.optional_disk_bytes,p.optional_io_bytes_per_second,p.local_headroom_slots,
 p.per_vm_cpu_millis,p.per_vm_memory_bytes,p.per_vm_guest_ephemeral_disk_bytes,COALESCE(h.local_optional_tasks,0)
 FROM worker_pools p JOIN worker_hosts h ON h.worker_pool_id=p.id WHERE h.id=$1 AND p.status='active'`, supply.host).Scan(&revision, &cpu, &memory, &slots, &tasks, &disk, &rate, &headroom, &perCPU, &perMemory, &perScratch, &localTasks)
	if err != nil {
		return 0, err
	}
	if disk <= 0 || rate <= 0 || tasks <= 0 {
		return 0, ErrNotReady
	}
	var all, seed allocationCapacity
	err = tx.QueryRow(ctx, `SELECT COALESCE(sum(reserved_cpu_millis),0),COALESCE(sum(reserved_memory_bytes),0),COALESCE(sum(reserved_scratch_bytes),0),count(*),count(*) FILTER(WHERE starting),
 COALESCE(sum(reserved_cpu_millis) FILTER(WHERE kind='seed'),0),COALESCE(sum(reserved_memory_bytes) FILTER(WHERE kind='seed'),0),count(*) FILTER(WHERE kind='seed')
 FROM (`+db.AllocationUsageSQL+`) usage WHERE worker_host_id=$1`, supply.host).Scan(&all.cpu, &all.memory, &all.scratch, &all.slots, &all.starts, &seed.cpu, &seed.memory, &seed.slots)
	if err != nil {
		return 0, err
	}
	fits := func(used, added, limit int64) bool { return added > 0 && added <= limit && used <= limit-added }
	if !fits(seed.cpu, shape.CPUMillis, cpu) || !fits(seed.memory, shape.MemoryBytes, memory) || !fits(seed.slots, 1, slots) || !fits(seed.slots+localTasks, 1, tasks) ||
		!fits(all.cpu, shape.CPUMillis, capacityAfterHeadroom(supply.capacity.cpu, perCPU, headroom)) || !fits(all.memory, shape.MemoryBytes, capacityAfterHeadroom(supply.capacity.memory, perMemory, headroom)) ||
		!fits(all.scratch, shape.ScratchBytes, capacityAfterHeadroom(supply.capacity.scratch, perScratch, headroom)) || !fits(all.slots, 1, supply.capacity.slots-headroom) || !fits(all.starts, 1, supply.capacity.starts) {
		return 0, ErrNotReady
	}
	return revision, nil
}

// Real grants and seed termination share the host's supply transaction. Logical
// release here never clears physical charges; Worker joins still own that proof.
func reclaimConflictingSeeds(ctx context.Context, tx pgx.Tx, supply allocationSupply, mandatory allocationCapacity, shape computer.AllocationShape) error {
	type seed struct {
		env, id              uuid.UUID
		cpu, memory, scratch int64
		starting             bool
	}
	rows, err := tx.Query(ctx, `SELECT environment_id,id,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,started_at IS NULL
 FROM boot_seed_builds WHERE worker_host_id=$1 AND fenced_at IS NULL ORDER BY environment_id,id`, supply.host)
	if err != nil {
		return err
	}
	seeds, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (seed, error) {
		var s seed
		err := row.Scan(&s.env, &s.id, &s.cpu, &s.memory, &s.scratch, &s.starting)
		return s, err
	})
	if err != nil {
		return err
	}
	used := mandatory
	used.cpu += shape.CPUMillis
	used.memory += shape.MemoryBytes
	used.scratch += shape.ScratchBytes
	used.slots++
	used.starts++
	for _, s := range seeds {
		used.cpu += s.cpu
		used.memory += s.memory
		used.scratch += s.scratch
		used.slots++
		if s.starting {
			used.starts++
		}
	}
	for _, s := range seeds {
		if used.cpu <= supply.capacity.cpu && used.memory <= supply.capacity.memory && used.scratch <= supply.capacity.scratch && used.slots <= supply.capacity.slots && used.starts <= supply.capacity.starts {
			break
		}
		if _, err := computer.LockBootSeedBuild(ctx, tx, s.env, s.id); err != nil {
			return err
		}
		if err := computer.TerminateBootSeedBuild(ctx, tx, s.env, s.id, "resource_reclaimed"); err != nil {
			return err
		}
		used.cpu -= s.cpu
		used.memory -= s.memory
		used.scratch -= s.scratch
		used.slots--
		if s.starting {
			used.starts--
		}
	}
	return nil
}

func capacityAfterHeadroom(capacity, perSlot, slots int64) int64 {
	if perSlot <= 0 || slots < 0 || slots > capacity/perSlot {
		return 0
	}
	return capacity - slots*perSlot
}
