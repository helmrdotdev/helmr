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

func projectRuntimeComputerSource(row db.ListComputerInstanceReconcileTargetsRow) (workerapi.RuntimeComputerSource, error) {
	var source workerapi.RuntimeComputerSource
	if !row.PreparationDiskVersionID.Valid || !row.ComputerDiskVersionStatus.Valid {
		return source, errors.New("computer instance has no exact computer version")
	}
	if (row.SourceDiskVersionID.Valid && row.SourceDiskVersionID != row.PreparationDiskVersionID) || (row.SourceCheckpointID.Valid && !row.SourceDiskVersionID.Valid) {
		return source, errors.New("computer instance source does not match its retained disk")
	}
	if row.ComputerArchitecture != string(deployment.ArchitectureX8664) || row.ReservedGuestEphemeralDiskBytes != computer.SeedCapacity {
		return source, errors.New("computer instance has an unsupported computer capacity or architecture")
	}
	source.VersionID = pgvalue.UUIDString(row.PreparationDiskVersionID)
	source.LogicalBytes = row.ReservedGuestEphemeralDiskBytes
	switch row.ComputerDiskVersionStatus.String {
	case "initializing":
		if len(row.ComputerGenerationLocator) != 0 || row.SourceCheckpointID.Valid || row.ComputerContentDigest.Valid ||
			!row.ComputerLogicalSizeBytes.Valid || row.ComputerLogicalSizeBytes.Int64 != 0 ||
			len(row.ComputerInitialConfig) != 0 {
			return source, errors.New("initializing computer has persisted disk or continuation state")
		}
		object := cas.Descriptor{Digest: row.ComputerImageDigest, SizeBytes: row.ComputerImageSizeBytes, MediaType: row.ComputerImageMediaType}
		spec, err := deployment.ParseComputerSpec(row.ComputerConfig, object)
		if err != nil {
			return source, fmt.Errorf("project computer seed: %w", err)
		}
		if !bytes.Equal(spec.Digest[:], row.ComputerSpecDigest) {
			return source, errors.New("computer seed does not match admitted specification")
		}
		var config deployment.ComputerConfig
		if err := json.Unmarshal(spec.Config, &config); err != nil {
			return source, err
		}
		if err := (computer.SeedArtifact{Object: object, LogicalBytes: source.LogicalBytes}).Validate(source.LogicalBytes); err != nil {
			return source, fmt.Errorf("project computer seed: %w", err)
		}
		source.Config = config.Image
		source.Seed = &workerapi.ComputerSeed{Profile: config.Profile, Object: workerapi.CASObject{
			Digest: object.Digest, SizeBytes: object.SizeBytes, MediaType: object.MediaType,
		}}
	case "committed", "private":
		root, err := computer.ParseGenerationRoot(row.ComputerGenerationLocator, source.LogicalBytes)
		if err != nil {
			return source, fmt.Errorf("project computer generation: %w", err)
		}
		if !row.ComputerLogicalSizeBytes.Valid || row.ComputerLogicalSizeBytes.Int64 != source.LogicalBytes || !row.ComputerContentDigest.Valid || row.ComputerContentDigest.String != root.Pack.Digest {
			return source, errors.New("computer generation identity is incomplete")
		}
		source.Root = &root
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
