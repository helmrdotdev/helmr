package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func projectRuntimeComputerSource(row db.ListRuntimeReconcileTargetsRow) (workerapi.RuntimeComputerSource, error) {
	var source workerapi.RuntimeComputerSource
	if !row.BaseWorkspaceVersionID.Valid || !row.ComputerVersionStatus.Valid {
		return source, errors.New("runtime reservation has no exact computer version")
	}
	if row.WorkspaceArchitecture != string(deployment.ArchitectureX8664) || row.ReservedGuestEphemeralDiskBytes != computer.SeedCapacity {
		return source, errors.New("runtime reservation has an unsupported computer capacity or architecture")
	}
	source.VersionID = pgvalue.UUIDString(row.BaseWorkspaceVersionID)
	source.LogicalBytes = row.ReservedGuestEphemeralDiskBytes
	switch row.ComputerVersionStatus.String {
	case "initializing":
		if len(row.ComputerGenerationLocator) != 0 || row.RestoreCheckpointID.Valid || row.WorkspaceContentDigest.Valid ||
			!row.WorkspaceLogicalSizeBytes.Valid || row.WorkspaceLogicalSizeBytes.Int64 != 0 ||
			row.WorkspaceArtifactDigest != "" || row.WorkspaceArtifactSizeBytes != 0 || row.WorkspaceArtifactMediaType != "" ||
			len(row.ComputerInitialConfig) != 0 {
			return source, errors.New("initializing computer has persisted disk or continuation state")
		}
		manifest, err := deployment.ParseSandboxManifest(row.SandboxManifestVersion, row.SandboxManifest)
		if err != nil {
			return source, fmt.Errorf("project computer seed: %w", err)
		}
		object := cas.Descriptor{Digest: row.WorkspaceImageDigest, SizeBytes: row.WorkspaceImageSizeBytes, MediaType: row.WorkspaceImageMediaType}
		if manifest.Image.Profile != computer.SeedProfile || manifest.Image.ArtifactDigest != object.Digest || manifest.Image.MediaType != object.MediaType {
			return source, errors.New("computer seed does not match admitted deployment")
		}
		if err := (computer.SeedArtifact{Object: object, LogicalBytes: source.LogicalBytes}).Validate(source.LogicalBytes); err != nil {
			return source, fmt.Errorf("project computer seed: %w", err)
		}
		source.Config = manifest.Image.Config
		source.Seed = &workerapi.ComputerSeed{Profile: manifest.Image.Profile, Object: workerapi.CASObject{
			Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType,
		}}
	case "committed", "private":
		root, err := computer.ParseGenerationRoot(row.ComputerGenerationLocator, source.LogicalBytes)
		if err != nil {
			return source, fmt.Errorf("project computer generation: %w", err)
		}
		if !row.WorkspaceLogicalSizeBytes.Valid || row.WorkspaceLogicalSizeBytes.Int64 != source.LogicalBytes || !row.WorkspaceContentDigest.Valid || row.WorkspaceContentDigest.String != root.Pack.Digest {
			return source, errors.New("computer generation identity is incomplete")
		}
		// The original configuration belongs to the Computer, not the current
		// deployment. A new deployment must not silently change its user or env.
		raw := bytes.TrimSpace(row.ComputerInitialConfig)
		if len(raw) == 0 || raw[0] != '{' {
			return source, errors.New("computer has no published initial configuration")
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&source.Config); err != nil {
			return source, fmt.Errorf("decode computer initial configuration: %w", err)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return source, errors.New("computer initial configuration has trailing data")
		}
	default:
		return source, errors.New("computer version is not available for preparation")
	}
	return source, nil
}
