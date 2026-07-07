package control

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func checkpointSubstrateDigest(manifest api.WorkerCheckpointManifest) pgtype.Text {
	if manifest.RecoveryPoint.Runtime.Substrate == nil {
		return pgtype.Text{}
	}
	return pgvalue.Text(strings.TrimSpace(manifest.RecoveryPoint.Runtime.Substrate.Digest))
}

func checkpointRuntimeSubstrateID(manifest api.WorkerCheckpointManifest) (pgtype.UUID, error) {
	if manifest.RecoveryPoint.Runtime.Substrate == nil {
		return pgtype.UUID{}, nil
	}
	if manifest.RuntimeState.RuntimeSubstrate == nil {
		return pgtype.UUID{}, errors.New("runtime_state.runtime_substrate is required")
	}
	id, err := uuid.Parse(strings.TrimSpace(manifest.RuntimeState.RuntimeSubstrate.ID))
	if err != nil {
		return pgtype.UUID{}, errors.New("runtime_state.runtime_substrate.id must be a UUID")
	}
	return pgvalue.UUID(id), nil
}

func validateWorkerRuntimeSubstrate(label string, artifact api.WorkerRuntimeSubstrate, substrate api.WorkerCheckpointRuntimeSubstrate) error {
	required := map[string]string{
		label + ".id":                    artifact.ID,
		label + ".deployment_sandbox_id": artifact.DeploymentSandboxID,
		label + ".artifact.digest":       artifact.Artifact.Digest,
		label + ".artifact.media_type":   artifact.Artifact.MediaType,
		label + ".substrate_digest":      artifact.SubstrateDigest,
		label + ".format":                artifact.Format,
		label + ".builder_abi":           artifact.BuilderABI,
		label + ".layout_abi":            artifact.LayoutABI,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", field)
		}
	}
	if strings.TrimSpace(artifact.Artifact.MediaType) != cas.RuntimeSubstrateMediaType {
		return fmt.Errorf("%s.artifact.media_type must be %s", label, cas.RuntimeSubstrateMediaType)
	}
	if artifact.Artifact.SizeBytes < 0 {
		return fmt.Errorf("%s.artifact.size_bytes must be non-negative", label)
	}
	if artifact.SizeBytes < 0 {
		return fmt.Errorf("%s.size_bytes must be non-negative", label)
	}
	if strings.TrimSpace(artifact.SubstrateDigest) != strings.TrimSpace(substrate.Digest) {
		return fmt.Errorf("%s.substrate_digest must match runtime.substrate.digest", label)
	}
	if strings.TrimSpace(artifact.Format) != strings.TrimSpace(substrate.Format) {
		return fmt.Errorf("%s.format must match runtime.substrate.format", label)
	}
	if strings.TrimSpace(artifact.BuilderABI) != strings.TrimSpace(substrate.BuilderABI) {
		return fmt.Errorf("%s.builder_abi must match runtime.substrate.builder_abi", label)
	}
	if strings.TrimSpace(artifact.LayoutABI) != strings.TrimSpace(substrate.LayoutABI) {
		return fmt.Errorf("%s.layout_abi must match runtime.substrate.layout_abi", label)
	}
	return nil
}

func validateWorkerCheckpointArtifact(label string, artifact api.WorkerCheckpointArtifact) error {
	if strings.TrimSpace(artifact.Digest) == "" {
		return fmt.Errorf("%s.digest is required", label)
	}
	if strings.TrimSpace(artifact.MediaType) == "" {
		return fmt.Errorf("%s.media_type is required", label)
	}
	if artifact.SizeBytes < 0 {
		return fmt.Errorf("%s.size_bytes must be non-negative", label)
	}
	return nil
}

type runCheckpointArtifactIDs struct {
	config      pgtype.UUID
	vmState     pgtype.UUID
	scratchDisk pgtype.UUID
	memory      []pgtype.UUID
}

