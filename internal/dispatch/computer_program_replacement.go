package dispatch

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Replacement preserves the current disk through a whole-Instance capture. It
// never resumes old Program memory with new code. Run placement retries after
// this transaction; disk promotion requires no capacity for the obsolete VM.
func prepareComputerProgram(ctx context.Context, tx pgx.Tx, candidate ReadyRunCandidate, environmentID, computerID pgtype.UUID) (bool, error) {
	q := db.New(tx)
	var deploymentID, instanceID, checkpointID pgtype.UUID
	if err := tx.QueryRow(ctx, `SELECT deployment_id FROM runs WHERE id=$1 AND org_id=$2 AND revision=$3`, candidate.RunID, candidate.OrgID, candidate.ExpectedRunRevision).Scan(&deploymentID); err != nil {
		return false, err
	}
	var currentProgram pgtype.UUID
	err := tx.QueryRow(ctx, `SELECT id,program_deployment_id FROM computer_instances WHERE computer_id=$1 AND environment_id=$2 AND reclaimed_at IS NULL`, computerID, environmentID).Scan(&instanceID, &currentProgram)
	if errors.Is(err, pgx.ErrNoRows) {
		err = tx.QueryRow(ctx, `SELECT id,source_computer_instance_id,program_deployment_id FROM computer_checkpoints WHERE computer_id=$1 AND environment_id=$2 AND status='ready' AND resume_committed_at IS NULL ORDER BY created_at DESC,id DESC LIMIT 1`, computerID, environmentID).Scan(&checkpointID, &instanceID, &currentProgram)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if currentProgram == deploymentID {
		return false, nil
	}
	source, err := q.GetComputerInstance(ctx, db.GetComputerInstanceParams{EnvironmentID: environmentID, ID: instanceID})
	if err != nil {
		return false, err
	}
	if !checkpointID.Valid {
		// Match capture's supply-before-Computer lock order.
		if _, err = q.LockRunLeaseClaimWorkerGroup(ctx, db.LockRunLeaseClaimWorkerGroupParams{ID: source.WorkerGroupID, RegionID: source.RegionID}); err != nil {
			return false, err
		}
		if _, err = q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{ID: source.WorkerHostID, WorkerGroupID: source.WorkerGroupID}); err != nil {
			return false, err
		}
	}
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: environmentID, ID: computerID})
	if err != nil {
		return false, err
	}
	if c.Status != "active" || c.DesiredState != "active" || c.DeletedAt.Valid || len(c.RecoveryFailure) > 0 || len(c.PreparationFailure) > 0 || c.DirtyState == "dirty_state_lost" || c.DirtyState == "capture_failed" {
		return false, ErrCandidateChanged
	}
	if _, err = tx.Exec(ctx, `SELECT id FROM computer_instances WHERE id=$1 FOR UPDATE`, instanceID); err != nil {
		return false, err
	}
	admission, err := q.GetComputerProgramAdmission(ctx, db.GetComputerProgramAdmissionParams{EnvironmentID: environmentID, ComputerID: computerID, ComputerSpecID: c.ComputerSpecID, DeploymentID: deploymentID})
	if err != nil {
		return false, err
	}
	if !admission.SpecCompatible || !admission.ProgramCompatible.Valid || !admission.ProgramCompatible.Bool {
		return false, ErrCapacityUnavailable
	}
	var clear bool
	err = tx.QueryRow(ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM run_leases WHERE computer_id=$1 AND process_reconciled_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM run_waits WHERE computer_id=$1 AND suspension_status IN ('parked','resume_pending','resuming'))
 AND NOT EXISTS(SELECT 1 FROM computer_commands WHERE computer_id=$1 AND computer_instance_id IS NOT NULL AND process_reconciled_at IS NULL)`, computerID).Scan(&clear)
	if err != nil {
		return false, err
	}
	if !clear {
		return false, ErrCapacityUnavailable
	}
	if !checkpointID.Valid {
		// Capture locks the stable resident set before any new target member lock.
		if _, err = BeginComputerCapture(ctx, tx, db.BeginComputerCheckpointParams{CheckpointID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: environmentID, ComputerInstanceID: source.ID, WriterGeneration: source.WriterGeneration, MembershipRevision: source.MembershipRevision, DesiredVersion: source.DesiredVersion}); err != nil {
			return false, err
		}
		if _, err = lockRunPlacementAuthority(ctx, tx, candidate, computerPlacement{computer: c}); err != nil {
			return false, err
		}
		return true, nil
	}
	cp, err := q.LockComputerCheckpoint(ctx, db.LockComputerCheckpointParams{EnvironmentID: environmentID, ComputerID: computerID, CheckpointID: checkpointID})
	if err != nil {
		return false, err
	}
	if cp.Status != "ready" || cp.ResumeCommittedAt.Valid || cp.ProgramDeploymentID != currentProgram || cp.BaseComputerDiskVersionID != c.HeadDiskVersionID || cp.WriterGeneration != c.WriterGeneration || cp.ComputerSpecID != c.ComputerSpecID {
		return false, ErrCandidateChanged
	}
	err = tx.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM computer_instances WHERE id=$2 AND computer_id=$1 AND reclaimed_at IS NOT NULL)
 AND NOT EXISTS(SELECT 1 FROM computer_instances WHERE computer_id=$1 AND reclaimed_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM computer_instances WHERE source_checkpoint_id=$3)`, computerID, instanceID, checkpointID).Scan(&clear)
	if err != nil {
		return false, err
	}
	if !clear {
		return false, ErrCapacityUnavailable
	}
	if _, err = lockRunPlacementAuthority(ctx, tx, candidate, computerPlacement{computer: c}); err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE computer_disk_versions v SET status='committed',published_at=clock_timestamp()
 WHERE v.id=$1 AND v.environment_id=$2 AND v.computer_id=$3 AND v.status='private'
 AND v.parent_version_id=$4 AND v.source_computer_instance_id=$5 AND v.writer_generation=$6 AND v.payload_retired_at IS NULL
 AND EXISTS(SELECT 1 FROM computer_disk_version_roots root JOIN computer_objects object ON object.environment_id=root.environment_id AND object.computer_id=root.computer_id AND object.digest=v.root_pack_digest
 WHERE root.version_id=v.id AND root.computer_id=v.computer_id AND object.certified)`, cp.PrivateComputerDiskVersionID, environmentID, computerID, c.HeadDiskVersionID, instanceID, c.WriterGeneration)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, ErrCandidateChanged
	}
	tag, err = tx.Exec(ctx, `UPDATE computers SET head_disk_version_id=$2,revision=revision+1,updated_at=clock_timestamp() WHERE id=$1 AND revision=$3 AND writer_generation=$4 AND head_disk_version_id=$5`, computerID, cp.PrivateComputerDiskVersionID, c.Revision, c.WriterGeneration, c.HeadDiskVersionID)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, ErrCandidateChanged
	}
	_, err = tx.Exec(ctx, `UPDATE computer_checkpoints SET status='invalid',invalidated_at=clock_timestamp(),invalidation_reason_code='program_replaced' WHERE id=$1`, checkpointID)
	return true, err
}
