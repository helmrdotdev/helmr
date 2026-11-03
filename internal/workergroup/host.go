package workergroup

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/helmrdotdev/helmr/internal/workerpoolname"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Enrollment is a worker host's request to join the worker group that its
// enrollment token admits, in the pool named PoolName, with the public half of
// the machine key it generated for itself.
type Enrollment struct {
	Token      auth.ParsedEnrollmentToken
	PoolName   string
	ResourceID string
	PublicKey  []byte
}

// EnrolledHost is a new worker host and the key it proves itself with.
type EnrolledHost struct {
	HostID  uuid.UUID
	GroupID uuid.UUID
	PoolID  uuid.UUID
	KeyID   uuid.UUID
}

// EnrollHost creates a registering worker host with its machine key in the
// named pool of the worker group that the enrollment token admits, creating
// the pool when it does not exist. A single-use token is consumed by this
// host and must name a resource ID no enrolled host holds. Repeating an
// enrollment with the same token, resource and key returns the same host.
// Authority is locked
// before checking existing allocations so a lock
// wait cannot hide a newly committed writer. This uses the database's READ
// COMMITTED isolation; allocation transactions must retain at least a shared
// group lock through their commit.
func EnrollHost(ctx context.Context, pool db.TxBeginner, policy auditpolicy.Policy, enrollment Enrollment) (EnrolledHost, error) {
	if enrollment.ResourceID == "" || strings.TrimSpace(enrollment.ResourceID) != enrollment.ResourceID || len(enrollment.ResourceID) > MaxResourceIDBytes {
		return EnrolledHost{}, invalidInput("resource_id is required and must not exceed %d bytes", MaxResourceIDBytes)
	}
	if err := workerpoolname.Validate(enrollment.PoolName); err != nil {
		return EnrolledHost{}, invalidInput("worker pool name: %v", err)
	}
	if len(enrollment.PublicKey) != ed25519.PublicKeySize {
		return EnrolledHost{}, invalidInput("public_key must be a %d-byte Ed25519 public key", ed25519.PublicKeySize)
	}
	if len(enrollment.Token.Hash) == 0 {
		return EnrolledHost{}, ErrInvalidEnrollmentToken
	}
	var hostKey db.EnrollWorkerHostRow
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var group, token uuid.UUID
		var consumedBy *uuid.UUID
		live := true
		var lookup pgx.Row
		if enrollment.Token.SingleUse {
			lookup = tx.QueryRow(ctx, `SELECT g.id, t.id, t.worker_host_id, t.expires_at > clock_timestamp() FROM worker_enrollment_tokens t JOIN worker_groups g ON g.id=t.worker_group_id
 WHERE t.token_hash=$1 AND g.status IN ('active','paused') FOR UPDATE OF t,g`, enrollment.Token.Hash)
		} else {
			lookup = tx.QueryRow(ctx, `SELECT g.id, t.id, NULL::uuid, true FROM worker_group_tokens t JOIN worker_groups g ON g.token_id=t.id
 WHERE t.token_hash=$1 AND g.status IN ('active','paused') FOR UPDATE OF t,g`, enrollment.Token.Hash)
		}
		if err := lookup.Scan(&group, &token, &consumedBy, &live); err != nil {
			return err
		}
		// Repeating an enrollment whose response was lost returns the same
		// host: the machine still holds the token and the key it enrolled.
		// A single-use token replays only for the host that consumed it, also
		// after it expired; it admits a new host only before then.
		var replayHost, replayPool, replayKey uuid.UUID
		err := tx.QueryRow(ctx, `SELECT h.id, h.worker_pool_id, k.id FROM worker_hosts h
 JOIN worker_host_keys k ON k.worker_host_id=h.id AND k.revoked_at IS NULL
 WHERE h.worker_group_id=$1 AND h.resource_id=$2 AND h.status IN ('registering','active','draining') AND k.public_key=$3`,
			group, enrollment.ResourceID, enrollment.PublicKey).Scan(&replayHost, &replayPool, &replayKey)
		switch {
		case err == nil && (!enrollment.Token.SingleUse || consumedBy != nil && *consumedBy == replayHost):
			hostKey = db.EnrollWorkerHostRow{ID: pgvalue.UUID(replayKey), WorkerHostID: pgvalue.UUID(replayHost), WorkerGroupID: pgvalue.UUID(group), WorkerPoolID: pgvalue.UUID(replayPool)}
			return nil
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("read enrollment replay: %w", err)
		}
		if consumedBy != nil || !live {
			return ErrInvalidEnrollmentToken
		}
		// Use a fresh statement snapshot after the group lock. An allocation
		// committed while enrollment waited must remain visible across every
		// incarnation of this resource, including lost host rows.
		var custody bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM worker_hosts h
 WHERE h.worker_group_id=$1 AND h.resource_id=$2 AND EXISTS(SELECT 1 FROM (`+db.AllocationUsageSQL+`) allocation_usage WHERE worker_host_id=h.id))`, group, enrollment.ResourceID).Scan(&custody); err != nil {
			return err
		}
		if custody {
			return ErrHostCustody
		}
		// A single-use token admits one new machine. Re-registering a host
		// that is already enrolled, which replaces its key, takes the group's
		// token.
		if enrollment.Token.SingleUse {
			var enrolled bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM worker_hosts WHERE worker_group_id=$1 AND resource_id=$2 AND status IN ('registering','active','draining'))`, group, enrollment.ResourceID).Scan(&enrolled); err != nil {
				return err
			}
			if enrolled {
				return ErrHostResourceEnrolled
			}
		}
		proposedPool := uuid.NewV7()
		hostKey, err = db.New(tx).EnrollWorkerHost(ctx, db.EnrollWorkerHostParams{
			WorkerGroupID:    pgvalue.UUID(group),
			WorkerPoolID:     pgvalue.UUID(proposedPool),
			PoolName:         enrollment.PoolName,
			WorkerHostID:     pgvalue.UUID(uuid.NewV7()),
			CurrentServiceID: pgvalue.UUID(uuid.NewV7()),
			ResourceID:       enrollment.ResourceID,
			HostKeyID:        pgvalue.UUID(uuid.NewV7()),
			PublicKey:        enrollment.PublicKey,
		})
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "worker_host_keys_public_key_key" {
			return ErrHostKeyRegistered
		}
		if err != nil {
			return err
		}
		if enrollment.Token.SingleUse {
			// Expiry is rechecked at consumption: the lookup's predicate is not
			// re-evaluated after a lock wait.
			consumed, err := tx.Exec(ctx, `UPDATE worker_enrollment_tokens SET consumed_at=clock_timestamp(),worker_host_id=$2
 WHERE id=$1 AND consumed_at IS NULL AND expires_at > clock_timestamp()`, token, hostKey.WorkerHostID)
			if err != nil {
				return fmt.Errorf("consume enrollment token: %w", err)
			}
			if consumed.RowsAffected() != 1 {
				return ErrInvalidEnrollmentToken
			}
		} else if _, err := tx.Exec(ctx, `UPDATE worker_group_tokens SET last_used_at=now() WHERE id=$1`, token); err != nil {
			return fmt.Errorf("record enrollment token use: %w", err)
		}
		if err := dbnotify.Notify(ctx, tx, "dispatch", "capacity"); err != nil {
			return err
		}
		if pgvalue.MustUUIDValue(hostKey.WorkerPoolID) == proposedPool {
			return audit.Append(ctx, tx, policy, audit.Event{Evidence: audit.Internal(audit.ControlPlane), Action: "worker_pool.created", TargetKind: "worker_pool", TargetID: proposedPool})
		}
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return EnrolledHost{}, ErrInvalidEnrollmentToken
	}
	if errors.Is(err, ErrHostCustody) || errors.Is(err, ErrHostKeyRegistered) || errors.Is(err, ErrHostResourceEnrolled) || errors.Is(err, ErrInvalidEnrollmentToken) {
		return EnrolledHost{}, err
	}
	if err != nil {
		return EnrolledHost{}, fmt.Errorf("enroll worker host %q: %w", enrollment.ResourceID, err)
	}
	return EnrolledHost{
		HostID:  pgvalue.MustUUIDValue(hostKey.WorkerHostID),
		GroupID: pgvalue.MustUUIDValue(hostKey.WorkerGroupID),
		PoolID:  pgvalue.MustUUIDValue(hostKey.WorkerPoolID),
		KeyID:   pgvalue.MustUUIDValue(hostKey.ID),
	}, nil
}

