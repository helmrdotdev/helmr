package workergroup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/helmrdotdev/helmr/internal/workerpoolname"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Enrollment is a worker host's request to join the worker group whose
// enrollment token hashes to TokenHash, in the pool named PoolName. A nil
// TokenHash is an enrollment token that could not be parsed.
type Enrollment struct {
	TokenHash  []byte
	PoolName   string
	ResourceID string
}

// EnrolledHost is a new worker host with its raw host secret, which is
// returned only once.
type EnrolledHost struct {
	HostID  uuid.UUID
	GroupID uuid.UUID
	PoolID  uuid.UUID
	Secret  string
}

// EnrollHost creates a registering worker host with a new host secret in the
// named pool of the worker group that the enrollment token authorizes,
// creating the pool when it does not exist, in one statement.
func EnrollHost(ctx context.Context, q db.Querier, cfg HostAuthConfig, enrollment Enrollment) (EnrolledHost, error) {
	if enrollment.ResourceID == "" || strings.TrimSpace(enrollment.ResourceID) != enrollment.ResourceID || len(enrollment.ResourceID) > MaxResourceIDBytes {
		return EnrolledHost{}, invalidInput("resource_id is required and must not exceed %d bytes", MaxResourceIDBytes)
	}
	if err := workerpoolname.Validate(enrollment.PoolName); err != nil {
		return EnrolledHost{}, invalidInput("worker pool name: %v", err)
	}
	if len(enrollment.TokenHash) == 0 {
		return EnrolledHost{}, ErrInvalidEnrollmentToken
	}
	generated, err := generateHostSecret(cfg.hostSecretKey)
	if err != nil {
		return EnrolledHost{}, fmt.Errorf("generate worker host secret: %w", err)
	}
	hostSecret, err := q.EnrollWorkerHost(ctx, db.EnrollWorkerHostParams{
		TokenHash:        enrollment.TokenHash,
		WorkerPoolID:     pgvalue.UUID(uuid.NewV7()),
		PoolName:         enrollment.PoolName,
		WorkerHostID:     pgvalue.UUID(uuid.NewV7()),
		CurrentServiceID: pgvalue.UUID(uuid.NewV7()),
		ResourceID:       enrollment.ResourceID,
		HostSecretID:     pgvalue.UUID(uuid.NewV7()),
		KeyPrefix:        generated.KeyPrefix,
		SecretHash:       generated.SecretHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return EnrolledHost{}, ErrInvalidEnrollmentToken
	}
	if err != nil {
		return EnrolledHost{}, fmt.Errorf("enroll worker host %q: %w", enrollment.ResourceID, err)
	}
	return EnrolledHost{
		HostID:  pgvalue.MustUUIDValue(hostSecret.WorkerHostID),
		GroupID: pgvalue.MustUUIDValue(hostSecret.WorkerGroupID),
		PoolID:  pgvalue.MustUUIDValue(hostSecret.WorkerPoolID),
		Secret:  generated.Raw,
	}, nil
}

// Activation is the capacity a worker host reports for its epoch: the pool
// template it must match or seal, and the CPU environment evidence with its
// digest.
type Activation struct {
	Template             Template
	CPUEnvironment       []byte
	CPUEnvironmentDigest string
}

// ActivateHost activates the authenticated epoch of a worker host. It locks
// the group, then the host's pool, then the host. A pending pool is sealed
// with the reported template and CPU shapes and becomes the group's initial
// primary pool; an active or draining pool must match the template exactly.
func ActivateHost(ctx context.Context, txb db.TxBeginner, principal HostPrincipal, activation Activation) error {
	template := activation.Template
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		group, err := q.LockWorkerGroupForPoolMutation(ctx, pgvalue.UUID(principal.GroupID))
		if err != nil {
			return err
		}
		if group.Status != db.WorkerGroupStatusActive && group.Status != db.WorkerGroupStatusPaused && group.Status != db.WorkerGroupStatusDraining {
			return pgx.ErrNoRows
		}
		epoch := pgtype.Int8{Int64: principal.Epoch, Valid: true}
		poolID, err := q.GetWorkerHostPoolID(ctx, db.GetWorkerHostPoolIDParams{
			WorkerHostID: pgvalue.UUID(principal.HostID), WorkerGroupID: pgvalue.UUID(principal.GroupID),
			WorkerEpoch: epoch,
		})
		if err != nil {
			return err
		}
		pool, err := q.LockWorkerPool(ctx, db.LockWorkerPoolParams{
			WorkerGroupID: pgvalue.UUID(principal.GroupID), WorkerPoolID: poolID,
		})
		if err != nil {
			return err
		}
		if _, err := q.LockWorkerHostForActivation(ctx, db.LockWorkerHostForActivationParams{
			WorkerHostID: pgvalue.UUID(principal.HostID), WorkerGroupID: pgvalue.UUID(principal.GroupID),
			WorkerPoolID: poolID, WorkerEpoch: epoch,
		}); err != nil {
			return err
		}
		if _, err := q.UpsertVMPlatform(ctx, vmPlatformParams(template.Runtime)); err != nil {
			return err
		}
		switch pool.Status {
		case "pending":
			if group.Status == db.WorkerGroupStatusDraining {
				return pgx.ErrNoRows
			}
			for _, shape := range template.CPUShapes {
				inserted, err := q.InsertWorkerPoolCPUShape(ctx, db.InsertWorkerPoolCPUShapeParams{
					VCPUCount: shape.VCPUCount, CPUConfigDigest: shape.CPUConfigDigest, WorkerPoolID: poolID,
				})
				if err != nil {
					return err
				}
				if inserted != 1 {
					return pgx.ErrNoRows
				}
			}
			pool, err = q.SealWorkerPool(ctx, sealPoolParams(principal.GroupID, poolID, template))
			if err != nil {
				return err
			}
			if _, err := q.SetInitialWorkerGroupPrimaryPool(ctx, db.SetInitialWorkerGroupPrimaryPoolParams{
				WorkerGroupID: pgvalue.UUID(principal.GroupID), WorkerPoolID: poolID,
			}); err != nil {
				return err
			}
		case "active", "draining":
			shapes, err := q.ListWorkerPoolCPUShapes(ctx, poolID)
			if err != nil {
				return err
			}
			if !poolMatches(pool, shapes, template) {
				return pgx.ErrNoRows
			}
		default:
			return pgx.ErrNoRows
		}
		_, err = q.ActivateWorkerHost(ctx, activationParams(principal, activation))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return conflict("worker activation is stale")
	}
	if err != nil {
		return fmt.Errorf("activate worker host %s: %w", principal.HostID, err)
	}
	return nil
}

