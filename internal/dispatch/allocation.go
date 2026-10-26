package dispatch

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// Allocator is a trusted control-plane owner. Workers cannot choose its targets,
// instance identities, resource amounts or epochs. Restart recovery requires the
// same configured channel key; a mismatching key never replaces a stored digest.
type ImageBuildTrustIssuer interface {
	GeneratePreparationProxyTrust(environmentID, buildID uuid.UUID, createdAt, deadline time.Time) (secret.ProxyTrust, error)
}

type Allocator struct {
	auditPolicy auditpolicy.Policy
	database    db.TxDB
	key         [32]byte
	trust       ImageBuildTrustIssuer
}

func NewAllocator(database db.TxDB, auditPolicy auditpolicy.Policy, channelKey []byte, trust ImageBuildTrustIssuer) (*Allocator, error) {
	if database == nil || len(channelKey) != 32 {
		return nil, ErrInvalidInput
	}
	if err := auditPolicy.Validate(); err != nil {
		return nil, err
	}
	a := &Allocator{database: database, trust: trust, auditPolicy: auditPolicy}
	copy(a.key[:], channelKey)
	return a, nil
}

func (a *Allocator) credential(domain string, receipt computer.Allocation) []byte {
	mac := hmac.New(sha256.New, a.key[:])
	_, _ = mac.Write([]byte(domain))
	for _, id := range []uuid.UUID{receipt.EnvironmentID, receipt.OwnerID, receipt.InstanceID, receipt.HostID} {
		_, _ = mac.Write(id[:])
	}
	var epochs [16]byte
	binary.BigEndian.PutUint64(epochs[:8], uint64(receipt.Epoch))
	binary.BigEndian.PutUint64(epochs[8:], uint64(receipt.HostEpoch))
	_, _ = mac.Write(epochs[:])
	return mac.Sum(nil)
}

func (a *Allocator) imageBuildCredential(receipt computer.Allocation) []byte {
	return a.credential("helmr.preparation-channel.v1\x00", receipt)
}

func (a *Allocator) leaseCredential(receipt computer.Allocation) string {
	return base64.RawURLEncoding.EncodeToString(a.credential("helmr.computer-channel.v1\x00", receipt))
}

var errEnvironmentCapacity = fmt.Errorf("%w: environment allocation capacity", ErrNotReady)

type allocationCapacity struct {
	cpu, memory, scratch, slots, starts int64
}

type allocationSupply struct {
	group, host uuid.UUID
	epoch       int64
	capacity    allocationCapacity
	maxMemory   int64
	maxCPU      int64
	shape       computer.AllocationShape
}

// Every wait is bounded, including implicit FK/unique waits after Host UPDATE.
// A failed candidate rolls back before the caller tries another Host.

func lockAllocationSupply(ctx context.Context, tx pgx.Tx, env, host uuid.UUID, primary bool) (allocationSupply, error) {
	s := allocationSupply{host: host}
	if err := workergroup.LockExecutionSupply(ctx, tx, env); err != nil {
		return s, err
	}
	var region string
	if err := tx.QueryRow(ctx, `SELECT p.default_region_id FROM environments e JOIN projects p ON p.id=e.project_id WHERE e.id=$1`, env).Scan(&region); err != nil {
		return s, err
	}
	if err := tx.QueryRow(ctx, `SELECT worker_group_id,current_epoch FROM worker_hosts WHERE id=$1 AND current_epoch IS NOT NULL`, host).Scan(&s.group, &s.epoch); err != nil {
		return s, err
	}
	admitting, err := workergroup.LockDispatchSupply(ctx, tx, workergroup.DispatchSupply{
		GroupID: pgvalue.UUID(s.group), HostID: pgvalue.UUID(host), Epoch: s.epoch,
		RegionID: region, RequirePrimary: primary,
	})
	if err != nil {
		return s, err
	}
	if !admitting {
		return s, ErrNotReady
	}
	err = tx.QueryRow(ctx, `SELECT epoch_cpu_millis,epoch_memory_bytes,epoch_guest_ephemeral_disk_bytes,max_vm_slots,
 per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,vm_platform_id,per_vm_cpu_millis,max_vm_starts FROM worker_hosts WHERE id=$1`, host).Scan(
		&s.capacity.cpu, &s.capacity.memory, &s.capacity.scratch, &s.capacity.slots, &s.maxMemory, &s.shape.ScratchBytes, &s.shape.VMPlatformID, &s.maxCPU, &s.capacity.starts)
	return s, err
}

