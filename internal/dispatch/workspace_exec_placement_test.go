package dispatch

import (
	"testing"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestValidateWorkspaceExecRuntimeIgnoresHistoricalRestoreProvenance(t *testing.T) {
	definitionID := pgvalue.UUID(uuid.NewV7())
	authority := workspaceExecAuthority{
		workspaceDefinitionID: definitionID,
		resources: runResources{
			cpuMillis: 1000, memoryBytes: 2 << 30,
			guestEphemeralDiskBytes: 8 << 30, executionSlots: 1,
		},
	}
	runtime := runRuntime{
		deploymentDefinition:    definitionID,
		restoreCheckpoint:       pgvalue.UUID(uuid.NewV7()),
		cpuMillis:               1000,
		memoryBytes:             2 << 30,
		guestEphemeralDiskBytes: 8 << 30,
		executionSlots:          1,
	}
	if err := validateWorkspaceExecRuntime(authority, runtime); err != nil {
		t.Fatalf("workspace exec rejected historical restore provenance: %v", err)
	}
}
