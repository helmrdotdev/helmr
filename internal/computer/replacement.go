package computer

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	// ErrReplacementBlocked reports a program replacement that cannot proceed
	// yet: the Computer does not admit the new program, members still hold
	// the Computer, or the checkpoint's source Instance is not yet excluded.
	ErrReplacementBlocked = errors.New("computer program replacement is blocked")
	// ErrReplacementChanged reports a Computer or checkpoint that no longer
	// matches the program replacement discovered for it.
	ErrReplacementChanged = errors.New("computer program replacement changed")
)

// ReplacementRef addresses a Computer that a Run of the deployment needs to
// run on.
type ReplacementRef struct {
	EnvironmentID uuid.UUID
	ComputerID    uuid.UUID
	DeploymentID  uuid.UUID
}

// ReplacementKind is the step a program replacement takes next.
type ReplacementKind uint8

const (
	// ReplacementNone needs no step: the Computer's program is the
	// deployment's, or the Computer has no Instance or ready checkpoint.
	ReplacementNone ReplacementKind = iota
	// ReplacementCapture captures the live Instance of an obsolete program.
	ReplacementCapture
	// ReplacementPromotion promotes the disk of the ready checkpoint of an
	// obsolete program to the Computer's head, discarding its memory.
	ReplacementPromotion
)

// Replacement is the fence of a program replacement, which preserves a
// Computer's current disk through a whole-Instance capture and never resumes
// old program memory with new code. A Replacement is valid only inside the
// transaction that locked it.
type Replacement struct {
	tx         pgx.Tx
	kind       ReplacementKind
	capture    captureFence
	computer   db.Computer
	checkpoint db.ComputerCheckpoint
	sourceID   pgtype.UUID
}

