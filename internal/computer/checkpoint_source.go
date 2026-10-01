package computer

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// CheckpointRef addresses the capture checkpoint of one Instance incarnation
// on a worker epoch. WorkerEpoch is the epoch the capture request names;
// it must be the host's authenticated epoch.
type CheckpointRef struct {
	Host           Host
	InstanceID     uuid.UUID
	WorkerEpoch    int64
	DesiredVersion int64
	CheckpointID   uuid.UUID
}

// checkpointSource is the fence of a capture source: the worker Group and
// Host, the Computer, the Instance, its resident members and its capture
// checkpoint are locked in the owning transaction. Checkpoint registration,
// readiness, failure and object recording build on it. A checkpointSource is
// valid only inside the transaction that locked it.
type checkpointSource struct {
	tx         pgx.Tx
	computer   db.Computer
	instance   db.ComputerInstance
	checkpoint db.ComputerCheckpoint
}

// lockCheckpointSource locks the capture source of a checkpoint without
// comparing claim versions: the worker Group and Host at the epoch, the
// Computer, the Instance, then the Sessions, Runs, Attempts, Run leases and
// Waits of the Instance's unreconciled leases in id order, and finally the
// Instance's capture checkpoint, which must be the addressed one. A fence
// that no longer holds returns pgx.ErrNoRows.
func lockCheckpointSource(ctx context.Context, tx pgx.Tx, ref CheckpointRef) (checkpointSource, error) {
	q := db.New(tx)
	host := ref.Host
	groupID, hostID := pgvalue.UUID(host.GroupID), pgvalue.UUID(host.HostID)
	var environmentID, computerID, orgID, instanceID pgtype.UUID
	var region string
	if ref.WorkerEpoch != host.Epoch || host.Epoch <= 0 || ref.DesiredVersion <= 0 {
		return checkpointSource{}, pgx.ErrNoRows
	}
	if err := tx.QueryRow(ctx, `SELECT environment_id,computer_id,org_id,id,region_id FROM computer_instances
 WHERE id=$1 AND worker_group_id=$2 AND worker_host_id=$3 AND worker_epoch=$4`, pgvalue.UUID(ref.InstanceID), groupID, hostID, host.Epoch).Scan(&environmentID, &computerID, &orgID, &instanceID, &region); err != nil {
		return checkpointSource{}, err
	}
	if _, err := workergroup.LockHostIgnoringClaims(ctx, q, host.GroupID, region, host.HostID, host.Epoch); err != nil {
		return checkpointSource{}, err
	}
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: environmentID, ID: computerID})
	if err != nil {
		return checkpointSource{}, err
	}
	instance, err := q.LockWorkerComputerInstance(ctx, db.LockWorkerComputerInstanceParams{ID: instanceID, OrgID: orgID, WorkerHostID: hostID, WorkerGroupID: groupID, WorkerEpoch: host.Epoch})
	if err != nil {
		return checkpointSource{}, err
	}
	if instance.ID != pgvalue.UUID(ref.InstanceID) {
		return checkpointSource{}, pgx.ErrNoRows
	}
	for _, query := range []string{
		`SELECT s.id FROM sessions s JOIN runs r ON r.session_id=s.id JOIN run_leases l ON l.run_id=r.id WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL ORDER BY s.id FOR UPDATE OF s`,
		`SELECT r.id FROM runs r JOIN run_leases l ON l.run_id=r.id WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL ORDER BY r.id FOR UPDATE OF r`,
		`SELECT a.run_id FROM run_attempts a JOIN run_leases l ON l.run_id=a.run_id AND l.attempt_number=a.number WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL ORDER BY a.run_id,a.number FOR UPDATE OF a`,
		`SELECT id FROM run_leases WHERE computer_instance_id=$1 AND process_reconciled_at IS NULL ORDER BY run_id,id FOR UPDATE`,
		`SELECT w.id FROM run_waits w JOIN run_leases l ON l.run_id=w.run_id AND l.attempt_number=w.attempt_number WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL ORDER BY w.run_id,w.id FOR UPDATE OF w`,
	} {
		if _, err = tx.Exec(ctx, query, instance.ID); err != nil {
			return checkpointSource{}, err
		}
	}
	cp, err := q.LockComputerCheckpoint(ctx, db.LockComputerCheckpointParams{EnvironmentID: environmentID, ComputerID: computerID, CheckpointID: instance.CaptureCheckpointID})
	if err != nil {
		return checkpointSource{}, err
	}
	if cp.ID != pgvalue.UUID(ref.CheckpointID) {
		return checkpointSource{}, pgx.ErrNoRows
	}
	return checkpointSource{tx: tx, computer: c, instance: instance, checkpoint: cp}, nil
}

