package computer

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// CheckpointRef addresses the capture checkpoint of one Instance incarnation
// on a worker host epoch. WorkerEpoch is the epoch the capture request names;
// it must be the host's authenticated epoch.
type CheckpointRef struct {
	Host           Host
	InstanceID     uuid.UUID
	WorkerEpoch    int64
	DesiredVersion int64
	CheckpointID   uuid.UUID
}

// CheckpointSource is the fence of a capture source: the worker Group and
// Host, the Computer, the Instance, its resident members and its capture
// checkpoint are locked in the owning transaction. Checkpoint registration,
// readiness and failure build on it. A CheckpointSource is valid only inside
// the transaction that locked it.
type CheckpointSource struct {
	tx         pgx.Tx
	computer   db.Computer
	instance   db.ComputerInstance
	checkpoint db.ComputerCheckpoint
}

// LockCheckpointSource locks the capture source of a checkpoint without
// comparing claim versions: the worker Group and Host at the epoch, the
// Computer, the Instance, then the Sessions, Runs, Attempts, Run leases and
// Waits of the Instance's unreconciled leases in id order, and finally the
// Instance's capture checkpoint, which must be the addressed one. A fence
// that no longer holds returns pgx.ErrNoRows.
func LockCheckpointSource(ctx context.Context, tx pgx.Tx, ref CheckpointRef) (CheckpointSource, error) {
	q := db.New(tx)
	host := ref.Host
	groupID, hostID := pgvalue.UUID(host.GroupID), pgvalue.UUID(host.HostID)
	var environmentID, computerID, orgID, instanceID pgtype.UUID
	var region string
	if ref.WorkerEpoch != host.Epoch || host.Epoch <= 0 || ref.DesiredVersion <= 0 {
		return CheckpointSource{}, pgx.ErrNoRows
	}
	if err := tx.QueryRow(ctx, `SELECT environment_id,computer_id,org_id,id,region_id FROM computer_instances
 WHERE id=$1 AND worker_group_id=$2 AND worker_host_id=$3 AND worker_epoch=$4`, pgvalue.UUID(ref.InstanceID), groupID, hostID, host.Epoch).Scan(&environmentID, &computerID, &orgID, &instanceID, &region); err != nil {
		return CheckpointSource{}, err
	}
	if _, err := workergroup.LockHostIgnoringClaims(ctx, q, host.GroupID, region, host.HostID, host.Epoch); err != nil {
		return CheckpointSource{}, err
	}
	c, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: environmentID, ID: computerID})
	if err != nil {
		return CheckpointSource{}, err
	}
	instance, err := q.LockWorkerComputerInstance(ctx, db.LockWorkerComputerInstanceParams{ID: instanceID, OrgID: orgID, WorkerHostID: hostID, WorkerGroupID: groupID, WorkerEpoch: host.Epoch})
	if err != nil {
		return CheckpointSource{}, err
	}
	if instance.ID != pgvalue.UUID(ref.InstanceID) {
		return CheckpointSource{}, pgx.ErrNoRows
	}
	for _, query := range []string{
		`SELECT s.id FROM sessions s JOIN runs r ON r.session_id=s.id JOIN run_leases l ON l.run_id=r.id WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL ORDER BY s.id FOR UPDATE OF s`,
		`SELECT r.id FROM runs r JOIN run_leases l ON l.run_id=r.id WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL ORDER BY r.id FOR UPDATE OF r`,
		`SELECT a.run_id FROM run_attempts a JOIN run_leases l ON l.run_id=a.run_id AND l.attempt_number=a.number WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL ORDER BY a.run_id,a.number FOR UPDATE OF a`,
		`SELECT id FROM run_leases WHERE computer_instance_id=$1 AND process_reconciled_at IS NULL ORDER BY run_id,id FOR UPDATE`,
		`SELECT w.id FROM run_waits w JOIN run_leases l ON l.run_id=w.run_id AND l.attempt_number=w.attempt_number WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL ORDER BY w.run_id,w.id FOR UPDATE OF w`,
	} {
		if _, err = tx.Exec(ctx, query, instance.ID); err != nil {
			return CheckpointSource{}, err
		}
	}
	cp, err := q.LockComputerCheckpoint(ctx, db.LockComputerCheckpointParams{EnvironmentID: environmentID, ComputerID: computerID, CheckpointID: instance.CaptureCheckpointID})
	if err != nil {
		return CheckpointSource{}, err
	}
	if cp.ID != pgvalue.UUID(ref.CheckpointID) {
		return CheckpointSource{}, pgx.ErrNoRows
	}
	return CheckpointSource{tx: tx, computer: c, instance: instance, checkpoint: cp}, nil
}

