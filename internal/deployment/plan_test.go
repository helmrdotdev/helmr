package deployment

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestBuildPlanCanonicalRoundTrip(t *testing.T) {
	plan := testBuildPlan()
	raw, err := CanonicalBuildPlan(plan)
	if err != nil {
		t.Fatalf("CanonicalBuildPlan: %v", err)
	}
	parsed, err := ParseBuildPlan(raw)
	if err != nil {
		t.Fatalf("ParseBuildPlan: %v", err)
	}
	reencoded, err := CanonicalBuildPlan(parsed)
	if err != nil {
		t.Fatalf("CanonicalBuildPlan(parsed): %v", err)
	}
	if string(reencoded) != string(raw) {
		t.Fatalf("reencoded plan differs:\n%s\n%s", reencoded, raw)
	}
	if parsed.Definitions[0].Task == nil ||
		parsed.Definitions[1].Actor == nil ||
		parsed.Definitions[2].RunStream == nil ||
		parsed.Definitions[3].Workspace == nil {
		t.Fatalf("typed definition union was not preserved: %+v", parsed.Definitions)
	}
}

func TestParseBuildPlanRequiresClosedCanonicalShape(t *testing.T) {
	raw := canonicalTestBuildPlan(t)
	tests := []struct {
		name   string
		raw    func() []byte
		errMsg string
	}{
		{
			name: "noncanonical",
			raw: func() []byte {
				return append([]byte(" "), raw...)
			},
			errMsg: "canonical",
		},
		{
			name: "missing format version",
			raw: func() []byte {
				return mutateBuildPlanJSON(t, raw, func(root map[string]any) {
					delete(root, "formatVersion")
				})
			},
			errMsg: "complete canonical v0 shape",
		},
		{
			name: "null queue array",
			raw: func() []byte {
				return mutateBuildPlanJSON(t, raw, func(root map[string]any) {
					root["queues"] = nil
				})
			},
			errMsg: "queues must be an array",
		},
		{
			name: "unknown root member",
			raw: func() []byte {
				return mutateBuildPlanJSON(t, raw, func(root map[string]any) {
					root["unknown"] = true
				})
			},
			errMsg: "unknown field",
		},
		{
			name: "unknown definition member",
			raw: func() []byte {
				return mutateBuildPlanJSON(t, raw, func(root map[string]any) {
					definitions := root["definitions"].([]any)
					definitions[0].(map[string]any)["unknown"] = true
				})
			},
			errMsg: "unknown field",
		},
		{
			name: "unknown manifest member",
			raw: func() []byte {
				return mutateBuildPlanJSON(t, raw, func(root map[string]any) {
					definitions := root["definitions"].([]any)
					definitions[0].(map[string]any)["manifest"].(map[string]any)["unknown"] = true
				})
			},
			errMsg: "unknown field",
		},
		{
			name: "null optional member",
			raw: func() []byte {
				return mutateBuildPlanJSON(t, raw, func(root map[string]any) {
					definitions := root["definitions"].([]any)
					run := definitions[0].(map[string]any)["manifest"].(map[string]any)["run"].(map[string]any)
					run["ttlMs"] = nil
				})
			},
			errMsg: "complete canonical v0 shape",
		},
		{
			name: "manifest does not match kind",
			raw: func() []byte {
				return mutateBuildPlanJSON(t, raw, func(root map[string]any) {
					definitions := root["definitions"].([]any)
					definitions[0].(map[string]any)["kind"] = "actor"
				})
			},
			errMsg: "unknown field",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseBuildPlan(test.raw())
			if err == nil || !strings.Contains(err.Error(), test.errMsg) {
				t.Fatalf("error = %v, want containing %q", err, test.errMsg)
			}
		})
	}
}