// SingleUseEnrollmentTokenTTL is how long a single-use enrollment token can
// admit its machine.
const SingleUseEnrollmentTokenTTL = time.Hour

// IssuedEnrollmentToken is a new single-use enrollment token, returned once.
type IssuedEnrollmentToken struct {
	Token     string
	ExpiresAt time.Time
}

// IssueEnrollmentToken issues a single-use enrollment token for an active or
// paused group. It admits one machine before SingleUseEnrollmentTokenTTL
// passes.
func IssueEnrollmentToken(ctx context.Context, txb db.TxBeginner, policy auditpolicy.Policy, operator Operator, groupID uuid.UUID) (IssuedEnrollmentToken, error) {
	if operator.safety {
		return IssuedEnrollmentToken{}, errSafetyOperation
	}
	token, err := auth.GenerateSingleUseEnrollmentToken()
	if err != nil {
		return IssuedEnrollmentToken{}, err
	}
	var issued IssuedEnrollmentToken
	err = db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		actor, owner, err := operator.admit(ctx, tx, groupID)
		if err != nil {
			return err
		}
		id := uuid.NewV7()
		err = tx.QueryRow(ctx, `INSERT INTO worker_enrollment_tokens(id,worker_group_id,token_hash,expires_at)
 SELECT $1,g.id,$3,clock_timestamp()+make_interval(secs => $4) FROM worker_groups g WHERE g.id=$2 AND g.status IN ('active','paused')
 RETURNING expires_at`, id, groupID, token.Hash, SingleUseEnrollmentTokenTTL.Seconds()).Scan(&issued.ExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return conflict("worker group is draining or disabled")
		}
		if err != nil {
			return fmt.Errorf("issue enrollment token: %w", err)
		}
		return audit.Append(ctx, tx, policy, audit.Event{Evidence: actor, OrgID: owner, Action: "worker_group.enrollment_token_issued", TargetKind: "worker_group", TargetID: groupID})
	})
	if err != nil {
		return IssuedEnrollmentToken{}, err
	}
	issued.Token = token.Raw
	return issued, nil
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
func ActivateHost(ctx context.Context, txb db.TxBeginner, policy auditpolicy.Policy, principal HostPrincipal, activation Activation) error {
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
			selected, err := q.SetInitialWorkerGroupPrimaryPool(ctx, db.SetInitialWorkerGroupPrimaryPoolParams{
				WorkerGroupID: pgvalue.UUID(principal.GroupID), WorkerPoolID: poolID,
			})
			if err != nil {
				return err
			}
			if !group.PrimaryPoolID.Valid && selected.PrimaryPoolID.Valid {
				if err := audit.Append(ctx, tx, policy, audit.Event{Evidence: audit.Internal(audit.ControlPlane), Action: "worker_pool.primary_selected", TargetKind: "worker_pool", TargetID: pgvalue.MustUUIDValue(selected.PrimaryPoolID)}); err != nil {
					return err
				}
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
		if err != nil {
			return err
		}
		return dbnotify.Notify(ctx, tx, "dispatch", "capacity")
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
		ID:                profile.ID,
		DescriptorDigest:  profile.VMRuntimeDescriptorDigest,
		FirecrackerDigest: profile.FirecrackerDigest, FirecrackerVersion: profile.FirecrackerVersion,
		SnapshotFormatVersion: profile.SnapshotFormatVersion, HostKernelRelease: profile.HostKernelRelease,
		CPUTemplateDigest: digest,
		KernelDigest:      profile.KernelDigest, InitramfsDigest: profile.InitramfsDigest, RootfsDigest: profile.RootfsDigest,
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

// RecordStartupRecovery acknowledges completed cleanup for the current recovering
// epoch. Computer physical observations must already be committed; this gate does
// not infer closure from an inventory or retain an audit record.
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
	Resources             ResourceObservation
	AdmissionPausedReason string
}

// RecordObservation records an observation of the authenticated epoch.
func RecordObservation(ctx context.Context, q db.Querier, principal HostPrincipal, observation HostObservation) error {
	if err := validateResourceObservation(observation.Resources); err != nil {
		return err
	}
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
	keys, _ := json.Marshal(observation.Resources.ReadyLeafKeys)
	if observation.Resources.ReadyLeafKeys == nil {
		keys = []byte("[]")
	}
	return db.RecordWorkerObservationParams{
		Sequence: observation.Resources.Sequence, PolicyRevision: observation.Resources.PolicyRevision,
		LocalCpuMillis: observation.Resources.LocalOptional.CPUMillis, LocalMemoryBytes: observation.Resources.LocalOptional.MemoryBytes,
		LocalVmSlots: observation.Resources.LocalOptional.VMSlots, LocalTasks: observation.Resources.LocalOptional.Tasks,
		OptionalDiskBytes: observation.Resources.OptionalDiskBytes, DiskSlotsForRealWork: observation.Resources.DiskSlotsForRealWork,
		ReclaimPending: observation.Resources.ReclaimPending, ReadyLeafKeys: keys,
		AdmissionPausedReason: pgtype.Text{String: observation.AdmissionPausedReason, Valid: observation.AdmissionPausedReason != ""},
		WorkerHostID:          pgvalue.UUID(principal.HostID), WorkerGroupID: pgvalue.UUID(principal.GroupID),
		WorkerEpoch: pgtype.Int8{Int64: principal.Epoch, Valid: true},
	}
}

// BeginHostDrain starts the worker-requested drain of the authenticated
// epoch, fenced by its host claim version. Draining again is a replay.
func BeginHostDrain(ctx context.Context, txb db.TxBeginner, principal HostPrincipal) error {
	return db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		poolID, err := q.GetWorkerHostPoolID(ctx, db.GetWorkerHostPoolIDParams{
			WorkerHostID: pgvalue.UUID(principal.HostID), WorkerGroupID: pgvalue.UUID(principal.GroupID),
			WorkerEpoch: pgtype.Int8{Int64: principal.Epoch, Valid: true},
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrHostNotFound
		}
		if err != nil {
			return fmt.Errorf("resolve draining worker pool: %w", err)
		}
		if _, err := LockHostWithPool(ctx, q, principal.GroupID, pgvalue.MustUUIDValue(poolID), principal.HostID, principal.Epoch); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrHostNotFound
			}
			return fmt.Errorf("lock worker drain: %w", err)
		}
		_, err = q.DrainWorkerHost(ctx, db.DrainWorkerHostParams{
			ID: pgvalue.UUID(principal.HostID), WorkerGroupID: pgvalue.UUID(principal.GroupID),
			ExpectedEpoch:        pgtype.Int8{Int64: principal.Epoch, Valid: true},
			ExpectedClaimVersion: principal.HostClaimVersion, DrainReason: "shutdown",
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrHostNotFound
		}
		if err != nil {
			return fmt.Errorf("drain worker host %s: %w", principal.HostID, err)
		}
		return dbnotify.Notify(ctx, tx, "dispatch", "capacity")
	})
}

