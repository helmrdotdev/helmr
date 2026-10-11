package agent

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"math"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// AllocationShape is the immutable physical reservation and materialization
// profile selected from registered resources and qualified worker supply.
type AllocationShape struct {
	CPUMillis       int64
	MemoryBytes     int64
	ScratchBytes    int64
	VMPlatformID    string
	VCPUCount       int64
	CPUConfigDigest string
}

// Allocation is a durable placement receipt, not permission to start a VM.
// Only delivery to its authenticated Host can issue the channel credential.
type Allocation struct {
	EnvironmentID uuid.UUID
	OwnerID       uuid.UUID
	InstanceID    uuid.UUID
	Epoch         int64
	HostID        uuid.UUID
	HostEpoch     int64
	Shape         AllocationShape
	Status        string
	DeliveredAt   *time.Time
	ExpiresAt     *time.Time
	FencedAt      *time.Time
}

type ComputerAllocation struct {
	Allocation
	BaseVersion   string
	RestoredFrom  *uuid.UUID
	InitializedAt *time.Time
}

// Allocator is a trusted control-plane owner. Workers cannot choose its targets,
// instance identities, resource amounts or epochs. Restart recovery requires the
// same configured channel key; a mismatching key never replaces a stored digest.
type PreparationTrustIssuer interface {
	GeneratePreparationProxyTrust(environmentID, preparationID uuid.UUID, createdAt, deadline time.Time) (secret.ProxyTrust, error)
}

type Allocator struct {
	database db.TxDB
	key      [32]byte
	trust    PreparationTrustIssuer
}

func NewAllocator(database db.TxDB, channelKey []byte, trust PreparationTrustIssuer) (*Allocator, error) {
	if database == nil || len(channelKey) != 32 {
		return nil, ErrInvalidInput
	}
	a := &Allocator{database: database, trust: trust}
	copy(a.key[:], channelKey)
	return a, nil
}