func TestValidateBuildPlanDefinitions(t *testing.T) {
	tests := []struct {
		name   string
		change func(*BuildPlan)
		errMsg string
	}{
		{
			name: "empty definitions",
			change: func(plan *BuildPlan) {
				plan.Definitions = []DefinitionInput{}
			},
			errMsg: "non-empty array",
		},
		{
			name: "definition order",
			change: func(plan *BuildPlan) {
				plan.Definitions[0], plan.Definitions[1] = plan.Definitions[1], plan.Definitions[0]
			},
			errMsg: "canonical order",
		},
		{
			name: "declared id",
			change: func(plan *BuildPlan) {
				plan.Definitions[0].DeclaredID = "task/id"
			},
			errMsg: "ASCII ID domain",
		},
		{
			name: "manifest oneof",
			change: func(plan *BuildPlan) {
				plan.Definitions[0].Actor = plan.Definitions[1].Actor
			},
			errMsg: "exactly one manifest",
		},
		{
			name: "kind manifest mismatch",
			change: func(plan *BuildPlan) {
				plan.Definitions[0].Kind = DefinitionKindActor
			},
			errMsg: "actor manifest",
		},
		{
			name: "payload kind",
			change: func(plan *BuildPlan) {
				plan.Definitions[0].Task.Payload.Kind = "json_schema"
			},
			errMsg: "payload kind",
		},
		{
			name: "scheduled task payload",
			change: func(plan *BuildPlan) {
				plan.Definitions[0].Task.Payload.Kind = SchemaKindNone
			},
			errMsg: "scheduled task payload",
		},
		{
			name: "run stream schema",
			change: func(plan *BuildPlan) {
				plan.Definitions[2].RunStream.Schema.Kind = SchemaKindNone
			},
			errMsg: "schema kind",
		},
		{
			name: "actor idle timeout",
			change: func(plan *BuildPlan) {
				plan.Definitions[1].Actor.IdleTimeoutMs = 0
			},
			errMsg: "idleTimeoutMs",
		},
		{
			name: "workspace architecture",
			change: func(plan *BuildPlan) {
				plan.Definitions[3].Workspace.Architecture = "amd64"
			},
			errMsg: "architecture",
		},
		{
			name: "mixed Workspace architectures",
			change: func(plan *BuildPlan) {
				plan.Definitions = append(plan.Definitions, DefinitionInput{
					Kind:       DefinitionKindWorkspace,
					DeclaredID: "repo-arm",
					Workspace: &WorkspaceInputManifest{
						ImageBuild: builder.ImageBuild{
							FormatVersion: builder.ImageBuildFormatVersion,
							Root:          "repo-arm",
							Images: []builder.ImageSpec{{
								Key: "repo-arm",
								Platform: builder.ImagePlatform{
									OS:           "linux",
									Architecture: "aarch64",
								},
								Steps: []builder.ImageStep{{
									From: &builder.ImageFrom{Ref: "alpine:3.23"},
								}},
							}},
						},
						Resources: ResourcesManifest{
							MilliCPU:  1000,
							MemoryMiB: 1024,
							DiskMiB:   8192,
						},
						Network: NetworkManifest{
							Internet:  true,
							DenyCIDRs: []string{},
						},
						Architecture: ArchitectureAArch64,
					},
				})
			},
			errMsg: "Workspace architectures must match",
		},
		{
			name: "image architecture",
			change: func(plan *BuildPlan) {
				plan.Definitions[3].Workspace.ImageBuild.Images[0].Platform.Architecture = "aarch64"
			},
			errMsg: "imageBuild",
		},
		{
			name: "workspace resources",
			change: func(plan *BuildPlan) {
				plan.Definitions[3].Workspace.Resources.MemoryMiB = 0
			},
			errMsg: "memoryMiB",
		},
		{
			name: "workspace network array",
			change: func(plan *BuildPlan) {
				plan.Definitions[3].Workspace.Network.DenyCIDRs = nil
			},
			errMsg: "denyCidrs must be an array",
		},
		{
			name: "workspace network canonical prefix",
			change: func(plan *BuildPlan) {
				plan.Definitions[3].Workspace.Network.DenyCIDRs = []string{"10.1.0.1/8"}
			},
			errMsg: "canonical masked",
		},
		{
			name: "workspace network disabled deny",
			change: func(plan *BuildPlan) {
				plan.Definitions[3].Workspace.Network.Internet = false
			},
			errMsg: "empty when internet is disabled",
		},
		{
			name: "workspace network order",
			change: func(plan *BuildPlan) {
				plan.Definitions[3].Workspace.Network.DenyCIDRs = []string{
					"2001:db8::/32",
					"10.0.0.0/8",
				}
			},
			errMsg: "canonical order",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := testBuildPlan()
			test.change(&plan)
			assertBuildPlanError(t, plan, test.errMsg)
		})
	}
}

