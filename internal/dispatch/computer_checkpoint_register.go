package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ComputerCaptureWorker is supplied by authenticated transport, not the payload.
type ComputerCaptureWorker struct {
	GroupID pgtype.UUID
	HostID  pgtype.UUID
	Epoch   int64
}

var ErrCheckpointCandidate = errors.New("invalid Computer checkpoint candidate")

type checkpointObject struct {
	role, media string
	artifact    workerapi.CheckpointArtifact
}

// RegisterComputerCheckpoint pins a complete immutable candidate before upload.
// The caller owns commit/rollback. Nothing here claims remote object existence,
// moves the Computer head, or grants a restored Run execution authority.
func RegisterComputerCheckpoint(ctx context.Context, tx pgx.Tx, worker ComputerCaptureWorker, request workerapi.RegisterCheckpointRequest) (db.ComputerCheckpoint, error) {
	source, err := lockComputerCheckpointSource(ctx, tx, worker, computerCheckpointFence{request.ComputerInstanceID, request.WorkerEpoch, request.DesiredVersion, request.CheckpointID})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	return registerComputerCheckpoint(ctx, tx, worker, request, source)
}

func registerComputerCheckpoint(ctx context.Context, tx pgx.Tx, worker ComputerCaptureWorker, request workerapi.RegisterCheckpointRequest, source computerCheckpointSource) (db.ComputerCheckpoint, error) {
	var err error
	q := db.New(tx)
	instance, cp := source.instance, source.checkpoint
	environmentID, computerID := instance.EnvironmentID, instance.ComputerID
	validateLive := func() error {
		_, err := q.GetComputerInstanceCaptureCheckpoint(ctx, db.GetComputerInstanceCaptureCheckpointParams{ComputerInstanceID: instance.ID, EnvironmentID: environmentID, WorkerGroupID: worker.GroupID, WorkerHostID: worker.HostID, WorkerEpoch: worker.Epoch, DesiredVersion: request.DesiredVersion, WorkerFreshnessSeconds: workergroup.ObservationFreshnessSeconds})
		return err
	}
	if err = validateLive(); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	members, err := q.ListComputerCheckpointRuns(ctx, db.ListComputerCheckpointRunsParams{EnvironmentID: environmentID, CheckpointID: cp.ID})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	platform, err := q.GetVMPlatformForCheckpoint(ctx, instance.VMPlatformID)
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	manifest := request.Manifest
	point := manifest.RecoveryPoint
	if point.ID != request.CheckpointID || point.ComputerID != pgvalue.UUIDString(computerID) || point.ComputerInstanceID != request.ComputerInstanceID || point.WriterGeneration != cp.WriterGeneration || point.MembershipRevision != cp.MembershipRevision || point.ComputerSpecID != pgvalue.UUIDString(cp.ComputerSpecID) || point.ProgramDeploymentID != pgvalue.UUIDString(cp.ProgramDeploymentID) || len(point.Runs) != len(members) || point.Runs == nil {
		return db.ComputerCheckpoint{}, ErrCheckpointCandidate
	}
	identity := point.Runtime
	if identity.Backend != "firecracker" || identity.ID != platform.ID || identity.Arch != platform.Arch || identity.Contract != platform.Contract || identity.KernelDigest != platform.KernelDigest || identity.InitramfsDigest != platform.InitramfsDigest || identity.RootfsDigest != platform.RootfsDigest || !sha256sum.ValidDigest(identity.ConfigDigest) || identity.VMVCPUCount != instance.VMVCPUCount || identity.CPUConfigDigest != instance.CPUConfigDigest {
		return db.ComputerCheckpoint{}, ErrCheckpointCandidate
	}
	disk := manifest.RuntimeState.Computer
	if disk == nil || disk.ComputerID != point.ComputerID || disk.LogicalBytes != instance.ReservedGuestEphemeralDiskBytes || disk.Root.Validate(disk.LogicalBytes) != nil || manifest.ComputerState.Base.MountPath != "/workspace" {
		return db.ComputerCheckpoint{}, ErrCheckpointCandidate
	}
	byRun := make(map[string]workerapi.CheckpointRun, len(point.Runs))
	for _, member := range point.Runs {
		if _, exists := byRun[member.RunID]; exists || strings.TrimSpace(member.CorrelationID) == "" {
			return db.ComputerCheckpoint{}, ErrCheckpointCandidate
		}
		byRun[member.RunID] = member
	}
	manifest.RecoveryPoint.Runs = make([]workerapi.CheckpointRun, 0, len(members))
	for _, member := range members {
		supplied, exists := byRun[pgvalue.UUIDString(member.RunID)]
		if !exists || supplied.AttemptNumber != member.AttemptNumber || supplied.RunLeaseID != pgvalue.UUIDString(member.SourceRunLeaseID) || supplied.RunWaitID != pgvalue.UUIDString(member.RunWaitID) || (supplied.ActorSpeculativeInputSequence != nil) != member.ActorSpeculativeInputSequence.Valid || (supplied.ActorSpeculativeInputSequence != nil && *supplied.ActorSpeculativeInputSequence != member.ActorSpeculativeInputSequence.Int64) {
			return db.ComputerCheckpoint{}, ErrCheckpointCandidate
		}
		manifest.RecoveryPoint.Runs = append(manifest.RecoveryPoint.Runs, supplied)
	}
	validateMembers := func() error { return validateComputerCheckpointMembers(ctx, tx, instance, cp, len(members)) }
	if err = validateMembers(); err != nil {
		return db.ComputerCheckpoint{}, err
	}

	if len(manifest.RuntimeState.MemoryArtifacts) != 1 || !json.Valid(manifest.RuntimeState.Config) {
		return db.ComputerCheckpoint{}, ErrCheckpointCandidate
	}
	objects := []checkpointObject{{"vm_config", cas.CheckpointVMConfigMediaType, manifest.RuntimeState.ConfigArtifact}, {"vm_state", cas.CheckpointVMStateMediaType, manifest.RuntimeState.VMStateArtifact}, {"scratch_disk", cas.CheckpointScratchDiskMediaType, manifest.RuntimeState.ScratchDiskArtifact}, {"memory", cas.CheckpointMemoryMediaType, manifest.RuntimeState.MemoryArtifacts[0]}}
	seen := map[string]bool{}
	for _, object := range objects {
		a := object.artifact
		if !sha256sum.ValidDigest(a.Digest) || a.SizeBytes <= 0 || a.MediaType != object.media || seen[a.Digest] {
			return db.ComputerCheckpoint{}, ErrCheckpointCandidate
		}
		seen[a.Digest] = true
	}
	manifest.Phases = nil
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return db.ComputerCheckpoint{}, fmt.Errorf("%w: %v", ErrCheckpointCandidate, err)
	}
	encoded, err = jsoncanon.Transform(encoded)
	if err != nil || len(encoded) > 65536 {
		return db.ComputerCheckpoint{}, ErrCheckpointCandidate
	}
	n, err := q.RegisterCheckpointManifest(ctx, db.RegisterCheckpointManifestParams{ID: cp.ID, Manifest: encoded})
	if err != nil {
		return db.ComputerCheckpoint{}, err
	}
	if n != 1 {
		return db.ComputerCheckpoint{}, pgx.ErrNoRows
	}
	for _, object := range objects {
		a := object.artifact
		if _, err = q.RegisterCheckpointObject(ctx, db.RegisterCheckpointObjectParams{CheckpointID: cp.ID, Role: object.role, Digest: a.Digest, SizeBytes: a.SizeBytes, MediaType: a.MediaType}); err != nil {
			return db.ComputerCheckpoint{}, err
		}
	}
	if err = validateLive(); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	if err = validateMembers(); err != nil {
		return db.ComputerCheckpoint{}, err
	}
	cp.Manifest = encoded
	return cp, nil
}