// checkLive checks that the Instance is still capturing this checkpoint at
// the addressed desired version on a fresh worker host, with its writer and
// checkpoint deadlines unexpired. Callers repeat it after blocking writes.
func (s checkpointSource) checkLive(ctx context.Context, ref CheckpointRef) error {
	_, err := db.New(s.tx).GetComputerInstanceCaptureCheckpoint(ctx, db.GetComputerInstanceCaptureCheckpointParams{ComputerInstanceID: s.instance.ID, EnvironmentID: s.instance.EnvironmentID, WorkerGroupID: pgvalue.UUID(ref.Host.GroupID), WorkerHostID: pgvalue.UUID(ref.Host.HostID), WorkerEpoch: ref.Host.Epoch, DesiredVersion: ref.DesiredVersion, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds})
	return err
}

// checkMembers checks, under the source's resident member locks, that the
// Instance's unreconciled leases are exactly the checkpoint's memberCount
// members and that each is still checkpointing its Run, Attempt and Wait for
// this checkpoint. A changed member set returns pgx.ErrNoRows.
func (s checkpointSource) checkMembers(ctx context.Context, memberCount int) error {
	instance, cp := s.instance, s.checkpoint
	var live bool
	err := s.tx.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM run_leases WHERE computer_instance_id=$1 AND process_reconciled_at IS NULL)=$2
 AND NOT EXISTS(SELECT 1 FROM computer_checkpoint_runs m
 LEFT JOIN run_leases l ON l.id=m.source_run_lease_id
 LEFT JOIN runs r ON r.id=m.run_id
 LEFT JOIN run_attempts a ON a.run_id=m.run_id AND a.number=m.attempt_number
 LEFT JOIN run_waits w ON w.id=m.run_wait_id
 WHERE m.checkpoint_id=$3 AND NOT coalesce(l.status='checkpointing' AND l.expires_at>clock_timestamp()
 AND l.computer_instance_id=$1 AND l.writer_generation=$4 AND l.process_reconciled_at IS NULL
 AND r.status='waiting' AND r.current_run_lease_id=l.id AND r.current_attempt_number=m.attempt_number
 AND r.terminal_at IS NULL AND a.terminal_at IS NULL
 AND w.suspension_status='checkpointing' AND w.current_run_lease_id=l.id AND w.suspend_checkpoint_id=$3
 AND w.expected_run_revision=r.revision,false))`, instance.ID, memberCount, cp.ID, cp.WriterGeneration).Scan(&live)
	if err != nil {
		return err
	}
	if !live {
		return pgx.ErrNoRows
	}
	return nil
}

// requireRootPinned requires the disk root digest pinned for the
// checkpoint's publication by the source Instance at desiredVersion. A
// missing pin returns pgx.ErrNoRows.
func (s checkpointSource) requireRootPinned(ctx context.Context, desiredVersion int64, digest string) error {
	_, err := db.New(s.tx).RequireComputerObjectPin(ctx, db.RequireComputerObjectPinParams{ComputerInstanceID: s.instance.ID, PublicationKey: checkpointPublicationKey(pgvalue.MustUUIDValue(s.checkpoint.ID)), InstanceDesiredVersion: desiredVersion, Digest: digest})
	return err
}
