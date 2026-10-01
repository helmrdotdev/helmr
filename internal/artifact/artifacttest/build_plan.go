// Package artifacttest supplies program artifact fixtures shared by the
// packages that build, verify and bundle program artifacts: a reference build
// plan, its declaration locator and program index, a runtime descriptor and an
// in-memory SquashFS artifact.
package artifacttest

import (
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

// BuildPlan is a build plan declaring a scheduled Task, an Actor and the
// Sandbox both use.
func BuildPlan() definition.BuildPlan {
	return definition.BuildPlan{
		FormatVersion: definition.BuildPlanFormatVersion,
		Definitions: []definition.Input{
			{
				Kind:       definition.KindTask,
				DeclaredID: "build",
				Task: &definition.TaskManifest{
					Payload: definition.SchemaManifest{Kind: definition.SchemaKindStandard},
					Run: definition.RunManifest{
						Queue:         "task/build",
						MaxDurationMs: 900000,
						Retry:         definition.RetryManifest{Enabled: false},
					},
					Schedule: &definition.ScheduleManifest{
						Cron:     "0 9 * * *",
						Timezone: "UTC",
						Computer: definition.ScheduleComputerManifest{
							SandboxDeclaredID: "repo",
							Secrets:           []secretbinding.Binding{},
						},
					},
				},
			},
			{
				Kind:       definition.KindActor,
				DeclaredID: "chat",
				Actor: &definition.ActorManifest{
					Run: definition.RunManifest{
						Queue:         "actor/chat",
						MaxDurationMs: 900000,
						Retry:         definition.RetryManifest{Enabled: false},
					},
					IdleTimeoutMs: 30000,
				},
			},
			{
				Kind:       definition.KindSandbox,
				DeclaredID: "repo",
				Sandbox: &definition.SandboxInputManifest{
					ImageBuild: definition.ImageBuild{
						Root: "repo",
						Images: []definition.ImageSpec{{
							Key: "repo",
							Platform: definition.ImagePlatform{
								OS:           "linux",
								Architecture: "x86_64",
							},
							Steps: []definition.ImageStep{
								{From: &definition.ImageFrom{Ref: "debian:bookworm-slim"}},
								{CopySourceFile: &definition.ImageCopySourceFile{
									Dst:  "/app/package.json",
									Path: "package.json",
								}},
							},
						}},
					},
					Resources: definition.ResourcesManifest{
						MilliCPU:  2000,
						MemoryMiB: 4096,
					},
				},
			},
		},
		Queues: []definition.QueueInput{
			{Name: "actor/chat", ConcurrencyLimit: new(int64(1))},
			{Name: "task/build"},
		},
	}
}
