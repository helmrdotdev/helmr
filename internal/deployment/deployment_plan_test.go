package deployment

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestDeploymentPlanFromProgramIndex(t *testing.T) {
	index := testProgramIndex(t)
	plan, err := DeploymentPlanFromProgramIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProgramIndexDeployment(index, plan); err != nil {
		t.Fatal(err)
	}
	ttl := int64(60000)
	maxAttempts := int64(3)
	taskIndex := -1
	for position := range index.Declarations {
		if index.Declarations[position].Kind == DefinitionKindTask {
			taskIndex = position
			break
		}
	}
	if taskIndex < 0 {
		t.Fatal("test Program index has no task")
	}
	index.Declarations[taskIndex].Task.Run.TTLMs = &ttl
	index.Declarations[taskIndex].Task.Run.Retry = RetryManifest{
		Enabled: true, MaxAttempts: &maxAttempts,
		Backoff: &RetryBackoff{MinMs: 100, MaxMs: 1000, Factor: 2, Jitter: RetryJitterFull},
	}
	index.Declarations[taskIndex].Task.Schedule = &ScheduleManifest{
		Cron: "0 * * * *", Timezone: "UTC",
		Workspace: ScheduleWorkspaceManifest{
			SandboxDeclaredID: "repo",
			Secrets:           []api.WorkspaceSecret{{Name: "TOKEN", Env: "TOKEN"}},
		},
	}
	plan, err = DeploymentPlanFromProgramIndex(index)
	if err != nil {
		t.Fatal(err)
	}

	plan.Definitions[0].DeclaredID = "changed"
	if index.Declarations[0].DeclaredID == "changed" {
		t.Fatal("deployment plan aliases the Program index")
	}
	if len(plan.Queues) > 0 && plan.Queues[0].ConcurrencyLimit != nil {
		*plan.Queues[0].ConcurrencyLimit = 99
		if *index.Queues[0].ConcurrencyLimit == 99 {
			t.Fatal("deployment plan aliases queue configuration")
		}
	}
	*plan.Definitions[taskIndex].Task.Run.TTLMs = 1
	*plan.Definitions[taskIndex].Task.Run.Retry.MaxAttempts = 9
	plan.Definitions[taskIndex].Task.Run.Retry.Backoff.MinMs = 999
	plan.Definitions[taskIndex].Task.Schedule.Workspace.Secrets[0].Name = "CHANGED"
	if *index.Declarations[taskIndex].Task.Run.TTLMs != ttl ||
		*index.Declarations[taskIndex].Task.Run.Retry.MaxAttempts != maxAttempts ||
		index.Declarations[taskIndex].Task.Run.Retry.Backoff.MinMs != 100 ||
		index.Declarations[taskIndex].Task.Schedule.Workspace.Secrets[0].Name != "TOKEN" {
		t.Fatal("deployment plan aliases nested Program index state")
	}
}

func TestDeploymentPlanFromProgramIndexRejectsInvalidIndex(t *testing.T) {
	index := testProgramIndex(t)
	index.RuntimeContract = "helmr.runtime.v1"
	if _, err := DeploymentPlanFromProgramIndex(index); err == nil {
		t.Fatal("DeploymentPlanFromProgramIndex accepted an invalid index")
	}
}

func TestDeploymentPlanFromProgramIndexPreservesEmptyQueueArray(t *testing.T) {
	index := testProgramIndex(t)
	for _, declaration := range index.Declarations {
		if declaration.Kind == DefinitionKindSandbox {
			index.Declarations = []ProgramIndexDeclaration{declaration}
			break
		}
	}
	index.Queues = []QueueInput{}
	plan, err := DeploymentPlanFromProgramIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Queues == nil || len(plan.Queues) != 0 {
		t.Fatalf("deployment plan queues = %#v", plan.Queues)
	}
	cloned := cloneProgramIndex(index)
	if cloned.Queues == nil || len(cloned.Queues) != 0 {
		t.Fatalf("cloned Program index queues = %#v", cloned.Queues)
	}
}