func qualifyAllocationShape(ctx context.Context, tx pgx.Tx, supply allocationSupply, cpuMillis, memoryBytes int64) (computer.AllocationShape, error) {
	shape := supply.shape
	var err error
	shape.CPUMillis, err = vm.ReservedCPUMillis(cpuMillis)
	if err != nil {
		return shape, ErrInvalidInput
	}
	shape.MemoryBytes = memoryBytes
	shape.VCPUCount = shape.CPUMillis / 1000
	if shape.MemoryBytes > supply.maxMemory || shape.CPUMillis > supply.maxCPU || shape.ScratchBytes <= 0 {
		return shape, ErrNotReady
	}
	// The sealed pool enumerates every physically qualified whole-vCPU shape.
	err = tx.QueryRow(ctx, `SELECT s.cpu_config_digest FROM worker_pool_cpu_shapes s JOIN worker_hosts h ON h.worker_pool_id=s.worker_pool_id
 WHERE h.id=$1 AND s.vcpu_count=$2`, supply.host, shape.VCPUCount).Scan(&shape.CPUConfigDigest)
	return shape, err
}

func requireAllocationCapacity(ctx context.Context, tx pgx.Tx, env uuid.UUID, supply allocationSupply, shape computer.AllocationShape, computerID uuid.UUID, imageBuildID uuid.UUID) error {
	var host allocationCapacity
	budget, err := db.ReadExecutionBudgetForComputer(ctx, tx, env, computerID)
	if imageBuildID != uuid.Nil() {
		budget, err = db.ReadImageBuildExecutionBudget(ctx, tx, env, imageBuildID)
	}
	if err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(reserved_cpu_millis),0),COALESCE(sum(reserved_memory_bytes),0),COALESCE(sum(reserved_scratch_bytes),0),count(*),count(*) FILTER (WHERE starting) FROM (`+db.AllocationUsageSQL+`) allocation_usage WHERE worker_host_id=$1 AND kind NOT IN ('seed','retained')`, supply.host).Scan(&host.cpu, &host.memory, &host.scratch, &host.slots, &host.starts); err != nil {
		return err
	}
	fits := func(used, added, limit int64) bool { return added > 0 && added <= limit && used <= limit-added }
	if !budget.Fits(shape.CPUMillis, shape.MemoryBytes, computerID != uuid.Nil()) {
		return errEnvironmentCapacity
	}
	if !fits(host.cpu, shape.CPUMillis, supply.capacity.cpu) ||
		!fits(host.memory, shape.MemoryBytes, supply.capacity.memory) || !fits(host.scratch, shape.ScratchBytes, supply.capacity.scratch) ||
		!fits(host.slots, 1, supply.capacity.slots) || !fits(host.starts, 1, supply.capacity.starts) {
		return ErrNotReady
	}
	return reclaimConflictingSeeds(ctx, tx, supply, host, shape)
}

func requireAllocationFreshSupply(ctx context.Context, tx pgx.Tx, supply allocationSupply) error {
	var fresh bool
	err := tx.QueryRow(ctx, `SELECT observed_at>=clock_timestamp()-$2*interval '1 second' FROM worker_hosts WHERE id=$1`, supply.host, workergroup.ObservationFreshnessSeconds).Scan(&fresh)
	if err != nil {
		return err
	}
	if !fresh {
		return ErrNotReady
	}
	return nil
}

func ReadImageBuildAllocation(ctx context.Context, q db.DBTX, env, preparation uuid.UUID) (computer.Allocation, error) {
	r := computer.Allocation{EnvironmentID: env, OwnerID: preparation}
	err := q.QueryRow(ctx, `SELECT allocation_id,executor_epoch,worker_host_id,worker_epoch,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,
 vm_platform_id,vm_vcpu_count,cpu_config_digest,status,delivered_at,executor_expires_at,fenced_at,channel_credential_digest FROM image_builds
 WHERE environment_id=$1 AND id=$2 AND worker_host_id IS NOT NULL`, env, preparation).Scan(&r.InstanceID, &r.Epoch, &r.HostID, &r.HostEpoch,
		&r.Shape.CPUMillis, &r.Shape.MemoryBytes, &r.Shape.ScratchBytes, &r.Shape.VMPlatformID, &r.Shape.VCPUCount, &r.Shape.CPUConfigDigest,
		&r.Status, &r.DeliveredAt, &r.ExpiresAt, &r.FencedAt, &r.CredentialDigest)
	return r, err
}

func ReadLeaseAllocation(ctx context.Context, q db.DBTX, env, computerID uuid.UUID, epoch int64) (computer.ComputerAllocation, error) {
	r := computer.ComputerAllocation{EnvironmentID: env, OwnerID: computerID, Epoch: epoch}
	err := q.QueryRow(ctx, `SELECT l.allocation_id,l.worker_host_id,l.worker_epoch,l.reserved_cpu_millis,l.reserved_memory_bytes,l.reserved_scratch_bytes,
 l.vm_platform_id,l.vm_vcpu_count,l.cpu_config_digest,l.status,l.delivered_at,l.expires_at,l.fenced_at,COALESCE(l.restored_from_save_id::text,'sha256:'||encode(c.initial_root_digest,'hex')),l.restored_from_save_id,l.disk_only_recovery,l.initialized_at,l.channel_credential_digest
 FROM computer_leases l JOIN computers c ON (c.environment_id,c.id)=(l.environment_id,l.computer_id) WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3`, env, computerID, epoch).Scan(&r.InstanceID, &r.HostID, &r.HostEpoch,
		&r.Shape.CPUMillis, &r.Shape.MemoryBytes, &r.Shape.ScratchBytes, &r.Shape.VMPlatformID, &r.Shape.VCPUCount, &r.Shape.CPUConfigDigest,
		&r.Status, &r.DeliveredAt, &r.ExpiresAt, &r.FencedAt, &r.BaseVersion, &r.RestoredFrom, &r.DiskOnlyRecovery, &r.InitializedAt, &r.CredentialDigest)
	return r, err
}

// AllocateImageBuild reserves one private executor atomically. Its admitted
// deadline is unchanged. A historical allocation is returned before considering
// a new candidate, even if that allocation has since expired or been fenced.
func (a *Allocator) AllocateImageBuild(ctx context.Context, env, preparation, host uuid.UUID) (computer.Allocation, error) {
	if env == uuid.Nil() || preparation == uuid.Nil() || host == uuid.Nil() {
		return computer.Allocation{}, ErrInvalidInput
	}
	if prior, err := ReadImageBuildAllocation(ctx, a.database, env, preparation); err == nil {
		return prior, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return computer.Allocation{}, err
	}
	var result computer.Allocation
	err := db.RunTx(ctx, a.database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		supply, err := lockAllocationSupply(ctx, tx, env, host, true)
		if err != nil {
			return err
		}
		_, err = org.LockAdmission(ctx, tx, env)
		if err != nil {
			return err
		}
		before, err := workergroup.ReadExecutionPlacement(ctx, tx, env)
		if err != nil {
			return err
		}
		p, err := computer.ReadImageBuild(ctx, tx, env, preparation)
		if err != nil {
			return err
		}
		if err := computer.LockImageSpecSecrets(ctx, tx, env, p.SpecID); err != nil {
			return err
		}
		if err := computer.LockImageSpec(ctx, tx, env, p.SpecID); err != nil {
			return err
		}
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM image_builds WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, preparation).Scan(&locked); err != nil {
			return err
		}
		if prior, err := ReadImageBuildAllocation(ctx, tx, env, preparation); err == nil {
			result = prior
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var cpuMillis, memoryBytes int64
		if err := tx.QueryRow(ctx, `SELECT cpu_millis,memory_bytes FROM template_definitions WHERE environment_id=$1 AND image_spec_id=$2 ORDER BY deployment_id,template_id LIMIT 1`, env, p.SpecID).Scan(&cpuMillis, &memoryBytes); err != nil {
			return err
		}
		shape, err := qualifyAllocationShape(ctx, tx, supply, cpuMillis, memoryBytes)
		if err != nil {
			return err
		}
		if err := requireAllocationCapacity(ctx, tx, env, supply, shape, uuid.Nil(), preparation); err != nil {
			return err
		}
		result = computer.Allocation{EnvironmentID: env, OwnerID: preparation, InstanceID: uuid.NewV7(), Epoch: 1, HostID: host, HostEpoch: supply.epoch, Shape: shape, Status: "running"}
		digest := sha256.Sum256(a.imageBuildCredential(result))
		result.CredentialDigest = digest[:]
		assigned, err := computer.AssignImageBuild(ctx, tx, result, p.SpecID)
		if err != nil {
			return err
		}
		if !assigned {
			return ErrNotReady
		}
		var protected bool
		var now, deadline time.Time
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM secret_bindings b WHERE b.environment_id=p.environment_id AND b.image_spec_id=p.image_spec_id AND b.mode='protected'),clock_timestamp(),p.deadline_at FROM image_builds p WHERE p.environment_id=$1 AND p.id=$2`, env, preparation).Scan(&protected, &now, &deadline); err != nil {
			return err
		}
		if protected {
			if a.trust == nil {
				return errors.New("preparation Secret trust issuer is required")
			}
			trust, err := a.trust.GeneratePreparationProxyTrust(env, preparation, now, deadline)
			if err != nil {
				return err
			}
			if err := computer.RecordImageBuildProxyTrust(ctx, tx, env, preparation, trust); err != nil {
				return err
			}
		}
		if err := requireAllocationFreshSupply(ctx, tx, supply); err != nil {
			return err
		}
		var current bool
		if err := tx.QueryRow(ctx, `SELECT deadline_at>clock_timestamp() FROM image_builds WHERE environment_id=$1 AND id=$2`, env, preparation).Scan(&current); err != nil {
			return err
		}
		if !current {
			return ErrNotReady
		}
		return workergroup.RequireExecutionPlacement(ctx, tx, env, uuid.Nil(), before)
	})
	if err != nil {
		return computer.Allocation{}, allocationError(err)
	}
	return result, nil
}