func (a *Allocator) credential(domain string, receipt Allocation) []byte {
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

func (a *Allocator) preparationCredential(receipt Allocation) []byte {
	return a.credential("helmr.preparation-channel.v1\x00", receipt)
}

func (a *Allocator) computerCredential(receipt Allocation) string {
	return base64.RawURLEncoding.EncodeToString(a.credential("helmr.computer-channel.v1\x00", receipt))
}

var errEnvironmentCapacity = fmt.Errorf("%w: environment allocation capacity", ErrNotReady)

type allocationCapacity struct {
	cpu, memory, scratch, slots int64
}

type allocationSupply struct {
	group, host uuid.UUID
	epoch       int64
	capacity    allocationCapacity
	maxMemory   int64
	maxCPU      int64
	shape       AllocationShape
}

// Every wait is bounded, including implicit FK/unique waits after Host UPDATE.
// A failed candidate rolls back before the caller tries another Host.
func allocationLockTimeout(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SET LOCAL lock_timeout='100ms'`)
	return err
}

func allocationError(err error) error {
	var lock *pgconn.PgError
	if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &lock) && lock.Code == "55P03") {
		return ErrNotReady
	}
	return err
}

func lockAllocationSupply(ctx context.Context, tx pgx.Tx, env, host uuid.UUID, primary bool) (allocationSupply, error) {
	s := allocationSupply{host: host}
	var region string
	if err := tx.QueryRow(ctx, `SELECT p.default_region_id FROM environments e JOIN projects p ON p.id=e.project_id WHERE e.id=$1 AND e.retired_at IS NULL`, env).Scan(&region); err != nil {
		return s, err
	}
	if err := tx.QueryRow(ctx, `SELECT worker_group_id,current_epoch FROM worker_hosts WHERE id=$1 AND current_epoch IS NOT NULL`, host).Scan(&s.group, &s.epoch); err != nil {
		return s, err
	}
	admitting, err := workergroup.LockDispatchSupply(ctx, tx, workergroup.DispatchSupply{
		GroupID: pgvalue.UUID(s.group), HostID: pgvalue.UUID(host), Epoch: s.epoch,
		RegionID: region, RunArchitecture: string(definition.ArchitectureX8664), RequirePrimary: primary,
	})
	if err != nil {
		return s, err
	}
	if !admitting {
		return s, ErrNotReady
	}
	err = tx.QueryRow(ctx, `SELECT epoch_cpu_millis,epoch_memory_bytes,epoch_guest_ephemeral_disk_bytes,max_vm_slots,
 per_vm_memory_bytes,per_vm_guest_ephemeral_disk_bytes,vm_platform_id,per_vm_cpu_millis FROM worker_hosts WHERE id=$1`, host).Scan(
		&s.capacity.cpu, &s.capacity.memory, &s.capacity.scratch, &s.capacity.slots, &s.maxMemory, &s.shape.ScratchBytes, &s.shape.VMPlatformID, &s.maxCPU)
	return s, err
}

func qualifyAllocationShape(ctx context.Context, tx pgx.Tx, supply allocationSupply, raw []byte) (AllocationShape, error) {
	shape := supply.shape
	var declared definition.ResourcesManifest
	if json.Unmarshal(raw, &declared) != nil || definition.ValidateResourcesManifest(declared) != nil || declared.MemoryMiB > math.MaxInt64/(1<<20) {
		return shape, ErrInvalidInput
	}
	var err error
	shape.CPUMillis, err = vm.ReservedCPUMillis(declared.MilliCPU)
	if err != nil {
		return shape, ErrInvalidInput
	}
	shape.MemoryBytes = declared.MemoryMiB * (1 << 20)
	shape.VCPUCount = shape.CPUMillis / 1000
	if shape.MemoryBytes > supply.maxMemory || shape.CPUMillis > supply.maxCPU || shape.ScratchBytes <= 0 {
		return shape, ErrNotReady
	}
	// The sealed pool enumerates every physically qualified whole-vCPU shape.
	err = tx.QueryRow(ctx, `SELECT s.cpu_config_digest FROM worker_pool_cpu_shapes s JOIN worker_hosts h ON h.worker_pool_id=s.worker_pool_id
 WHERE h.id=$1 AND s.vcpu_count=$2`, supply.host, shape.VCPUCount).Scan(&shape.CPUConfigDigest)
	return shape, err
}

func requireAllocationCapacity(ctx context.Context, tx pgx.Tx, env uuid.UUID, supply allocationSupply, policy admissionEnvironment, shape AllocationShape, computer bool) error {
	if !policy.configured {
		return errEnvironmentCapacity
	}
	var environment, host allocationCapacity
	var computers int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(cpu),0),COALESCE(sum(memory),0),COALESCE(sum(computers),0) FROM (
 SELECT reserved_cpu_millis cpu,reserved_memory_bytes memory,1::bigint computers FROM computer_leases WHERE environment_id=$1 AND fenced_at IS NULL
 UNION ALL SELECT reserved_cpu_millis,reserved_memory_bytes,0 FROM computer_preparations WHERE environment_id=$1 AND worker_host_id IS NOT NULL AND fenced_at IS NULL) usage`, env).Scan(&environment.cpu, &environment.memory, &computers); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(cpu),0),COALESCE(sum(memory),0),COALESCE(sum(scratch),0),count(*) FROM (
 SELECT reserved_cpu_millis cpu,reserved_memory_bytes memory,reserved_scratch_bytes scratch FROM computer_leases WHERE worker_host_id=$1 AND fenced_at IS NULL
 UNION ALL SELECT reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes FROM computer_preparations WHERE worker_host_id=$1 AND fenced_at IS NULL) usage`, supply.host).Scan(&host.cpu, &host.memory, &host.scratch, &host.slots); err != nil {
		return err
	}
	fits := func(used, added, limit int64) bool { return added > 0 && added <= limit && used <= limit-added }
	if !fits(environment.cpu, shape.CPUMillis, policy.maxCPU) || !fits(environment.memory, shape.MemoryBytes, policy.maxMemory) ||
		(computer && !fits(computers, 1, policy.maxResident)) {
		return errEnvironmentCapacity
	}
	if !fits(host.cpu, shape.CPUMillis, supply.capacity.cpu) ||
		!fits(host.memory, shape.MemoryBytes, supply.capacity.memory) || !fits(host.scratch, shape.ScratchBytes, supply.capacity.scratch) ||
		!fits(host.slots, 1, supply.capacity.slots) {
		return ErrNotReady
	}
	return nil
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

func readPreparationAllocation(ctx context.Context, q db.DBTX, env, preparation uuid.UUID) (Allocation, error) {
	r := Allocation{EnvironmentID: env, OwnerID: preparation}
	err := q.QueryRow(ctx, `SELECT instance_id,executor_epoch,worker_host_id,worker_epoch,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,
 vm_platform_id,vm_vcpu_count,cpu_config_digest,status,delivered_at,executor_expires_at,fenced_at FROM computer_preparations
 WHERE environment_id=$1 AND id=$2 AND worker_host_id IS NOT NULL`, env, preparation).Scan(&r.InstanceID, &r.Epoch, &r.HostID, &r.HostEpoch,
		&r.Shape.CPUMillis, &r.Shape.MemoryBytes, &r.Shape.ScratchBytes, &r.Shape.VMPlatformID, &r.Shape.VCPUCount, &r.Shape.CPUConfigDigest,
		&r.Status, &r.DeliveredAt, &r.ExpiresAt, &r.FencedAt)
	return r, err
}

func readComputerAllocation(ctx context.Context, q db.DBTX, env, computer uuid.UUID, epoch int64) (ComputerAllocation, error) {
	r := ComputerAllocation{Allocation: Allocation{EnvironmentID: env, OwnerID: computer, Epoch: epoch}}
	err := q.QueryRow(ctx, `SELECT l.computer_instance_id,l.worker_host_id,l.worker_epoch,l.reserved_cpu_millis,l.reserved_memory_bytes,l.reserved_scratch_bytes,
 l.vm_platform_id,l.vm_vcpu_count,l.cpu_config_digest,l.status,l.delivered_at,l.expires_at,l.fenced_at,COALESCE(l.restored_from_save_id::text,'sha256:'||encode(c.initial_root_digest,'hex')),l.restored_from_save_id,l.initialized_at
 FROM computer_leases l JOIN computers c ON (c.environment_id,c.id)=(l.environment_id,l.computer_id) WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3`, env, computer, epoch).Scan(&r.InstanceID, &r.HostID, &r.HostEpoch,
		&r.Shape.CPUMillis, &r.Shape.MemoryBytes, &r.Shape.ScratchBytes, &r.Shape.VMPlatformID, &r.Shape.VCPUCount, &r.Shape.CPUConfigDigest,
		&r.Status, &r.DeliveredAt, &r.ExpiresAt, &r.FencedAt, &r.BaseVersion, &r.RestoredFrom, &r.InitializedAt)
	return r, err
}

// AllocatePreparation reserves one private executor atomically. Its admitted
// deadline is unchanged. A historical allocation is returned before considering
// a new candidate, even if that allocation has since expired or been fenced.
func (a *Allocator) AllocatePreparation(ctx context.Context, env, preparation, host uuid.UUID) (Allocation, error) {
	if env == uuid.Nil() || preparation == uuid.Nil() || host == uuid.Nil() {
		return Allocation{}, ErrInvalidInput
	}
	if prior, err := readPreparationAllocation(ctx, a.database, env, preparation); err == nil {
		return prior, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Allocation{}, err
	}
	var result Allocation
	err := db.RunTx(ctx, a.database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		supply, err := lockAllocationSupply(ctx, tx, env, host, true)
		if err != nil {
			return err
		}
		policy, err := lockAdmissionEnvironment(ctx, tx, env)
		if err != nil {
			return err
		}
		p, err := readPreparation(ctx, tx, env, preparation)
		if err != nil {
			return err
		}
		if err := lockPreparationImageSecrets(ctx, tx, env, p.SpecID); err != nil {
			return err
		}
		if err := lockPreparationSpec(ctx, tx, env, p.SpecID); err != nil {
			return err
		}
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM computer_preparations WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, preparation).Scan(&locked); err != nil {
			return err
		}
		if prior, err := readPreparationAllocation(ctx, tx, env, preparation); err == nil {
			result = prior
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var resources []byte
		if err := tx.QueryRow(ctx, `SELECT resources FROM computer_definitions WHERE environment_id=$1 AND preparation_spec_id=$2 ORDER BY deployment_id,definition_key LIMIT 1`, env, p.SpecID).Scan(&resources); err != nil {
			return err
		}
		shape, err := qualifyAllocationShape(ctx, tx, supply, resources)
		if err != nil {
			return err
		}
		if err := requireAllocationCapacity(ctx, tx, env, supply, policy, shape, false); err != nil {
			return err
		}
		result = Allocation{EnvironmentID: env, OwnerID: preparation, InstanceID: uuid.NewV7(), Epoch: 1, HostID: host, HostEpoch: supply.epoch, Shape: shape, Status: "running"}
		digest := sha256.Sum256(a.preparationCredential(result))
		changed, err := tx.Exec(ctx, `UPDATE computer_preparations SET status='running',executor_epoch=1,worker_host_id=$3,worker_epoch=$4,instance_id=$5,channel_credential_digest=$6,
 reserved_cpu_millis=$7,reserved_memory_bytes=$8,reserved_scratch_bytes=$9,vm_platform_id=$10,vm_vcpu_count=$11,cpu_config_digest=$12
 WHERE environment_id=$1 AND id=$2 AND status='queued' AND deadline_at>clock_timestamp() AND NOT EXISTS(
 SELECT 1 FROM computer_secret_bindings binding JOIN secrets s ON s.environment_id=binding.environment_id AND s.id=binding.secret_id
 WHERE binding.environment_id=$1 AND binding.preparation_spec_id=$13 AND s.status='revoked')`, env, preparation, host, supply.epoch, result.InstanceID, digest[:], shape.CPUMillis, shape.MemoryBytes, shape.ScratchBytes, shape.VMPlatformID, shape.VCPUCount, shape.CPUConfigDigest, p.SpecID)
		if err != nil {
			return err
		}
		if changed.RowsAffected() != 1 {
			return ErrNotReady
		}
		var protected bool
		var now, deadline time.Time
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_secret_bindings b WHERE b.environment_id=p.environment_id AND b.preparation_spec_id=p.preparation_spec_id AND b.mode='protected'),clock_timestamp(),p.deadline_at FROM computer_preparations p WHERE p.environment_id=$1 AND p.id=$2`, env, preparation).Scan(&protected, &now, &deadline); err != nil {
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
			if _, err := tx.Exec(ctx, `UPDATE computer_preparations SET proxy_ca_certificate=$3,proxy_ca_private_key_nonce=$4,proxy_ca_private_key_ciphertext=$5,proxy_ca_not_after=$6 WHERE environment_id=$1 AND id=$2`, env, preparation, trust.Certificate, trust.PrivateKeyNonce, trust.PrivateKeyCiphertext, trust.NotAfter); err != nil {
				return err
			}
		}
		if err := requireAllocationFreshSupply(ctx, tx, supply); err != nil {
			return err
		}
		var current bool
		if err := tx.QueryRow(ctx, `SELECT deadline_at>clock_timestamp() FROM computer_preparations WHERE environment_id=$1 AND id=$2`, env, preparation).Scan(&current); err != nil {
			return err
		}
		if !current {
			return ErrNotReady
		}
		return nil
	})
	if err != nil {
		return Allocation{}, allocationError(err)
	}
	return result, nil
}

