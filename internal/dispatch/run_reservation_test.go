package dispatch

import (
	"testing"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestValidateRunRuntimeUsesCheckpointContractOnlyForRestore(t *testing.T) {
	workspaceDefinitionID := pgvalue.UUID(uuid.NewV7())
	deploymentID := pgvalue.UUID(uuid.NewV7())
	authority := runPlacementAuthority{
		workspaceDefinitionID: workspaceDefinitionID,
		deploymentID:          deploymentID,
		resources: runResources{
			cpuMillis: 1000, memoryBytes: 2 << 30,
			guestEphemeralDiskBytes: 8 << 30, executionSlots: 1,
		},
	}
	runtime := runRuntime{
		id:                      pgvalue.UUID(uuid.NewV7()),
		deploymentDefinition:    workspaceDefinitionID,
		programDeployment:       deploymentID,
		cpuMillis:               1000,
		memoryBytes:             2 << 30,
		guestEphemeralDiskBytes: 8 << 30,
		executionSlots:          1,
	}
	if err := validateRunRuntime(authority, runtime); err != nil {
		t.Fatalf("fresh runtime validation failed: %v", err)
	}
	runtime.restoreCheckpoint = pgvalue.UUID(uuid.NewV7())
	if err := validateRunRuntime(authority, runtime); err != nil {
		t.Fatalf("fresh runtime rejected historical restore provenance: %v", err)
	}

	authority.restoreCheckpointID = pgvalue.UUID(uuid.NewV7())
	authority.restoreRuntimeIdentityID = "sha256:source"
	authority.restoreSubstrateID = pgvalue.UUID(uuid.NewV7())
	if err := validateRunRuntime(authority, runtime); err == nil {
		t.Fatal("restore accepted a runtime without the checkpoint identity")
	}
	runtime.restoreCheckpoint = authority.restoreCheckpointID
	runtime.runtimeIdentityID = authority.restoreRuntimeIdentityID
	runtime.runtimeSubstrateID = authority.restoreSubstrateID
	if err := validateRunRuntime(authority, runtime); err != nil {
		t.Fatalf("compatible restore runtime validation failed: %v", err)
	}
}