// CompleteHostDrain marks a drained epoch termination ready once its instance
// state is gone, fenced by its host claim version. It locks the host's drain
// completion authority before completing the drain.
func CompleteHostDrain(ctx context.Context, txb db.TxBeginner, principal HostPrincipal) (db.CompleteWorkerDrainRow, error) {
	params := db.CompleteWorkerDrainParams{
		WorkerHostID:         pgvalue.UUID(principal.HostID),
		WorkerGroupID:        pgvalue.UUID(principal.GroupID),
		WorkerEpoch:          pgtype.Int8{Int64: principal.Epoch, Valid: true},
		ExpectedClaimVersion: principal.HostClaimVersion,
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
		if err != nil {
			return err
		}
		return dbnotify.Notify(ctx, tx, "dispatch", "capacity")
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
func FenceHost(ctx context.Context, txb db.TxBeginner, principal HostPrincipal, reasonCode string) error {
	reasonCode = strings.TrimSpace(reasonCode)
	if reasonCode != "provider_termination" && reasonCode != "worker_retired" {
		return invalidInput("unsupported worker fence reason")
	}
	return db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		if err := fenceHost(ctx, db.New(tx), principal); err != nil {
			return err
		}
		return dbnotify.Notify(ctx, tx, "dispatch", "capacity")
	})
}

func fenceHost(ctx context.Context, q db.Querier, principal HostPrincipal) error {
	_, err := q.FenceWorkerHost(ctx, db.FenceWorkerHostParams{
		ID:                   pgvalue.UUID(principal.HostID),
		WorkerGroupID:        pgvalue.UUID(principal.GroupID),
		ExpectedEpoch:        pgtype.Int8{Int64: principal.Epoch, Valid: true},
		ExpectedClaimVersion: principal.HostClaimVersion,
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
