package controlplane

import (
	"context"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCheckpointSubstrateAuthorityMatchesRelationalIdentity(t *testing.T) {
	orgID := pgvalue.UUID(uuid.NewV7())
	projectID := pgvalue.UUID(uuid.NewV7())
	environmentID := pgvalue.UUID(uuid.NewV7())
	definitionID := pgvalue.UUID(uuid.NewV7())
	substrateID := pgvalue.UUID(uuid.NewV7())
	authority := runLeaseClaimAuthority{
		run: db.Run{
			OrgID:         orgID,
			ProjectID:     projectID,
			EnvironmentID: environmentID,
		},
		runtime: db.RuntimeInstance{
			DeploymentDefinitionID: definitionID,
			RuntimeSubstrateID:     substrateID,
		},
	}
	row := db.RuntimeSubstrate{
		ID:                     substrateID,
		OrgID:                  orgID,
		ProjectID:              projectID,
		EnvironmentID:          environmentID,
		DeploymentDefinitionID: definitionID,
		SubstrateDigest:        "sha256:substrate",
		SubstrateFormat:        "ext4",
		SubstrateContract:      "builder-v0",
		SubstrateSizeBytes:     4096,
	}
	manifest := workerapi.CheckpointManifest{
		RecoveryPoint: workerapi.CheckpointRecoveryPoint{
			Runtime: workerapi.CheckpointRuntime{
				Substrate: &workerapi.CheckpointRuntimeSubstrate{
					Digest:    row.SubstrateDigest,
					Format:    row.SubstrateFormat,
					Contract:  row.SubstrateContract,
					SizeBytes: row.SubstrateSizeBytes,
				},
			},
		},
	}
	store := checkpointSubstrateStore{row: row}
	if err := validateCheckpointSubstrateAuthority(
		context.Background(),
		store,
		authority,
		manifest,
	); err != nil {
		t.Fatal(err)
	}

	mismatched := manifest
	mismatched.RecoveryPoint.Runtime.Substrate = &workerapi.CheckpointRuntimeSubstrate{
		Digest:    "sha256:different",
		Format:    row.SubstrateFormat,
		Contract:  row.SubstrateContract,
		SizeBytes: row.SubstrateSizeBytes,
	}
	if err := validateCheckpointSubstrateAuthority(
		context.Background(),
		store,
		authority,
		mismatched,
	); !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("mismatched identity error = %v, want stale", err)
	}

	crossTenant := store
	crossTenant.row.EnvironmentID = pgvalue.UUID(uuid.NewV7())
	if err := validateCheckpointSubstrateAuthority(
		context.Background(),
		crossTenant,
		authority,
		manifest,
	); !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("cross-tenant substrate error = %v, want stale", err)
	}

	authority.runtime.RuntimeSubstrateID = pgtype.UUID{}
	if err := validateCheckpointSubstrateAuthority(
		context.Background(),
		store,
		authority,
		workerapi.CheckpointManifest{},
	); err != nil {
		t.Fatalf("absent substrate authority error = %v", err)
	}
	if err := validateCheckpointSubstrateAuthority(
		context.Background(),
		store,
		authority,
		manifest,
	); !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("unexpected substrate identity error = %v, want stale", err)
	}
}

type checkpointSubstrateStore struct {
	row db.RuntimeSubstrate
	err error
}

func (s checkpointSubstrateStore) GetRuntimeSubstrateForCheckpoint(
	context.Context,
	pgtype.UUID,
) (db.RuntimeSubstrate, error) {
	return s.row, s.err
}