func activationParams(principal HostPrincipal, activation Activation) db.ActivateWorkerHostParams {
	template := activation.Template
	return db.ActivateWorkerHostParams{
		VMPlatformID:   pgtype.Text{String: template.Runtime.ID, Valid: true},
		EpochCPUMillis: template.Capacity.CPUMillis, EpochMemoryBytes: template.Capacity.MemoryBytes,
		EpochGuestEphemeralDiskBytes: template.Capacity.GuestEphemeralDiskBytes,
		PerVMCPUMillis:               template.PerVM.CPUMillis, PerVMMemoryBytes: template.PerVM.MemoryBytes,
		PerVMGuestEphemeralDiskBytes: template.PerVM.GuestEphemeralDiskBytes,
		MaxVMSlots:                   int32(template.Capacity.VMSlots),
		MaxVMStarts:                  int32(template.Capacity.VMSlots),
		CPUEnvironment:               activation.CPUEnvironment, CPUEnvironmentDigest: pgtype.Text{String: activation.CPUEnvironmentDigest, Valid: true},
		WorkerHostID: pgvalue.UUID(principal.HostID), WorkerGroupID: pgvalue.UUID(principal.GroupID),
		WorkerEpoch: pgtype.Int8{Int64: principal.Epoch, Valid: true},
	}
}