// AllocateFreshLease is only for a first physical execution. The CP retains
// the requested epoch across retries; requesting the next epoch is a new attempt,
// permissible only after all previous VMs stopped without ever becoming ready.
func (a *Allocator) AllocateFreshLease(ctx context.Context, env, computerID uuid.UUID, epoch int64, host uuid.UUID) (computer.ComputerAllocation, error) {
	if env == uuid.Nil() || computerID == uuid.Nil() || epoch <= 0 || host == uuid.Nil() {
		return computer.ComputerAllocation{}, ErrInvalidInput
	}
	if prior, err := ReadLeaseAllocation(ctx, a.database, env, computerID, epoch); err == nil {
		if prior.RestoredFrom != nil || prior.DiskOnlyRecovery {
			return computer.ComputerAllocation{}, ErrConflict
		}
		return prior, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return computer.ComputerAllocation{}, err
	}
	var result computer.ComputerAllocation
	err := db.RunTx(ctx, a.database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		supply, err := lockAllocationSupply(ctx, tx, env, host, true)
		if err != nil {
			return err
		}
		_, err = org.LockAdmission(ctx, tx, env)
		if err != nil {
			return err
		}
		before, err := workergroup.ReadExecutionPlacement(ctx, tx, env)
		if err != nil {
			return err
		}
		sessions, err := session.LockInitialSessions(ctx, tx, env, computerID)
		if err != nil {
			return err
		}
		if prior, err := ReadLeaseAllocation(ctx, tx, env, computerID, epoch); err == nil {
			if prior.RestoredFrom != nil || prior.DiskOnlyRecovery {
				return ErrConflict
			}
			result = prior
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		allowed, err := initialSessionDemand(ctx, a.auditPolicy, tx, env, computerID, sessions)
		if err != nil {
			return err
		}
		if !allowed {
			return nil
		} // Commit revoked input settlement without reserving a VM.
		var cpuMillis, memoryBytes int64
		var base string
		if err := tx.QueryRow(ctx, `SELECT cpu_millis,memory_bytes,'sha256:'||encode(initial_root_digest,'hex') FROM computers c WHERE environment_id=$1 AND id=$2
 AND initial_root_id IS NOT NULL AND image_id IS NOT NULL AND preparation_failed_at IS NULL AND deleted_at IS NULL AND integrity_fault_at IS NULL AND recovery_save_id IS NULL
 AND execution_deadline_at>clock_timestamp()
 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=$1 AND l.computer_id=$2 AND (l.fenced_at IS NULL OR l.initialized_at IS NOT NULL))
 AND (SELECT COALESCE(max(l.epoch),0) FROM computer_leases l WHERE l.environment_id=$1 AND l.computer_id=$2)=$3::bigint-1`, env, computerID, epoch).Scan(&cpuMillis, &memoryBytes, &base); err != nil {
			return err
		}
		shape, err := qualifyAllocationShape(ctx, tx, supply, cpuMillis, memoryBytes)
		if err != nil {
			return err
		}
		if err := requireAllocationCapacity(ctx, tx, env, supply, shape, computerID, uuid.Nil()); err != nil {
			return err
		}
		result = computer.ComputerAllocation{EnvironmentID: env, OwnerID: computerID, InstanceID: uuid.NewV7(), Epoch: epoch, HostID: host, HostEpoch: supply.epoch, Shape: shape, Status: "acquiring", BaseVersion: base}
		if err := a.insertComputerAllocation(ctx, tx, &result); err != nil {
			return err
		}
		if err := session.AssignInitialProcesses(ctx, a.auditPolicy, tx, result, sessions); err != nil {
			return err
		}
		if err := requireAllocationFreshSupply(ctx, tx, supply); err != nil {
			return err
		}
		if err := computer.RequireSecretsNotRevoked(ctx, tx, env, computerID); err != nil {
			return err
		}
		return workergroup.RequireExecutionPlacement(ctx, tx, env, uuid.Nil(), before)
	})
	if err != nil {
		return computer.ComputerAllocation{}, allocationError(err)
	}
	if result.InstanceID == uuid.Nil() {
		return computer.ComputerAllocation{}, ErrNotReady
	}
	return result, nil
}