// AllocateFreshComputer is only for a first physical execution. The CP retains
// the requested epoch across retries; requesting the next epoch is a new attempt,
// permissible only after all previous VMs stopped without ever becoming ready.
func (a *Allocator) AllocateFreshComputer(ctx context.Context, env, computer uuid.UUID, epoch int64, host uuid.UUID) (ComputerAllocation, error) {
	if env == uuid.Nil() || computer == uuid.Nil() || epoch <= 0 || host == uuid.Nil() {
		return ComputerAllocation{}, ErrInvalidInput
	}
	if prior, err := readComputerAllocation(ctx, a.database, env, computer, epoch); err == nil {
		if prior.RestoredFrom != nil {
			return ComputerAllocation{}, ErrConflict
		}
		return prior, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ComputerAllocation{}, err
	}
	var result ComputerAllocation
	err := db.RunTx(ctx, a.database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		supply, err := lockAllocationSupply(ctx, tx, env, host, true)
		if err != nil {
			return err
		}
		policy, err := lockAdmissionEnvironment(ctx, tx, env)
		if err != nil {
			return err
		}
		sessions, err := lockInitialComputerSessions(ctx, tx, env, computer)
		if err != nil {
			return err
		}
		if prior, err := readComputerAllocation(ctx, tx, env, computer, epoch); err == nil {
			if prior.RestoredFrom != nil {
				return ErrConflict
			}
			result = prior
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var resources []byte
		var base string
		if err := tx.QueryRow(ctx, `SELECT resources,'sha256:'||encode(initial_root_digest,'hex') FROM computers c WHERE environment_id=$1 AND id=$2
 AND initial_root_id IS NOT NULL AND image_id IS NOT NULL AND preparation_failed_at IS NULL AND deleted_at IS NULL AND integrity_fault_at IS NULL AND recovery_save_id IS NULL
 AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=$1 AND l.computer_id=$2 AND (l.fenced_at IS NULL OR l.initialized_at IS NOT NULL))
 AND (SELECT COALESCE(max(l.epoch),0) FROM computer_leases l WHERE l.environment_id=$1 AND l.computer_id=$2)=$3::bigint-1`, env, computer, epoch).Scan(&resources, &base); err != nil {
			return err
		}
		shape, err := qualifyAllocationShape(ctx, tx, supply, resources)
		if err != nil {
			return err
		}
		if err := requireAllocationCapacity(ctx, tx, env, supply, policy, shape, true); err != nil {
			return err
		}
		result = ComputerAllocation{Allocation: Allocation{EnvironmentID: env, OwnerID: computer, InstanceID: uuid.NewV7(), Epoch: epoch, HostID: host, HostEpoch: supply.epoch, Shape: shape, Status: "acquiring"}, BaseVersion: base}
		if err := a.insertComputerAllocation(ctx, tx, result); err != nil {
			return err
		}
		if err := assignInitialComputerSessions(ctx, tx, result, sessions); err != nil {
			return err
		}
		if err := requireAllocationFreshSupply(ctx, tx, supply); err != nil {
			return err
		}
		return requireComputerImageAllowed(ctx, tx, env, computer)
	})
	if err != nil {
		return ComputerAllocation{}, allocationError(err)
	}
	return result, nil
}

