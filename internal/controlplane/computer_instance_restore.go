package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func populateRuntimeRestoreSource(ctx context.Context, store db.Querier, source *workerapi.RuntimeSource, row db.ListComputerInstanceReconcileTargetsRow) error {
	if store == nil || source == nil || row.AdmissionState != "restoring" || !row.SourceCheckpointID.Valid {
		return errors.New("computer restore destination is incomplete")
	}
	authority, err := store.GetComputerInstanceRestoreCheckpoint(ctx, db.GetComputerInstanceRestoreCheckpointParams{
		ComputerInstanceID: row.ID, EnvironmentID: row.EnvironmentID, WorkerGroupID: row.WorkerGroupID,
		WorkerHostID: row.WorkerHostID, WorkerEpoch: row.WorkerEpoch, DesiredVersion: row.DesiredVersion,
	})
	if err != nil {
		return fmt.Errorf("load computer restore checkpoint: %w", err)
	}
	cp := authority.ComputerCheckpoint
	if cp.ID != row.SourceCheckpointID || cp.ComputerID != row.ComputerID || cp.ComputerSpecID != row.ComputerSpecID ||
		cp.ProgramDeploymentID != row.ProgramDeploymentID || cp.PrivateComputerDiskVersionID != row.PreparationDiskVersionID {
		return errors.New("computer restore source changed during discovery")
	}
	members, err := store.ListComputerCheckpointRuns(ctx, db.ListComputerCheckpointRunsParams{EnvironmentID: row.EnvironmentID, CheckpointID: cp.ID})
	if err != nil {
		return fmt.Errorf("load computer restore membership: %w", err)
	}
	restore, err := projectComputerInstanceRestore(authority, members)
	if err != nil {
		return err
	}
	source.Restore = &restore
	return nil
}

func projectComputerInstanceRestore(authority db.GetComputerInstanceRestoreCheckpointRow, members []db.ComputerCheckpointRun) (workerapi.RuntimeRestore, error) {
	cp := authority.ComputerCheckpoint
	var manifest workerapi.CheckpointManifest
	if cp.Status != "ready" || json.Unmarshal(cp.Manifest, &manifest) != nil {
		return workerapi.RuntimeRestore{}, errors.New("computer checkpoint manifest is invalid")
	}
	point := manifest.RecoveryPoint
	if point.ID != pgvalue.UUIDString(cp.ID) || point.ComputerID != pgvalue.UUIDString(cp.ComputerID) ||
		point.ComputerInstanceID != pgvalue.UUIDString(cp.SourceComputerInstanceID) || point.WriterGeneration != cp.WriterGeneration ||
		point.MembershipRevision != cp.MembershipRevision || point.ComputerSpecID != pgvalue.UUIDString(cp.ComputerSpecID) ||
		point.ProgramDeploymentID != pgvalue.UUIDString(cp.ProgramDeploymentID) {
		return workerapi.RuntimeRestore{}, errors.New("computer checkpoint manifest does not match captured identity")
	}
	if len(point.Runs) > 0 && !cp.ProgramDeploymentID.Valid {
		return workerapi.RuntimeRestore{}, errors.New("captured Runs require a Program")
	}
	if len(point.Runs) != len(members) {
		return workerapi.RuntimeRestore{}, errors.New("computer checkpoint manifest membership is incomplete")
	}
	expected := make(map[string]db.ComputerCheckpointRun, len(members))
	for _, member := range members {
		id := pgvalue.UUIDString(member.RunID)
		if _, duplicate := expected[id]; duplicate || !member.RunID.Valid || member.CheckpointID != cp.ID || member.EnvironmentID != cp.EnvironmentID || member.ComputerID != cp.ComputerID || member.SourceComputerInstanceID != cp.SourceComputerInstanceID || member.WriterGeneration != cp.WriterGeneration {
			return workerapi.RuntimeRestore{}, errors.New("computer checkpoint member has inconsistent authority")
		}
		expected[id] = member
	}
	for _, captured := range point.Runs {
		member, ok := expected[captured.RunID]
		if !ok || captured.AttemptNumber != member.AttemptNumber || captured.RunWaitID != pgvalue.UUIDString(member.RunWaitID) || captured.RunLeaseID != pgvalue.UUIDString(member.SourceRunLeaseID) || strings.TrimSpace(captured.CorrelationID) == "" ||
			(captured.ActorSpeculativeInputSequence != nil) != member.ActorSpeculativeInputSequence.Valid {
			return workerapi.RuntimeRestore{}, errors.New("computer checkpoint manifest contains an unexpected member")
		}
		if captured.ActorSpeculativeInputSequence != nil && *captured.ActorSpeculativeInputSequence != member.ActorSpeculativeInputSequence.Int64 {
			return workerapi.RuntimeRestore{}, errors.New("computer checkpoint Actor cursor does not match captured authority")
		}
		delete(expected, captured.RunID)
	}
	if len(manifest.RuntimeState.MemoryArtifacts) != 1 {
		return workerapi.RuntimeRestore{}, errors.New("computer checkpoint requires one memory artifact")
	}
	objects := []struct {
		role, digest string
		size         int64
		media        string
		captured     workerapi.CheckpointArtifact
	}{
		{"vm_config", authority.VMConfigDigest, authority.VMConfigSizeBytes, authority.VMConfigMediaType, manifest.RuntimeState.ConfigArtifact},
		{"vm_state", authority.VMStateDigest, authority.VMStateSizeBytes, authority.VMStateMediaType, manifest.RuntimeState.VMStateArtifact},
		{"memory", authority.MemoryDigest, authority.MemorySizeBytes, authority.MemoryMediaType, manifest.RuntimeState.MemoryArtifacts[0]},
		{"scratch_disk", authority.ScratchDiskDigest, authority.ScratchDiskSizeBytes, authority.ScratchDiskMediaType, manifest.RuntimeState.ScratchDiskArtifact},
	}
	restore := workerapi.RuntimeRestore{CheckpointID: point.ID, Manifest: append(json.RawMessage(nil), cp.Manifest...)}
	for _, object := range objects {
		if err := cas.ValidateDescriptor(cas.Descriptor{Digest: object.digest, SizeBytes: object.size, MediaType: object.media}); err != nil {
			return workerapi.RuntimeRestore{}, fmt.Errorf("computer checkpoint %s: %w", object.role, err)
		}
		if object.captured.Digest != object.digest || object.captured.SizeBytes != object.size || object.captured.MediaType != object.media {
			return workerapi.RuntimeRestore{}, errors.New("computer checkpoint artifact differs from retained descriptor")
		}
		restore.Artifacts = append(restore.Artifacts, workerapi.RunLeaseCheckpointArtifact{Role: object.role, Object: workerapi.CASObject{Digest: object.digest, SizeBytes: object.size, MediaType: object.media}})
	}
	return restore, nil
}
