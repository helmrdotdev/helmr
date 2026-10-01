package dispatch

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Assignment discovery acquires no authority. Supply is locked before Computer,
// Instance and member rows; the discovery receipt is revalidated under those locks.
type instanceAssignment struct {
	computer            db.Computer
	instance            db.ComputerInstance
	worker              db.SelectComputerInstanceCapacityRow
	checkpoint, program pgtype.UUID
	checkpointGroup     pgtype.UUID
	platform, cpuDigest string
	vcpu                int32
	cpu, memory, disk   int64
}

func discoverInstanceAssignment(ctx context.Context, tx pgx.Tx, environmentID, computerID pgtype.UUID) (instanceAssignment, error) {
	var p instanceAssignment
	var err error
	err = tx.QueryRow(ctx, `SELECT id,environment_id,region_id,revision,computer_spec_id FROM computers WHERE environment_id=$1 AND id=$2`, environmentID, computerID).Scan(&p.computer.ID, &p.computer.EnvironmentID, &p.computer.RegionID, &p.computer.Revision, &p.computer.ComputerSpecID)
	if err != nil {
		return p, err
	}
	spec, err := db.New(tx).GetComputerSpec(ctx, db.GetComputerSpecParams{EnvironmentID: environmentID, ID: p.computer.ComputerSpecID})
	if err != nil {
		return p, err
	}
	config, err := definition.ParseComputerConfig(spec.Config)
	if err != nil {
		return p, err
	}
	if config.Resources.MilliCPU <= 0 || config.Resources.MemoryMiB <= 0 || config.Resources.MemoryMiB > math.MaxInt64/(1024*1024) {
		return p, errors.New("invalid Computer resources")
	}
	p.cpu = config.Resources.MilliCPU
	p.memory = config.Resources.MemoryMiB * 1024 * 1024
	p.disk = disk.SeedCapacity
	var id pgtype.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM computer_instances WHERE environment_id=$1 AND computer_id=$2 AND reclaimed_at IS NULL`, environmentID, computerID).Scan(&id)
	if err == nil {
		p.instance, err = db.New(tx).GetComputerInstance(ctx, db.GetComputerInstanceParams{EnvironmentID: environmentID, ID: id})
		if err != nil {
			return p, err
		}
		p.worker = db.SelectComputerInstanceCapacityRow{WorkerGroupID: p.instance.WorkerGroupID, WorkerHostID: p.instance.WorkerHostID, WorkerEpoch: pgtype.Int8{Int64: p.instance.WorkerEpoch, Valid: true}, VMPlatformID: pgvalue.Text(p.instance.VMPlatformID)}
		return p, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return p, err
	}
	err = tx.QueryRow(ctx, `SELECT cp.id,cp.program_deployment_id,source.worker_group_id,source.vm_platform_id,source.vm_vcpu_count,source.cpu_config_digest
 FROM computer_checkpoints cp JOIN computer_instances source ON source.id=cp.source_computer_instance_id
 WHERE cp.environment_id=$1 AND cp.computer_id=$2 AND cp.status='ready' AND cp.resume_committed_at IS NULL
 ORDER BY cp.created_at DESC,cp.id DESC LIMIT 1`, environmentID, computerID).Scan(&p.checkpoint, &p.program, &p.checkpointGroup, &p.platform, &p.vcpu, &p.cpuDigest)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return p, err
	}
	p.worker, err = db.New(tx).SelectComputerInstanceCapacity(ctx, db.SelectComputerInstanceCapacityParams{
		RegionID: p.computer.RegionID, ObservationFreshnessSeconds: workergroup.ObservationFreshnessSeconds,
		RunArchitecture: runtimeArchitecture, Contract: vmplatform.Contract,
		RequiredCPUMillis: p.cpu, RequiredMemoryBytes: p.memory, RequiredGuestEphemeralDiskBytes: p.disk,
		RequiredWorkerGroupID: p.checkpointGroup, RequiredVMPlatformID: p.platform, RequiredVMVCPUCount: p.vcpu, RequiredCPUConfigDigest: p.cpuDigest,
	})
	return p, err
}

func lockInstanceAssignment(ctx context.Context, tx pgx.Tx, p instanceAssignment) (instanceAssignment, error) {
	if !p.worker.WorkerEpoch.Valid || !p.worker.VMPlatformID.Valid {
		return p, ErrCapacityUnavailable
	}
	if _, err := workergroup.LockDispatchSupply(ctx, tx, workergroup.DispatchSupply{GroupID: p.worker.WorkerGroupID, RegionID: p.computer.RegionID, HostID: p.worker.WorkerHostID, Epoch: p.worker.WorkerEpoch.Int64, RunArchitecture: runtimeArchitecture, RequirePrimary: !p.instance.ID.Valid && !p.checkpoint.Valid}); err != nil {
		return p, err
	}
	c, err := db.New(tx).LockComputer(ctx, db.LockComputerParams{EnvironmentID: p.computer.EnvironmentID, ID: p.computer.ID})
	if err != nil {
		return p, err
	}
	if c.Revision != p.computer.Revision || c.Status != "active" || c.DesiredState != "active" || c.DeletedAt.Valid || len(c.RecoveryFailure) > 0 || len(c.PreparationFailure) > 0 || c.DirtyState == "dirty_state_lost" || c.DirtyState == "capture_failed" {
		return p, ErrCandidateChanged
	}
	p.computer = c
	live, err := db.New(tx).LockComputerInstance(ctx, db.LockComputerInstanceParams{EnvironmentID: c.EnvironmentID, ComputerID: c.ID})
	if p.instance.ID.Valid {
		if err != nil {
			return p, err
		}
		if live.ID != p.instance.ID || live.WorkerEpoch != p.worker.WorkerEpoch.Int64 || live.WorkerHostID != p.worker.WorkerHostID || live.WriterGeneration != c.WriterGeneration {
			return p, ErrCandidateChanged
		}
		p.instance = live
		return p, nil
	}
	if err == nil {
		return p, ErrCandidateChanged
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return p, err
	}
	// A parked process cannot silently fall back to a fresh disk start.
	var valid bool
	err = tx.QueryRow(ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM run_waits w WHERE w.computer_id=$1 AND w.suspension_status IN ('parked','resume_pending','resuming') AND w.suspend_checkpoint_id IS DISTINCT FROM $2::uuid)
 AND ($2::uuid IS NOT NULL OR NOT EXISTS(SELECT 1 FROM computer_checkpoints cp WHERE cp.computer_id=$1 AND cp.status='ready' AND cp.resume_committed_at IS NULL))`, c.ID, p.checkpoint).Scan(&valid)
	if err != nil {
		return p, err
	}
	if !valid {
		return p, ErrCandidateChanged
	}
	if err = workergroup.CheckHostInstanceAdmission(ctx, tx, p.worker.WorkerHostID, p.worker.WorkerEpoch.Int64); err != nil {
		return p, err
	}
	err = tx.QueryRow(ctx, `SELECT h.per_vm_cpu_millis >= $3 AND h.per_vm_memory_bytes >= $4 AND h.per_vm_guest_ephemeral_disk_bytes >= $5
 AND h.max_vm_slots > count(i.id) AND h.max_vm_starts > count(i.id) FILTER (WHERE i.observed_state='allocated')
 AND h.epoch_cpu_millis-COALESCE(sum(i.reserved_cpu_millis),0)>=$3
 AND h.epoch_memory_bytes-COALESCE(sum(i.reserved_memory_bytes),0)>=$4
 AND h.epoch_guest_ephemeral_disk_bytes-COALESCE(sum(i.reserved_guest_ephemeral_disk_bytes),0)>=$5
 FROM worker_hosts h LEFT JOIN computer_instances i ON i.worker_host_id=h.id AND i.worker_epoch=h.current_epoch AND i.reclaimed_at IS NULL
 WHERE h.id=$1 AND h.current_epoch=$2 GROUP BY h.id`, p.worker.WorkerHostID, p.worker.WorkerEpoch.Int64, p.cpu, p.memory, p.disk).Scan(&valid)
	if err != nil {
		return p, err
	}
	if !valid {
		return p, ErrCapacityUnavailable
	}
	err = tx.QueryRow(ctx, `SELECT shape.vcpu_count,shape.cpu_config_digest FROM worker_hosts h JOIN worker_pool_cpu_shapes shape ON shape.worker_pool_id=h.worker_pool_id
 WHERE h.id=$1 AND shape.vcpu_count=ceil($2::numeric/1000)::integer
 AND ($3::text='' OR shape.cpu_config_digest=$3)`, p.worker.WorkerHostID, p.cpu, p.cpuDigest).Scan(&p.vcpu, &p.cpuDigest)
	return p, err
}