// Computer is the locked Computer.
func (s CheckpointSource) Computer() db.Computer {
	return s.computer
}

// Instance is the locked source Instance.
func (s CheckpointSource) Instance() db.ComputerInstance {
	return s.instance
}

// Checkpoint is the locked capture checkpoint.
func (s CheckpointSource) Checkpoint() db.ComputerCheckpoint {
	return s.checkpoint
}

// CheckMembers checks, under the source's resident member locks, that the
// Instance's unreconciled leases are exactly the checkpoint's memberCount
// members and that each is still checkpointing its Run, Attempt and Wait for
// this checkpoint. A changed member set returns pgx.ErrNoRows.
func (s CheckpointSource) CheckMembers(ctx context.Context, memberCount int) error {
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

// RequireRootPinned requires the disk root digest pinned for the checkpoint's
// publication by the source Instance at desiredVersion. A missing pin returns
// pgx.ErrNoRows.
func (s CheckpointSource) RequireRootPinned(ctx context.Context, desiredVersion int64, digest string) error {
	_, err := db.New(s.tx).RequireComputerObjectPin(ctx, db.RequireComputerObjectPinParams{ComputerInstanceID: s.instance.ID, PublicationKey: checkpointPublicationKey(pgvalue.MustUUIDValue(s.checkpoint.ID)), InstanceDesiredVersion: desiredVersion, Digest: digest})
	return err
}

// checkpointPublication is the authority that records a capture's disk
// objects: its checkpoint source is locked and fresh, its manifest is
// registered and its members are still checkpointing. It compares no claim
// versions.
type checkpointPublication struct {
	tx     pgx.Tx
	ref    CheckpointRef
	source CheckpointSource
}

// lockCheckpointPublication locks the checkpoint source and checks capture
// freshness, the registered manifest and the member set.
func lockCheckpointPublication(ctx context.Context, tx pgx.Tx, ref CheckpointRef) (checkpointPublication, error) {
	source, err := LockCheckpointSource(ctx, tx, ref)
	if err != nil {
		return checkpointPublication{}, err
	}
	instance, cp := source.instance, source.checkpoint
	q := db.New(tx)
	if _, err = q.GetComputerInstanceCaptureCheckpoint(ctx, db.GetComputerInstanceCaptureCheckpointParams{ComputerInstanceID: instance.ID, EnvironmentID: instance.EnvironmentID, WorkerGroupID: pgvalue.UUID(ref.Host.GroupID), WorkerHostID: pgvalue.UUID(ref.Host.HostID), WorkerEpoch: ref.Host.Epoch, DesiredVersion: ref.DesiredVersion, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds}); err != nil {
		return checkpointPublication{}, err
	}
	if _, err = q.RequireRegisteredCheckpointManifest(ctx, db.RequireRegisteredCheckpointManifestParams{ID: cp.ID, Manifest: cp.Manifest}); err != nil {
		return checkpointPublication{}, err
	}
	members, err := q.ListComputerCheckpointRuns(ctx, db.ListComputerCheckpointRunsParams{EnvironmentID: instance.EnvironmentID, CheckpointID: cp.ID})
	if err != nil {
		return checkpointPublication{}, err
	}
	if err = source.CheckMembers(ctx, len(members)); err != nil {
		return checkpointPublication{}, err
	}
	return checkpointPublication{tx: tx, ref: ref, source: source}, nil
}

// recheck re-evaluates the whole authority after blocking object writes,
// before commit.
func (p checkpointPublication) recheck(ctx context.Context) error {
	_, err := lockCheckpointPublication(ctx, p.tx, p.ref)
	return err
}

// objects is the checkpoint's object scope: the Instance's retained source
// keys and its pinned write key.
func (p checkpointPublication) objects(ctx context.Context) (objectScope, error) {
	instance := p.source.instance
	q := db.New(p.tx)
	keys, err := q.ListInstanceComputerSourceKeys(ctx, instance.ID)
	if err != nil {
		return objectScope{}, err
	}
	allowed := make(map[string]bool, len(keys)+1)
	for _, key := range keys {
		allowed[pgvalue.UUIDString(key.ID)] = true
	}
	write, err := q.GetRuntimeComputerWriteKey(ctx, db.GetRuntimeComputerWriteKeyParams{ComputerInstanceID: instance.ID, EnvironmentID: instance.EnvironmentID, ComputerID: instance.ComputerID})
	if err != nil {
		return objectScope{}, err
	}
	if !instance.WriteKeyID.Valid || instance.WriteKeyID != write.ID {
		return objectScope{}, objectConflict("runtime write key is not pinned")
	}
	allowed[pgvalue.UUIDString(write.ID)] = true
	return objectScope{
		objectRetention: objectRetention{environmentID: instance.EnvironmentID, computerID: instance.ComputerID, instanceID: instance.ID, desiredVersion: instance.DesiredVersion, key: checkpointPublicationKey(pgvalue.MustUUIDValue(p.source.checkpoint.ID))},
		orgID:           instance.OrgID, projectID: instance.ProjectID, logicalBytes: instance.ReservedGuestEphemeralDiskBytes, allowedKeys: allowed,
	}, nil
}

// RegisterCheckpointObject registers a capture's disk object before its
// upload, in one transaction that rechecks the checkpoint authority after
// recording.
func (p Publisher) RegisterCheckpointObject(ctx context.Context, ref CheckpointRef, inspection blockformat.ObjectInspection) error {
	return p.inCheckpointPublication(ctx, ref, func(c checkpointPublication, scope objectScope) error {
		return scope.registerObject(ctx, c.tx, inspection)
	})
}

// ReuseCheckpointObject pins an already certified object of the Computer for
// the capture.
func (p Publisher) ReuseCheckpointObject(ctx context.Context, ref CheckpointRef, inspection blockformat.ObjectInspection) error {
	return p.inCheckpointPublication(ctx, ref, func(c checkpointPublication, scope objectScope) error {
		return scope.reuseObject(ctx, c.tx, inspection)
	})
}

// CertifyCheckpointObject certifies a registered capture object after its
// upload: a first transaction verifies the exact registration and pin, object
// storage confirms the bytes outside any transaction, and a second
// transaction certifies them and rechecks the checkpoint authority. A storage
// failure reports ErrStorageUnavailable.
func (p Publisher) CertifyCheckpointObject(ctx context.Context, ref CheckpointRef, inspection blockformat.ObjectInspection) error {
	object, err := describeObject(inspection)
	if err != nil {
		return err
	}
	if err = p.inCheckpointPublication(ctx, ref, func(c checkpointPublication, scope objectScope) error {
		return scope.verifyRegistered(ctx, c.tx, inspection)
	}); err != nil {
		return err
	}
	stored, err := p.objects.Stat(ctx, object.digest)
	if err != nil {
		return storageUnavailable(err)
	}
	return p.inCheckpointPublication(ctx, ref, func(c checkpointPublication, scope objectScope) error {
		return scope.certifyObject(ctx, c.tx, inspection, stored)
	})
}

// inCheckpointPublication runs fn in one transaction under the checkpoint
// publication authority and its object scope, then rechecks the authority.
func (p Publisher) inCheckpointPublication(ctx context.Context, ref CheckpointRef, fn func(checkpointPublication, objectScope) error) error {
	err := db.RunTx(ctx, p.db, func(tx pgx.Tx) error {
		c, err := lockCheckpointPublication(ctx, tx, ref)
		if err != nil {
			return err
		}
		scope, err := c.objects(ctx)
		if err != nil {
			return err
		}
		if err = fn(c, scope); err != nil {
			return err
		}
		return c.recheck(ctx)
	})
	return authorityChanged(err)
}