func TestValidateBuildPlanRunPolicy(t *testing.T) {
	tests := []struct {
		name   string
		change func(*RunManifest)
		errMsg string
	}{
		{
			name: "queue domain",
			change: func(run *RunManifest) {
				run.Queue = "../queue"
			},
			errMsg: "queue name",
		},
		{
			name: "undeclared queue",
			change: func(run *RunManifest) {
				run.Queue = "missing"
			},
			errMsg: "not declared",
		},
		{
			name: "minimum duration",
			change: func(run *RunManifest) {
				run.MaxDurationMs = minRunDurationMs - 1
			},
			errMsg: "maxDurationMs",
		},
		{
			name: "ttl",
			change: func(run *RunManifest) {
				run.TTLMs = pointer(int64(0))
			},
			errMsg: "ttlMs",
		},
		{
			name: "disabled retry fields",
			change: func(run *RunManifest) {
				run.Retry.MaxAttempts = pointer(int64(1))
			},
			errMsg: "disabled retry",
		},
		{
			name: "enabled retry attempts",
			change: func(run *RunManifest) {
				run.Retry = validRetryManifest()
				run.Retry.MaxAttempts = pointer(int64(11))
			},
			errMsg: "maxAttempts",
		},
		{
			name: "enabled retry backoff",
			change: func(run *RunManifest) {
				run.Retry = validRetryManifest()
				run.Retry.Backoff = nil
			},
			errMsg: "requires backoff",
		},
		{
			name: "retry factor",
			change: func(run *RunManifest) {
				run.Retry = validRetryManifest()
				run.Retry.Backoff.Factor = 0
			},
			errMsg: "factor",
		},
		{
			name: "retry jitter",
			change: func(run *RunManifest) {
				run.Retry = validRetryManifest()
				run.Retry.Backoff.Jitter = "random"
			},
			errMsg: "jitter",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := testBuildPlan()
			test.change(&plan.Definitions[0].Task.Run)
			assertBuildPlanError(t, plan, test.errMsg)
		})
	}

	plan := testBuildPlan()
	plan.Definitions[0].Task.Run.Retry = validRetryManifest()
	if err := ValidateBuildPlan(plan); err != nil {
		t.Fatalf("enabled retry: %v", err)
	}
}

func TestValidateBuildPlanSchedule(t *testing.T) {
	tests := []struct {
		name   string
		change func(*ScheduleManifest)
		errMsg string
	}{
		{
			name: "cron whitespace",
			change: func(manifest *ScheduleManifest) {
				manifest.Cron = " 0 9 * * *"
			},
			errMsg: "normalized",
		},
		{
			name: "cron syntax",
			change: func(manifest *ScheduleManifest) {
				manifest.Cron = "every day"
			},
			errMsg: "5-field",
		},
		{
			name: "timezone normalization",
			change: func(manifest *ScheduleManifest) {
				manifest.Timezone = "utc"
			},
			errMsg: "normalized IANA",
		},
		{
			name: "timezone",
			change: func(manifest *ScheduleManifest) {
				manifest.Timezone = "Mars/Olympus"
			},
			errMsg: "IANA timezone",
		},
		{
			name: "workspace target missing",
			change: func(manifest *ScheduleManifest) {
				manifest.Workspace = WorkspaceTarget{}
			},
			errMsg: "exactly one",
		},
		{
			name: "workspace target ambiguous",
			change: func(manifest *ScheduleManifest) {
				manifest.Workspace.ID = pointer("wsp_" + strings.Repeat("a", 26))
			},
			errMsg: "exactly one",
		},
		{
			name: "workspace id",
			change: func(manifest *ScheduleManifest) {
				manifest.Workspace = WorkspaceTarget{ID: pointer("workspace")}
			},
			errMsg: "workspace target id",
		},
		{
			name: "workspace key",
			change: func(manifest *ScheduleManifest) {
				manifest.Workspace = WorkspaceTarget{Key: pointer(" key ")}
			},
			errMsg: "Workspace key domain",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := testBuildPlan()
			test.change(plan.Definitions[0].Task.Schedule)
			assertBuildPlanError(t, plan, test.errMsg)
		})
	}

	plan := testBuildPlan()
	plan.Definitions[0].Task.Schedule.Workspace = WorkspaceTarget{
		ID: pointer("wsp_" + strings.Repeat("a", 26)),
	}
	if err := ValidateBuildPlan(plan); err != nil {
		t.Fatalf("workspace ID schedule: %v", err)
	}
}

