package bundle

import (
	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"testing"
)

func TestDeploymentPlanFromProgramMetadata(t *testing.T) {
	index := artifacttest.ProgramMetadata(t)
	index.Definitions[0].Agent.MaxTurnDurationMs = new(int64(60000))
	plan, err := PlanFromProgramMetadata(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProgramMetadataDeployment(index, plan); err != nil {
		t.Fatal(err)
	}
	plan.Definitions[0].DeclaredID = "changed"
	*plan.Definitions[0].Agent.MaxTurnDurationMs = 1
	plan.Definitions[0].Agent.Triggers["daily"].Input[0] = 'x'
	if index.Definitions[0].DeclaredID == "changed" || *index.Definitions[0].Agent.MaxTurnDurationMs != 60000 || string(index.Definitions[0].Agent.Triggers["daily"].Input) != "[]" {
		t.Fatal("deployment plan aliases Program index")
	}
}
func TestDeploymentPlanFromProgramMetadataRejectsInvalidIndex(t *testing.T) {
	index := artifacttest.ProgramMetadata(t)
	index.RuntimeContract = "helmr.runtime.unsupported"
	if _, err := PlanFromProgramMetadata(index); err == nil {
		t.Fatal("accepted invalid index")
	}
}
func TestDeploymentPlanSupportsStandaloneComputer(t *testing.T) {
	index := artifacttest.ProgramMetadata(t)
	index.Definitions = []artifact.ProgramDefinition{index.Definitions[2]}
	plan, err := PlanFromProgramMetadata(index)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Definitions) != 1 || plan.Definitions[0].Kind != definition.KindComputer {
		t.Fatal(plan)
	}
}