func vmPlatformParams(profile vmplatform.Profile) db.UpsertVMPlatformParams {
	digest := pgtype.Text{String: profile.CPUTemplate.Digest, Valid: profile.CPUTemplate.Digest != ""}
	return db.UpsertVMPlatformParams{
		ID: profile.ID, Arch: profile.Arch, Contract: profile.Contract,
		DescriptorDigest:  profile.VMRuntimeDescriptorDigest,
		FirecrackerDigest: profile.FirecrackerDigest, FirecrackerVersion: profile.FirecrackerVersion,
		SnapshotFormatVersion: profile.SnapshotFormatVersion, HostKernelRelease: profile.HostKernelRelease,
		CPUTemplateKind: string(profile.CPUTemplate.Kind), CPUTemplateDigest: digest,
		KernelDigest: profile.KernelDigest, InitramfsDigest: profile.InitramfsDigest, RootfsDigest: profile.RootfsDigest,
	}
}

func sealPoolParams(groupID uuid.UUID, poolID pgtype.UUID, template Template) db.SealWorkerPoolParams {
	return db.SealWorkerPoolParams{
		VMPlatformID:                    pgtype.Text{String: template.Runtime.ID, Valid: true},
		CapacityCPUMillis:               pgtype.Int8{Int64: template.Capacity.CPUMillis, Valid: true},
		CapacityMemoryBytes:             pgtype.Int8{Int64: template.Capacity.MemoryBytes, Valid: true},
		CapacityGuestEphemeralDiskBytes: pgtype.Int8{Int64: template.Capacity.GuestEphemeralDiskBytes, Valid: true},
		PerVMCPUMillis:                  pgtype.Int8{Int64: template.PerVM.CPUMillis, Valid: true},
		PerVMMemoryBytes:                pgtype.Int8{Int64: template.PerVM.MemoryBytes, Valid: true},
		PerVMGuestEphemeralDiskBytes:    pgtype.Int8{Int64: template.PerVM.GuestEphemeralDiskBytes, Valid: true},
		MaxVMSlots:                      pgtype.Int4{Int32: int32(template.Capacity.VMSlots), Valid: true},
		WorkerPoolID:                    poolID, WorkerGroupID: pgvalue.UUID(groupID),
	}
}

func poolMatches(pool db.WorkerPool, shapes []db.WorkerPoolCpuShape, template Template) bool {
	if !pool.SealedAt.Valid ||
		!pool.VMPlatformID.Valid || pool.VMPlatformID.String != template.Runtime.ID ||
		!pool.CapacityCPUMillis.Valid || pool.CapacityCPUMillis.Int64 != template.Capacity.CPUMillis ||
		!pool.CapacityMemoryBytes.Valid || pool.CapacityMemoryBytes.Int64 != template.Capacity.MemoryBytes ||
		!pool.CapacityGuestEphemeralDiskBytes.Valid || pool.CapacityGuestEphemeralDiskBytes.Int64 != template.Capacity.GuestEphemeralDiskBytes ||
		!pool.PerVMCPUMillis.Valid || pool.PerVMCPUMillis.Int64 != template.PerVM.CPUMillis ||
		!pool.PerVMMemoryBytes.Valid || pool.PerVMMemoryBytes.Int64 != template.PerVM.MemoryBytes ||
		!pool.PerVMGuestEphemeralDiskBytes.Valid || pool.PerVMGuestEphemeralDiskBytes.Int64 != template.PerVM.GuestEphemeralDiskBytes ||
		!pool.MaxVMSlots.Valid || int64(pool.MaxVMSlots.Int32) != template.Capacity.VMSlots ||
		len(shapes) != len(template.CPUShapes) {
		return false
	}
	for index := range shapes {
		if shapes[index].VCPUCount != template.CPUShapes[index].VCPUCount ||
			shapes[index].CPUConfigDigest != template.CPUShapes[index].CPUConfigDigest {
			return false
		}
	}
	return true
}

// RecordStartupRecovery records the startup recovery evidence of the
// authenticated epoch, which returns the recovering host to service.
func RecordStartupRecovery(ctx context.Context, q db.Querier, principal HostPrincipal, evidence []byte) error {
	_, err := q.CompleteWorkerStartupRecovery(ctx, db.CompleteWorkerStartupRecoveryParams{
		WorkerHostID: pgvalue.UUID(principal.HostID), WorkerGroupID: pgvalue.UUID(principal.GroupID),
		WorkerEpoch: pgtype.Int8{Int64: principal.Epoch, Valid: true}, RecoveryEvidence: evidence,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return conflict("worker startup recovery fence is stale")
	}
	if err != nil {
		return fmt.Errorf("record worker host %s startup recovery: %w", principal.HostID, err)
	}
	return nil
}

// HostObservation is a worker host's report of why it pauses run or VM
// work; an empty reason reports that the role is not paused.
type HostObservation struct {
	RunPausedReason string
	VMPausedReason  string
}

// RecordObservation records an observation of the authenticated epoch.
func RecordObservation(ctx context.Context, q db.Querier, principal HostPrincipal, observation HostObservation) error {
	_, err := q.RecordWorkerObservation(ctx, observationParams(principal, observation))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrObservationConflict
	}
	if err != nil {
		return fmt.Errorf("record worker host %s observation: %w", principal.HostID, err)
	}
	return nil
}

