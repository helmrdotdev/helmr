package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
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

func registerComputerCheckpoint(ctx context.Context, tx pgx.Tx, worker ComputerCaptureWorker, request workerapi.RegisterCheckpointRequest, source computer.CheckpointSource) (db.ComputerCheckpoint, error) {
	var err error
	q := db.New(tx)
	instance, cp := source.Instance, source.Checkpoint
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
	validateMembers := func() error { return computer.CheckCheckpointMembers(ctx, tx, instance, cp, len(members)) }
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

// lockComputerCheckpointSource locks the capture source a worker request
// names through the computer owner's checkpoint source fence.
func lockComputerCheckpointSource(ctx context.Context, tx pgx.Tx, worker ComputerCaptureWorker, request computerCheckpointFence) (computer.CheckpointSource, error) {
	instanceID, instanceErr := uuid.Parse(request.ComputerInstanceID)
	checkpointID, checkpointErr := uuid.Parse(request.CheckpointID)
	if instanceErr != nil || checkpointErr != nil || instanceID.String() != request.ComputerInstanceID || checkpointID.String() != request.CheckpointID || !worker.GroupID.Valid || !worker.HostID.Valid {
		return computer.CheckpointSource{}, pgx.ErrNoRows
	}
	return computer.LockCheckpointSource(ctx, tx, computer.CheckpointRef{
		Host:       computer.Host{GroupID: pgvalue.MustUUIDValue(worker.GroupID), HostID: pgvalue.MustUUIDValue(worker.HostID), Epoch: worker.Epoch},
		InstanceID: instanceID, WorkerEpoch: request.WorkerEpoch, DesiredVersion: request.DesiredVersion, CheckpointID: checkpointID,
	})
}