// The caller has locked the member and checked its admission before allocating.
func (d *Authority) allocateInstanceAssignment(ctx context.Context, tx pgx.Tx, p instanceAssignment) (db.ComputerInstance, error) {
	if p.computer.WriterGeneration == math.MaxInt64 {
		return db.ComputerInstance{}, errors.New("computer writer generation exhausted")
	}
	id := uuid.NewV7()
	generation := p.computer.WriterGeneration + 1
	hash, err := computer.WriterTokenHash(d.fencingKey, id, pgvalue.MustUUIDValue(p.computer.ID), generation)
	if err != nil {
		return db.ComputerInstance{}, err
	}
	i, err := db.New(tx).AllocateComputerInstance(ctx, db.AllocateComputerInstanceParams{
		ID: pgvalue.UUID(id), WorkerGroupID: p.worker.WorkerGroupID, WorkerHostID: p.worker.WorkerHostID, WorkerEpoch: p.worker.WorkerEpoch.Int64,
		VMPlatformID: p.worker.VMPlatformID.String, VMVCPUCount: p.vcpu, CPUConfigDigest: p.cpuDigest,
		ReservedCPUMillis: p.cpu, ReservedMemoryBytes: p.memory, ReservedGuestEphemeralDiskBytes: p.disk, ReservedExecutionSlots: 1,
		ProgramDeploymentID: p.program, SourceCheckpointID: p.checkpoint, PreparationSeconds: int64(computer.PreparationTTL / time.Second), Reason: "computer_preparation",
		WriterTokenHash: hash, WriterTtlSeconds: int64(computer.WriterTTL / time.Second), WriterGeneration: generation,
		ComputerID: p.computer.ID, EnvironmentID: p.computer.EnvironmentID, ComputerSpecID: p.computer.ComputerSpecID,
	})
	if err != nil {
		return i, fmt.Errorf("allocate Computer Instance: %w", err)
	}
	if err = admitComputerPreparation(ctx, tx, p.computer.ID, i.ID); err != nil {
		return i, err
	}
	return i, nil
}

// Allocation and its Computer-owned charge commit together. The caller holds
// Computer and Instance authority; any rejection rolls back the allocation.
func admitComputerPreparation(ctx context.Context, tx pgx.Tx, computerID, instanceID pgtype.UUID) error {
	_, err := db.New(tx).ChargeComputerPreparation(ctx, db.ChargeComputerPreparationParams{ComputerID: computerID, InstanceID: instanceID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCandidateChanged
	}
	return err
}