func observationParams(principal HostPrincipal, observation HostObservation) db.RecordWorkerObservationParams {
	return db.RecordWorkerObservationParams{
		RunPausedReason: pgtype.Text{String: observation.RunPausedReason, Valid: observation.RunPausedReason != ""},
		VMPausedReason:  pgtype.Text{String: observation.VMPausedReason, Valid: observation.VMPausedReason != ""},
		WorkerHostID:    pgvalue.UUID(principal.HostID), WorkerGroupID: pgvalue.UUID(principal.GroupID),
		WorkerEpoch: pgtype.Int8{Int64: principal.Epoch, Valid: true},
	}
}

// BeginHostDrain starts the worker-requested drain of the authenticated
// epoch, fenced by its host claim version. Draining again is a replay.
func BeginHostDrain(ctx context.Context, q db.Querier, principal HostPrincipal) error {
	_, err := q.DrainWorkerHost(ctx, db.DrainWorkerHostParams{
		ID:                   pgvalue.UUID(principal.HostID),
		WorkerGroupID:        pgvalue.UUID(principal.GroupID),
		ExpectedEpoch:        pgtype.Int8{Int64: principal.Epoch, Valid: true},
		ExpectedClaimVersion: principal.HostClaimVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrHostNotFound
	}
	if err != nil {
		return fmt.Errorf("drain worker host %s: %w", principal.HostID, err)
	}
	return nil
}

// CompleteHostDrain marks a drained epoch termination ready once its instance
// state is gone, fenced by its host claim version. It locks the host's drain
// completion authority before completing the drain.
func CompleteHostDrain(ctx context.Context, txb db.TxBeginner, principal HostPrincipal, observedAt time.Time) (db.CompleteWorkerDrainRow, error) {
	params := db.CompleteWorkerDrainParams{
		WorkerHostID:         pgvalue.UUID(principal.HostID),
		WorkerGroupID:        pgvalue.UUID(principal.GroupID),
		WorkerEpoch:          pgtype.Int8{Int64: principal.Epoch, Valid: true},
		ExpectedClaimVersion: principal.HostClaimVersion,
		ObservedAt:           pgvalue.Timestamptz(observedAt),
	}
	var completed db.CompleteWorkerDrainRow
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := q.LockWorkerDrainCompletion(ctx, db.LockWorkerDrainCompletionParams{
			WorkerHostID:  params.WorkerHostID,
			WorkerGroupID: params.WorkerGroupID,
			WorkerEpoch:   params.WorkerEpoch,
		}); err != nil {
			return err
		}
		var err error
		completed, err = q.CompleteWorkerDrain(ctx, params)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.CompleteWorkerDrainRow{}, conflict("worker drain is not complete or its claim fence is stale")
	}
	if err != nil {
		return db.CompleteWorkerDrainRow{}, fmt.Errorf("complete worker host %s drain: %w", principal.HostID, err)
	}
	if completed.Status != db.WorkerHostStatusTerminationReady {
		return db.CompleteWorkerDrainRow{}, errors.New("complete worker drain returned a non-terminal worker state")
	}
	return completed, nil
}

