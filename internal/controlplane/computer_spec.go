package controlplane

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

// The finalization transaction owns both immutable spec registration and the
// deployment declaration that references it. Reuse never changes launch content.
func registerDeploymentComputerSpecs(
	ctx context.Context,
	queries db.Querier,
	environmentID pgtype.UUID,
	definitions []finalizedDeploymentDefinition,
	artifacts map[string]db.Artifact,
) (map[string]db.ComputerSpec, error) {
	specs := make(map[string]db.ComputerSpec)
	ordered := make([]finalizedDeploymentDefinition, 0, len(definitions))
	for _, finalized := range definitions {
		if finalized.computerSpec != nil {
			ordered = append(ordered, finalized)
		}
	}
	// Deployments can name the same specs in different orders. Acquire their
	// unique-key locks in content order to avoid a cross-deployment deadlock.
	slices.SortFunc(ordered, func(left, right finalizedDeploymentDefinition) int {
		return bytes.Compare(left.computerSpec.Digest[:], right.computerSpec.Digest[:])
	})
	for _, finalized := range ordered {
		spec := finalized.computerSpec
		artifact, ok := artifacts[spec.Seed.Digest]
		if !ok {
			return nil, fmt.Errorf("computer seed %q is not registered", spec.Seed.Digest)
		}
		stored, err := queries.RegisterComputerSpec(ctx, db.RegisterComputerSpecParams{
			ID: pgvalue.UUID(uuid.NewV7()), EnvironmentID: environmentID,
			Config: spec.Config, Digest: spec.Digest[:], SeedArtifactID: artifact.ID,
			SeedDigest: spec.Seed.Digest, SeedSizeBytes: spec.Seed.SizeBytes, SeedMediaType: spec.Seed.MediaType,
		})
		if err != nil {
			return nil, fmt.Errorf("register computer spec: %w", err)
		}
		roundTrip, err := definition.ParseComputerSpec(stored.Config, cas.Descriptor{
			Digest: stored.SeedDigest, SizeBytes: stored.SeedSizeBytes, MediaType: stored.SeedMediaType,
		})
		if err != nil {
			return nil, fmt.Errorf("read registered computer spec: %w", err)
		}
		if roundTrip.Digest != spec.Digest || !bytes.Equal(roundTrip.Digest[:], stored.Digest) ||
			!bytes.Equal(roundTrip.Config, spec.Config) {
			return nil, fmt.Errorf("registered computer spec has different canonical content")
		}
		specs[finalized.declaredID] = stored
	}
	return specs, nil
}