func (a *Allocator) insertComputerAllocation(ctx context.Context, tx pgx.Tx, r ComputerAllocation) error {
	digest := sha256.Sum256([]byte(a.computerCredential(r.Allocation)))
	_, err := tx.Exec(ctx, `INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,computer_instance_id,channel_credential_digest,status,
 reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,restored_from_save_id)
 VALUES($1,$2,$3,$4,$5,$6,$7,'acquiring',$8,$9,$10,$11,$12,$13,$14)`, r.EnvironmentID, r.OwnerID, r.Epoch, r.HostID, r.HostEpoch, r.InstanceID, digest[:], r.Shape.CPUMillis, r.Shape.MemoryBytes, r.Shape.ScratchBytes, r.Shape.VMPlatformID, r.Shape.VCPUCount, r.Shape.CPUConfigDigest, r.RestoredFrom)
	return err
}

// AllocateRestoredComputer reserves the full physical shape before a Host may
// materialize a retained checkpoint. It never falls back to fresh boot or setup.
func (a *Allocator) AllocateRestoredComputer(ctx context.Context, env, checkpoint uuid.UUID, epoch int64, host uuid.UUID) (ComputerAllocation, error) {
	if env == uuid.Nil() || checkpoint == uuid.Nil() || epoch <= 0 || host == uuid.Nil() {
		return ComputerAllocation{}, ErrInvalidInput
	}
	var computer, save uuid.UUID
	if err := a.database.QueryRow(ctx, `SELECT computer_id,disk_save_id FROM computer_checkpoints WHERE environment_id=$1 AND id=$2`, env, checkpoint).Scan(&computer, &save); err != nil {
		return ComputerAllocation{}, allocationError(err)
	}
	if r, err := readComputerAllocation(ctx, a.database, env, computer, epoch); err == nil {
		if r.RestoredFrom == nil || *r.RestoredFrom != save {
			return ComputerAllocation{}, ErrConflict
		}
		return r, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ComputerAllocation{}, err
	}
	var result ComputerAllocation
	err := db.RunTx(ctx, a.database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		supply, err := lockAllocationSupply(ctx, tx, env, host, false)
		if err != nil {
			return err
		}
		policy, err := lockAdmissionEnvironment(ctx, tx, env)
		if err != nil {
			return err
		}
		capture, err := lockCapture(ctx, tx, env, checkpoint)
		if err != nil {
			return err
		}
		if r, err := readComputerAllocation(ctx, tx, env, computer, epoch); err == nil {
			if r.RestoredFrom == nil || *r.RestoredFrom != save {
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
		eligible, err := checkpointDiskEligible(ctx, tx, env, computer, save)
		if err != nil {
			return err
		}
		if !eligible {
			return ErrNotReady
		}
		var noWriter bool
		if err := tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND fenced_at IS NULL)
 AND (SELECT COALESCE(max(epoch),0) FROM computer_leases WHERE environment_id=$1 AND computer_id=$2)=$3::bigint-1`, env, computer, epoch).Scan(&noWriter); err != nil {
			return err
		}
		if !noWriter {
			return ErrNotReady
		}
		source, err := readComputerAllocation(ctx, tx, env, computer, capture.SourceEpoch)
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
		if err := tx.QueryRow(ctx, `SELECT manifest FROM computer_checkpoints WHERE environment_id=$1 AND id=$2`, env, checkpoint).Scan(&raw); err != nil {
			return err
		}
		var manifest computercheckpoint.Manifest
		if json.Unmarshal(raw, &manifest) != nil {
			return ErrNotReady
		}
		if err := validateCheckpointRuntime(ctx, tx, workergroup.HostPrincipal{HostID: host}, manifest.Runtime); err != nil {
			if errors.Is(err, ErrConflict) {
				return ErrNotReady
			}
			return err
		}
		shape := source.Shape
		if shape.VMPlatformID != supply.shape.VMPlatformID || shape.VCPUCount != int64(manifest.Runtime.VMVCPUCount) || shape.CPUConfigDigest != manifest.Runtime.CPUConfigDigest || shape.MemoryBytes > supply.maxMemory || shape.CPUMillis > supply.maxCPU || shape.ScratchBytes > supply.shape.ScratchBytes {
			return ErrNotReady
		}
		if err := requireAllocationCapacity(ctx, tx, env, supply, policy, shape, true); err != nil {
			return err
		}
		result = ComputerAllocation{Allocation: Allocation{EnvironmentID: env, OwnerID: computer, InstanceID: uuid.NewV7(), Epoch: epoch, HostID: host, HostEpoch: supply.epoch, Shape: shape, Status: "acquiring"}, BaseVersion: save.String(), RestoredFrom: &save}
		if err := a.insertComputerAllocation(ctx, tx, result); err != nil {
			return err
		}
		if err := requireAllocationFreshSupply(ctx, tx, supply); err != nil {
			return err
		}
		return requireComputerImageAllowed(ctx, tx, env, computer)
	})
	if err != nil {
		return ComputerAllocation{}, allocationError(err)
	}
	return result, nil
}