// FenceHost fences the authenticated epoch for a worker-reported reason,
// fenced by its host claim version. Only provider termination and a
// retired worker are control inputs; diagnostic codes are rejected.
func FenceHost(ctx context.Context, q db.Querier, principal HostPrincipal, reasonCode string) error {
	reasonCode = strings.TrimSpace(reasonCode)
	if reasonCode != "provider_termination" && reasonCode != "worker_retired" {
		return invalidInput("unsupported worker fence reason")
	}
	_, err := q.FenceWorkerHost(ctx, db.FenceWorkerHostParams{
		ID:                   pgvalue.UUID(principal.HostID),
		WorkerGroupID:        pgvalue.UUID(principal.GroupID),
		ExpectedEpoch:        pgtype.Int8{Int64: principal.Epoch, Valid: true},
		ExpectedClaimVersion: principal.HostClaimVersion,
		ReasonCode:           pgtype.Text{String: reasonCode, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrHostNotFound
	}
	if err != nil {
		return fmt.Errorf("fence worker host %s: %w", principal.HostID, err)
	}
	return nil
}

// ReadHost returns the authenticated worker host's status with the
// readiness evidence of its latest observation.
func ReadHost(ctx context.Context, q db.Querier, principal HostPrincipal) (db.GetWorkerHostStatusRow, error) {
	state, err := q.GetWorkerHostStatus(ctx, db.GetWorkerHostStatusParams{
		ID:                          pgvalue.UUID(principal.HostID),
		WorkerGroupID:               pgvalue.UUID(principal.GroupID),
		ObservationFreshnessSeconds: ObservationFreshnessSeconds,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.GetWorkerHostStatusRow{}, ErrHostNotFound
	}
	if err != nil {
		return db.GetWorkerHostStatusRow{}, fmt.Errorf("get worker host %s status: %w", principal.HostID, err)
	}
	return state, nil
}

// DrainInvalidEpoch drains a worker epoch that reported its runtime
// invalid. It runs in its own transaction after the failed Computer
// operation, locking the group, the host's pool and the host, and drains an
// active host; a host that is already draining is left unchanged.
func DrainInvalidEpoch(ctx context.Context, txb db.TxBeginner, groupID uuid.UUID, hostID uuid.UUID, epoch int64) error {
	return db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		poolID, err := q.GetWorkerHostPoolID(ctx, db.GetWorkerHostPoolIDParams{
			WorkerHostID:  pgvalue.UUID(hostID),
			WorkerGroupID: pgvalue.UUID(groupID),
			WorkerEpoch:   pgtype.Int8{Int64: epoch, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("resolve invalid worker epoch pool: %w", err)
		}
		group, err := q.LockWorkerGroupForPoolMutation(ctx, pgvalue.UUID(groupID))
		if err != nil {
			return fmt.Errorf("lock invalid worker epoch group: %w", err)
		}
		if group.Status != db.WorkerGroupStatusActive && group.Status != db.WorkerGroupStatusPaused &&
			group.Status != db.WorkerGroupStatusDraining {
			return errors.New("invalid worker epoch group is inactive")
		}
		pool, err := q.LockWorkerPool(ctx, db.LockWorkerPoolParams{
			WorkerGroupID: pgvalue.UUID(groupID),
			WorkerPoolID:  poolID,
		})
		if err != nil {
			return fmt.Errorf("lock invalid worker epoch pool: %w", err)
		}
		if pool.Status != "active" && pool.Status != "draining" {
			return errors.New("invalid worker epoch pool is inactive")
		}
		host, err := q.LockWorkerHostForActivation(ctx, db.LockWorkerHostForActivationParams{
			WorkerHostID:  pgvalue.UUID(hostID),
			WorkerGroupID: pgvalue.UUID(groupID),
			WorkerPoolID:  poolID,
			WorkerEpoch:   pgtype.Int8{Int64: epoch, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("lock invalid worker epoch: %w", err)
		}
		if host.Status != db.WorkerHostStatusActive && host.Status != db.WorkerHostStatusDraining {
			return errors.New("invalid worker epoch is inactive")
		}
		if host.Status == db.WorkerHostStatusActive {
			if _, err := q.DrainWorkerHost(ctx, db.DrainWorkerHostParams{
				ID:                   pgvalue.UUID(hostID),
				WorkerGroupID:        pgvalue.UUID(groupID),
				ExpectedEpoch:        pgtype.Int8{Int64: epoch, Valid: true},
				ExpectedClaimVersion: host.ClaimVersion,
			}); err != nil {
				return fmt.Errorf("fence invalid worker epoch: %w", err)
			}
		}
		return nil
	})
}