func TestValidateBuildPlanQueues(t *testing.T) {
	tests := []struct {
		name   string
		change func(*BuildPlan)
		errMsg string
	}{
		{
			name: "nil",
			change: func(plan *BuildPlan) {
				plan.Queues = nil
			},
			errMsg: "queues must be an array",
		},
		{
			name: "order",
			change: func(plan *BuildPlan) {
				plan.Queues[0], plan.Queues[1] = plan.Queues[1], plan.Queues[0]
			},
			errMsg: "canonical name order",
		},
		{
			name: "duplicate",
			change: func(plan *BuildPlan) {
				plan.Queues[1].Name = plan.Queues[0].Name
			},
			errMsg: "canonical name order",
		},
		{
			name: "limit",
			change: func(plan *BuildPlan) {
				plan.Queues[0].ConcurrencyLimit = pointer(int64(0))
			},
			errMsg: "concurrencyLimit",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := testBuildPlan()
			test.change(&plan)
			assertBuildPlanError(t, plan, test.errMsg)
		})
	}
}

func testBuildPlan() BuildPlan {
	digest := "sha256:" + strings.Repeat("a", 64)
	return BuildPlan{
		FormatVersion: BuildPlanFormatVersion,
		Definitions: []DefinitionInput{
			{
				Kind:       DefinitionKindTask,
				DeclaredID: "build",
				Task: &TaskManifest{
					Payload: SchemaManifest{Kind: SchemaKindStandard},
					Run: RunManifest{
						Queue:         "task/build",
						MaxDurationMs: 900000,
						Retry:         RetryManifest{Enabled: false},
					},
					Schedule: &ScheduleManifest{
						Cron:     "0 9 * * *",
						Timezone: "UTC",
						Workspace: WorkspaceTarget{
							Key: pointer("nightly-builder"),
						},
					},
				},
			},
			{
				Kind:       DefinitionKindActor,
				DeclaredID: "chat",
				Actor: &ActorManifest{
					Run: RunManifest{
						Queue:         "actor/chat",
						MaxDurationMs: 900000,
						Retry:         RetryManifest{Enabled: false},
					},
					IdleTimeoutMs: 30000,
				},
			},
			{
				Kind:       DefinitionKindRunStream,
				DeclaredID: "events",
				RunStream: &RunStreamManifest{
					Schema: SchemaManifest{Kind: SchemaKindStandard},
				},
			},
			{
				Kind:       DefinitionKindWorkspace,
				DeclaredID: "repo",
				Workspace: &WorkspaceInputManifest{
					ImageBuild: builder.ImageBuild{
						FormatVersion: builder.ImageBuildFormatVersion,
						Root:          "repo",
						Images: []builder.ImageSpec{{
							Key: "repo",
							Platform: builder.ImagePlatform{
								OS:           "linux",
								Architecture: "x86_64",
							},
							Steps: []builder.ImageStep{
								{From: &builder.ImageFrom{Ref: "debian:bookworm-slim"}},
								{CopySourceFile: &builder.ImageCopySourceFile{
									Dst:    "/app/package.json",
									Path:   "package.json",
									Digest: digest,
								}},
							},
						}},
					},
					Resources: ResourcesManifest{
						MilliCPU:  2000,
						MemoryMiB: 4096,
						DiskMiB:   32768,
					},
					Network: NetworkManifest{
						Internet:  true,
						DenyCIDRs: []string{"10.0.0.0/8", "2001:db8::/32"},
					},
					Architecture: ArchitectureX8664,
				},
			},
		},
		Queues: []QueueInput{
			{Name: "actor/chat", ConcurrencyLimit: pointer(int64(1))},
			{Name: "task/build"},
		},
	}
}

func validRetryManifest() RetryManifest {
	return RetryManifest{
		Enabled:     true,
		MaxAttempts: pointer(int64(3)),
		Backoff: &RetryBackoff{
			MinMs:  1000,
			MaxMs:  30000,
			Factor: 2,
			Jitter: RetryJitterFull,
		},
	}
}

func canonicalTestBuildPlan(t *testing.T) []byte {
	t.Helper()
	raw, err := CanonicalBuildPlan(testBuildPlan())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mutateBuildPlanJSON(t *testing.T, raw []byte, mutate func(map[string]any)) []byte {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	mutate(root)
	encoded, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := jsoncanon.Transform(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func assertBuildPlanError(t *testing.T, plan BuildPlan, want string) {
	t.Helper()
	err := ValidateBuildPlan(plan)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want containing %q", err, want)
	}
}

func pointer[T any](value T) *T {
	return &value
}