func (a *Allocator) insertComputerAllocation(ctx context.Context, tx pgx.Tx, r *computer.ComputerAllocation) error {
	digest := sha256.Sum256([]byte(a.leaseCredential(r.Allocation)))
	r.CredentialDigest = digest[:]
	return computer.InsertLease(ctx, tx, *r)
}

// AllocateRestoredLease reserves the full physical shape before a Host may
// materialize a retained checkpoint. It never falls back to fresh boot or setup.
// A coherent restore resumes previously admitted native effects, including an
// operator-requested wake. Origin revocation blocks new business admission, not
// continuation of those effects; restored processes reauthorize new work.
func (a *Allocator) AllocateRestoredLease(ctx context.Context, env, checkpoint uuid.UUID, epoch int64, host uuid.UUID) (computer.ComputerAllocation, error) {
	if env == uuid.Nil() || checkpoint == uuid.Nil() || epoch <= 0 || host == uuid.Nil() {
		return computer.ComputerAllocation{}, ErrInvalidInput
	}
	var computerID, save uuid.UUID
	if err := a.database.QueryRow(ctx, `SELECT computer_id,disk_save_id FROM checkpoints WHERE environment_id=$1 AND id=$2`, env, checkpoint).Scan(&computerID, &save); err != nil {
		return computer.ComputerAllocation{}, allocationError(err)
	}
	if r, err := ReadLeaseAllocation(ctx, a.database, env, computerID, epoch); err == nil {
		if r.DiskOnlyRecovery || r.RestoredFrom == nil || *r.RestoredFrom != save {
			return computer.ComputerAllocation{}, ErrConflict
		}
		return r, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return computer.ComputerAllocation{}, err
	}
	var result computer.ComputerAllocation
	err := db.RunTx(ctx, a.database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		supply, err := lockAllocationSupply(ctx, tx, env, host, false)
		if err != nil {
			return err
		}
		_, err = org.LockAdmission(ctx, tx, env)
		if err != nil {
			return err
		}
		before, err := workergroup.ReadExecutionPlacement(ctx, tx, env)
		if err != nil {
			return err
		}
		capture, err := session.LockCapture(ctx, tx, env, checkpoint)
		if err != nil {
			return err
		}
		if r, err := ReadLeaseAllocation(ctx, tx, env, computerID, epoch); err == nil {
			if r.DiskOnlyRecovery || r.RestoredFrom == nil || *r.RestoredFrom != save {
				return ErrConflict
			}
			result = r
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if (capture.State != "ready" && capture.State != "restoring") || epoch <= capture.SourceEpoch {
			return ErrNotReady
		}
		eligible, err := computer.CheckpointDiskEligible(ctx, tx, env, computerID, save)
		if err != nil {
			return err
		}
		if !eligible {
			return ErrNotReady
		}
		var noWriter bool
		if err := tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND fenced_at IS NULL)
 AND (SELECT COALESCE(max(epoch),0) FROM computer_leases WHERE environment_id=$1 AND computer_id=$2)=$3::bigint-1`, env, computerID, epoch).Scan(&noWriter); err != nil {
			return err
		}
		if !noWriter {
			return ErrNotReady
		}
		source, err := ReadLeaseAllocation(ctx, tx, env, computerID, capture.SourceEpoch)
		if err != nil {
			return err
		}
		var sourceGroup uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT worker_group_id FROM worker_hosts WHERE id=$1`, source.HostID).Scan(&sourceGroup); err != nil {
			return err
		}
		if sourceGroup != supply.group {
			return ErrNotReady
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT manifest FROM checkpoints WHERE environment_id=$1 AND id=$2`, env, checkpoint).Scan(&raw); err != nil {
			return err
		}
		var manifest computercheckpoint.Manifest
		if json.Unmarshal(raw, &manifest) != nil {
			return ErrNotReady
		}
		if err := computer.ValidateCheckpointRuntime(ctx, tx, workergroup.HostPrincipal{HostID: host}, manifest.Runtime); err != nil {
			if errors.Is(err, computer.ErrConflict) {
				return ErrNotReady
			}
			return err
		}
		shape := source.Shape
		if shape.VMPlatformID != supply.shape.VMPlatformID || shape.VCPUCount != int64(manifest.Runtime.VMVCPUCount) || shape.CPUConfigDigest != manifest.Runtime.CPUConfigDigest || shape.MemoryBytes > supply.maxMemory || shape.CPUMillis > supply.maxCPU || shape.ScratchBytes > supply.shape.ScratchBytes {
			return ErrNotReady
		}
		if err := requireAllocationCapacity(ctx, tx, env, supply, shape, computerID, uuid.Nil()); err != nil {
			return err
		}
		result = computer.ComputerAllocation{EnvironmentID: env, OwnerID: computerID, InstanceID: uuid.NewV7(), Epoch: epoch, HostID: host, HostEpoch: supply.epoch, Shape: shape, Status: "acquiring", BaseVersion: save.String(), RestoredFrom: &save}
		if err := a.insertComputerAllocation(ctx, tx, &result); err != nil {
			return err
		}
		if err := requireAllocationFreshSupply(ctx, tx, supply); err != nil {
			return err
		}
		if err := computer.RequireSecretsNotRevoked(ctx, tx, env, computerID); err != nil {
			return err
		}
		return workergroup.RequireExecutionPlacement(ctx, tx, env, uuid.Nil(), before)
	})
	if err != nil {
		return computer.ComputerAllocation{}, allocationError(err)
	}
	return result, nil
}

// Independent command demand may still require a VM after Session revocation.
func initialSessionDemand(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env, computerID uuid.UUID, sessions []uuid.UUID) (bool, error) {
	allowed, err := session.AuthorizeInitialSessions(ctx, auditPolicy, tx, env, sessions)
	if err != nil || allowed {
		return allowed, err
	}
	var pending bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM commands WHERE environment_id=$1 AND computer_id=$2 AND status='pending')`, env, computerID).Scan(&pending)
	return pending, err
}