func (s *Server) createRunCheckpointArtifacts(ctx context.Context, store db.Querier, workerInstanceID pgtype.UUID, scope db.GetWorkerRunWaitScopeRow, manifest api.WorkerCheckpointManifest) (runCheckpointArtifactIDs, error) {
	config, err := createRunCheckpointArtifact(ctx, store, workerInstanceID, scope, manifest.RuntimeState.ConfigArtifact, db.ArtifactKindRunCheckpointConfig)
	if err != nil {
		return runCheckpointArtifactIDs{}, err
	}
	vmState, err := createRunCheckpointArtifact(ctx, store, workerInstanceID, scope, manifest.RuntimeState.VMStateArtifact, db.ArtifactKindRunCheckpointVmState)
	if err != nil {
		return runCheckpointArtifactIDs{}, err
	}
	scratchDisk, err := createRunCheckpointArtifact(ctx, store, workerInstanceID, scope, manifest.RuntimeState.ScratchDiskArtifact, db.ArtifactKindRunCheckpointScratchDisk)
	if err != nil {
		return runCheckpointArtifactIDs{}, err
	}
	memory := make([]pgtype.UUID, 0, len(manifest.RuntimeState.MemoryArtifacts))
	for _, artifact := range manifest.RuntimeState.MemoryArtifacts {
		row, err := createRunCheckpointArtifact(ctx, store, workerInstanceID, scope, artifact, db.ArtifactKindRunCheckpointMemory)
		if err != nil {
			return runCheckpointArtifactIDs{}, err
		}
		memory = append(memory, row.ID)
	}
	return runCheckpointArtifactIDs{config: config.ID, vmState: vmState.ID, scratchDisk: scratchDisk.ID, memory: memory}, nil
}

func createRunCheckpointArtifact(ctx context.Context, store db.Querier, workerInstanceID pgtype.UUID, scope db.GetWorkerRunWaitScopeRow, artifact api.WorkerCheckpointArtifact, kind db.ArtifactKind) (db.Artifact, error) {
	if _, err := store.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		OrgID:     scope.OrgID,
		Digest:    artifact.Digest,
		SizeBytes: artifact.SizeBytes,
		MediaType: artifact.MediaType,
	}); err != nil {
		return db.Artifact{}, err
	}
	return store.CreateArtifact(ctx, db.CreateArtifactParams{
		ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                     scope.OrgID,
		ProjectID:                 scope.ProjectID,
		EnvironmentID:             scope.EnvironmentID,
		Digest:                    artifact.Digest,
		Kind:                      kind,
		SizeBytes:                 artifact.SizeBytes,
		MediaType:                 artifact.MediaType,
		CreatedByWorkerInstanceID: workerInstanceID,
	})
}

func (s *Server) createRunCheckpointArtifactRows(ctx context.Context, store db.Querier, scope db.GetWorkerRunWaitScopeRow, runCheckpointID pgtype.UUID, manifest api.WorkerCheckpointManifest, artifacts runCheckpointArtifactIDs) error {
	rows := []struct {
		role     db.RunCheckpointArtifactRole
		ordinal  int32
		id       pgtype.UUID
		artifact api.WorkerCheckpointArtifact
	}{
		{role: db.RunCheckpointArtifactRoleRuntimeConfig, id: artifacts.config, artifact: manifest.RuntimeState.ConfigArtifact},
		{role: db.RunCheckpointArtifactRoleVmState, id: artifacts.vmState, artifact: manifest.RuntimeState.VMStateArtifact},
		{role: db.RunCheckpointArtifactRoleScratchDisk, id: artifacts.scratchDisk, artifact: manifest.RuntimeState.ScratchDiskArtifact},
	}
	for index, artifact := range manifest.RuntimeState.MemoryArtifacts {
		rows = append(rows, struct {
			role     db.RunCheckpointArtifactRole
			ordinal  int32
			id       pgtype.UUID
			artifact api.WorkerCheckpointArtifact
		}{role: db.RunCheckpointArtifactRoleMemory, ordinal: int32(index), id: artifacts.memory[index], artifact: artifact})
	}
	for _, row := range rows {
		if _, err := store.CreateRunCheckpointArtifact(ctx, db.CreateRunCheckpointArtifactParams{
			Role:              row.role,
			Ordinal:           row.ordinal,
			EncryptDurationMs: row.artifact.EncryptDurationMs,
			StoreDurationMs:   row.artifact.StoreDurationMs,
			ArtifactID:        row.id,
			Digest:            row.artifact.Digest,
			OrgID:             scope.OrgID,
			ProjectID:         scope.ProjectID,
			EnvironmentID:     scope.EnvironmentID,
			RunID:             scope.RunID,
			RunCheckpointID:   runCheckpointID,
		}); err != nil {
			return err
		}
	}
	return nil
}