// LockReplacement locks the program replacement the Computer needs for the
// deployment. A capture locks worker_groups and worker_hosts without checking
// them, the Computer, the source Instance, and after the program admission
// and member checks it requires the supply to continue admitted work and
// locks the capture's Instance fence. A promotion locks the Computer, the
// checkpoint's source Instance and then the checkpoint. A Computer or
// checkpoint that changed returns ErrReplacementChanged, a replacement that
// must wait returns ErrReplacementBlocked, and a capture fence that no longer
// holds returns pgx.ErrNoRows.
func LockReplacement(ctx context.Context, tx pgx.Tx, ref ReplacementRef) (Replacement, error) {
	q := db.New(tx)
	environmentID, computerID, deploymentID := pgvalue.UUID(ref.EnvironmentID), pgvalue.UUID(ref.ComputerID), pgvalue.UUID(ref.DeploymentID)
	var instanceID, checkpointID, currentProgram pgtype.UUID
	err := tx.QueryRow(ctx, `SELECT id,program_deployment_id FROM computer_instances WHERE computer_id=$1 AND environment_id=$2 AND reclaimed_at IS NULL`, computerID, environmentID).Scan(&instanceID, &currentProgram)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id,source_computer_instance_id,program_deployment_id FROM computer_checkpoints WHERE computer_id=$1 AND environment_id=$2 AND status='ready' AND resume_committed_at IS NULL ORDER BY created_at DESC,id DESC LIMIT 1`, computerID, environmentID).Scan(&checkpointID, &instanceID, &currentProgram)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Replacement{}, nil
	}
	if err != nil {
		return Replacement{}, err
	}
	if currentProgram == deploymentID {
		return Replacement{}, nil
	}
	source, err := q.GetComputerInstance(ctx, db.GetComputerInstanceParams{EnvironmentID: environmentID, ID: instanceID})
	if err != nil {
		return Replacement{}, err
	}
	var host workergroup.LockedHost
	if !checkpointID.Valid {
		// Match capture's supply-before-Computer lock order.
		host, err = workergroup.LockHostUnchecked(ctx, q, pgvalue.MustUUIDValue(source.WorkerGroupID), source.RegionID, pgvalue.MustUUIDValue(source.WorkerHostID), source.WorkerEpoch)
		if err != nil {
			return Replacement{}, err
		}
	}
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: environmentID, ID: computerID})
	if err != nil {
		return Replacement{}, err
	}
	if c.Status != "active" || c.DesiredState != "active" || c.DeletedAt.Valid || len(c.RecoveryFailure) > 0 || len(c.PreparationFailure) > 0 || c.DirtyState == "dirty_state_lost" {
		return Replacement{}, ErrReplacementChanged
	}
	if _, err = tx.Exec(ctx, `SELECT id FROM computer_instances WHERE id=$1 FOR UPDATE`, instanceID); err != nil {
		return Replacement{}, err
	}
	admission, err := q.GetComputerProgramAdmission(ctx, db.GetComputerProgramAdmissionParams{EnvironmentID: environmentID, ComputerID: computerID, ComputerSpecID: c.ComputerSpecID, DeploymentID: deploymentID})
	if err != nil {
		return Replacement{}, err
	}
	if !admission.SpecCompatible || !admission.ProgramCompatible.Valid || !admission.ProgramCompatible.Bool {
		return Replacement{}, ErrReplacementBlocked
	}
	var clear bool
	err = tx.QueryRow(ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM run_leases WHERE computer_id=$1 AND process_reconciled_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM run_waits WHERE computer_id=$1 AND suspension_status IN ('parked','resume_pending','resuming'))
 AND NOT EXISTS(SELECT 1 FROM computer_commands WHERE computer_id=$1 AND computer_instance_id IS NOT NULL AND process_reconciled_at IS NULL)`, computerID).Scan(&clear)
	if err != nil {
		return Replacement{}, err
	}
	if !clear {
		return Replacement{}, ErrReplacementBlocked
	}
	if !checkpointID.Valid {
		// The supply check ranks after the program and member checks, so a lost
		// host still blocks a replacement those checks block.
		if !host.Continues() {
			return Replacement{}, pgx.ErrNoRows
		}
		request := captureRequest(Capture{CheckpointID: uuid.NewV7(), EnvironmentID: ref.EnvironmentID, InstanceID: pgvalue.MustUUIDValue(source.ID), WriterGeneration: source.WriterGeneration, MembershipRevision: source.MembershipRevision, DesiredVersion: source.DesiredVersion})
		fence, err := lockCaptureInstance(ctx, tx, request, host.Host, source.WorkerEpoch, c)
		if err != nil {
			return Replacement{}, err
		}
		return Replacement{tx: tx, kind: ReplacementCapture, capture: fence}, nil
	}
	cp, err := q.LockComputerCheckpoint(ctx, db.LockComputerCheckpointParams{EnvironmentID: environmentID, ComputerID: computerID, CheckpointID: checkpointID})
	if err != nil {
		return Replacement{}, err
	}
	if cp.Status != "ready" || cp.ResumeCommittedAt.Valid || cp.ProgramDeploymentID != currentProgram || cp.BaseComputerDiskVersionID != c.HeadDiskVersionID || cp.WriterGeneration != c.WriterGeneration || cp.ComputerSpecID != c.ComputerSpecID {
		return Replacement{}, ErrReplacementChanged
	}
	err = tx.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM computer_instances WHERE id=$2 AND computer_id=$1 AND reclaimed_at IS NOT NULL)
 AND NOT EXISTS(SELECT 1 FROM computer_instances WHERE computer_id=$1 AND reclaimed_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM computer_instances WHERE source_checkpoint_id=$3)`, computerID, instanceID, checkpointID).Scan(&clear)
	if err != nil {
		return Replacement{}, err
	}
	if !clear {
		return Replacement{}, ErrReplacementBlocked
	}
	return Replacement{tx: tx, kind: ReplacementPromotion, computer: c, checkpoint: cp, sourceID: instanceID}, nil
}

// Kind is the step the replacement takes next.
func (r Replacement) Kind() ReplacementKind {
	return r.kind
}

// Capture seals the complete resident set of the obsolete program's Instance
// into a new creating checkpoint, as BeginCapture does, on a capture
// replacement.
func (r Replacement) Capture(ctx context.Context) (db.ComputerCheckpoint, error) {
	if r.kind != ReplacementCapture {
		return db.ComputerCheckpoint{}, errors.New("computer program replacement is not a capture")
	}
	return r.capture.seal(ctx)
}

// Promote commits the ready checkpoint's private disk version, makes it the
// Computer's head and invalidates the checkpoint, on a promotion replacement.
// The caller locks the Run it places after the checkpoint and before Promote.
// A disk version or Computer that changed returns ErrReplacementChanged.
func (r Replacement) Promote(ctx context.Context) error {
	if r.kind != ReplacementPromotion {
		return errors.New("computer program replacement is not a promotion")
	}
	c, cp := r.computer, r.checkpoint
	tag, err := r.tx.Exec(ctx, `UPDATE computer_disk_versions v SET status='committed',published_at=clock_timestamp()
 WHERE v.id=$1 AND v.environment_id=$2 AND v.computer_id=$3 AND v.status='private'
 AND v.parent_version_id=$4 AND v.source_computer_instance_id=$5 AND v.writer_generation=$6 AND v.payload_retired_at IS NULL
 AND EXISTS(SELECT 1 FROM computer_disk_version_roots root JOIN computer_disk_roots shared ON shared.environment_id=root.environment_id AND shared.id=root.root_id JOIN computer_objects object ON object.environment_id=shared.environment_id AND object.digest=shared.root_pack_digest
 WHERE root.version_id=v.id AND root.computer_id=v.computer_id AND object.certified)`, cp.PrivateComputerDiskVersionID, c.EnvironmentID, c.ID, c.HeadDiskVersionID, r.sourceID, c.WriterGeneration)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrReplacementChanged
	}
	tag, err = r.tx.Exec(ctx, `UPDATE computers SET head_disk_version_id=$2,revision=revision+1,updated_at=clock_timestamp() WHERE id=$1 AND revision=$3 AND writer_generation=$4 AND head_disk_version_id=$5`, c.ID, cp.PrivateComputerDiskVersionID, c.Revision, c.WriterGeneration, c.HeadDiskVersionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrReplacementChanged
	}
	_, err = r.tx.Exec(ctx, `UPDATE computer_checkpoints SET status='invalid',invalidated_at=clock_timestamp(),invalidation_reason_code='program_replaced' WHERE id=$1`, cp.ID)
	return err
}