type computerCheckpointFence struct {
	ComputerInstanceID string
	WorkerEpoch        int64
	DesiredVersion     int64
	CheckpointID       string
}

type computerCheckpointSource struct {
	computer   db.Computer
	instance   db.ComputerInstance
	checkpoint db.ComputerCheckpoint
}

func lockComputerCheckpointSource(ctx context.Context, tx pgx.Tx, worker ComputerCaptureWorker, request computerCheckpointFence) (computerCheckpointSource, error) {
	q := db.New(tx)
	var environmentID, computerID, orgID, instanceID pgtype.UUID
	var region string
	if request.WorkerEpoch != worker.Epoch || worker.Epoch <= 0 || request.DesiredVersion <= 0 {
		return computerCheckpointSource{}, pgx.ErrNoRows
	}
	if err := tx.QueryRow(ctx, `SELECT environment_id,computer_id,org_id,id,region_id FROM computer_instances
 WHERE id=$1 AND worker_group_id=$2 AND worker_host_id=$3 AND worker_epoch=$4`, request.ComputerInstanceID, worker.GroupID, worker.HostID, worker.Epoch).Scan(&environmentID, &computerID, &orgID, &instanceID, &region); err != nil {
		return computerCheckpointSource{}, err
	}
	group, err := q.LockRunLeaseClaimWorkerGroup(ctx, db.LockRunLeaseClaimWorkerGroupParams{ID: worker.GroupID, RegionID: region})
	if err != nil {
		return computerCheckpointSource{}, err
	}
	if group.Status != "active" && group.Status != "paused" && group.Status != "draining" {
		return computerCheckpointSource{}, pgx.ErrNoRows
	}
	host, err := q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{ID: worker.HostID, WorkerGroupID: worker.GroupID})
	if err != nil {
		return computerCheckpointSource{}, err
	}
	if !host.CurrentEpoch.Valid || host.CurrentEpoch.Int64 != worker.Epoch || (host.Status != "active" && host.Status != "draining") {
		return computerCheckpointSource{}, pgx.ErrNoRows
	}
	computer, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: environmentID, ID: computerID})
	if err != nil {
		return computerCheckpointSource{}, err
	}
	instance, err := q.LockWorkerComputerInstance(ctx, db.LockWorkerComputerInstanceParams{ID: instanceID, OrgID: orgID, WorkerHostID: worker.HostID, WorkerGroupID: worker.GroupID, WorkerEpoch: worker.Epoch})
	if err != nil {
		return computerCheckpointSource{}, err
	}
	if pgvalue.UUIDString(instance.ID) != request.ComputerInstanceID {
		return computerCheckpointSource{}, pgx.ErrNoRows
	}
	for _, query := range []string{
		`SELECT s.id FROM sessions s JOIN runs r ON r.session_id=s.id JOIN run_leases l ON l.run_id=r.id WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL ORDER BY s.id FOR UPDATE OF s`,
		`SELECT r.id FROM runs r JOIN run_leases l ON l.run_id=r.id WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL ORDER BY r.id FOR UPDATE OF r`,
		`SELECT a.run_id FROM run_attempts a JOIN run_leases l ON l.run_id=a.run_id AND l.attempt_number=a.number WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL ORDER BY a.run_id,a.number FOR UPDATE OF a`,
		`SELECT id FROM run_leases WHERE computer_instance_id=$1 AND process_reconciled_at IS NULL ORDER BY run_id,id FOR UPDATE`,
		`SELECT w.id FROM run_waits w JOIN run_leases l ON l.run_id=w.run_id AND l.attempt_number=w.attempt_number WHERE l.computer_instance_id=$1 AND l.process_reconciled_at IS NULL ORDER BY w.run_id,w.id FOR UPDATE OF w`,
	} {
		if _, err = tx.Exec(ctx, query, instance.ID); err != nil {
			return computerCheckpointSource{}, err
		}
	}
	cp, err := q.LockComputerCheckpoint(ctx, db.LockComputerCheckpointParams{EnvironmentID: environmentID, ComputerID: computerID, CheckpointID: instance.CaptureCheckpointID})
	if err != nil {
		return computerCheckpointSource{}, err
	}
	if pgvalue.UUIDString(cp.ID) != request.CheckpointID {
		return computerCheckpointSource{}, pgx.ErrNoRows
	}
	return computerCheckpointSource{computer: computer, instance: instance, checkpoint: cp}, nil
}

// Caller holds the source and all resident member locks.
func validateComputerCheckpointMembers(ctx context.Context, tx pgx.Tx, instance db.ComputerInstance, cp db.ComputerCheckpoint, memberCount int) error {
	var live bool
	err := tx.QueryRow(ctx, `SELECT
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
