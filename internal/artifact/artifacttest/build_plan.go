package artifacttest

import (
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/definition"
)

func BuildPlan() definition.BuildPlan {
	return definition.BuildPlan{FormatVersion: definition.BuildPlanFormatVersion, Definitions: []definition.Input{
		{Kind: definition.KindAgent, DeclaredID: "build", Agent: &definition.AgentManifest{ComputerDefinitionID: "repo", Setup: true, Triggers: map[string]definition.CronTrigger{"daily": {Cron: "0 9 * * *", Timezone: "UTC", Input: json.RawMessage(`[]`)}}}},
		{Kind: definition.KindAgent, DeclaredID: "chat", Agent: &definition.AgentManifest{ComputerDefinitionID: "repo", Triggers: map[string]definition.CronTrigger{}}},
		{Kind: definition.KindComputer, DeclaredID: "repo", Computer: &definition.ComputerInputManifest{
			ImageBuild: definition.ImageBuild{Root: "repo", Images: []definition.ImageSpec{{Key: "repo", Platform: definition.ImagePlatform{OS: "linux", Architecture: "x86_64"}, Steps: []definition.ImageStep{
				{From: &definition.ImageFrom{Ref: "debian:bookworm-slim"}}, {CopySourceFile: &definition.ImageCopySourceFile{Dst: "/app/package.json", Path: "package.json"}},
			}}}}, Resources: definition.ResourcesManifest{MilliCPU: 2000, MemoryMiB: 4096}, Secrets: []definition.SecretBinding{}, BuildSecrets: []definition.SecretBinding{},
		}},
	}}
}
