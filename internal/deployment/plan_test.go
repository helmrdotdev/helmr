package deployment

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/imagebuild"
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
		parsed.Definitions[2].Workspace == nil {
		t.Fatalf("typed definition union was not preserved: %+v", parsed.Definitions)
	}
}

func TestQueueConfigFromPlan(t *testing.T) {
	plan := testBuildPlan()
	config, err := QueueConfigFromPlan(plan)
	if err != nil {
		t.Fatalf("QueueConfigFromPlan: %v", err)
	}
	raw, err := CanonicalQueueConfig(config)
	if err != nil {
		t.Fatalf("CanonicalQueueConfig: %v", err)
	}
	want := `{"formatVersion":0,"queues":[{"concurrencyLimit":1,"name":"actor/chat"},{"name":"task/build"}]}`
	if string(raw) != want {
		t.Fatalf("queue config = %s, want %s", raw, want)
	}
	*config.Queues[0].ConcurrencyLimit = 3
	if *plan.Queues[0].ConcurrencyLimit != 1 {
		t.Fatal("queue config retained a plan pointer")
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
			name: "actor idle timeout",
			change: func(plan *BuildPlan) {
				plan.Definitions[1].Actor.IdleTimeoutMs = 0
			},
			errMsg: "idleTimeoutMs",
		},
		{
			name: "actor idle timeout maximum",
			change: func(plan *BuildPlan) {
				plan.Definitions[1].Actor.IdleTimeoutMs = maxActorIdleMs + 1
			},
			errMsg: "idleTimeoutMs",
		},
		{
			name: "image architecture",
			change: func(plan *BuildPlan) {
				plan.Definitions[2].Workspace.ImageBuild.Images[0].Platform.Architecture = "aarch64"
			},
			errMsg: "imageBuild",
		},
		{
			name: "workspace resources",
			change: func(plan *BuildPlan) {
				plan.Definitions[2].Workspace.Resources.MemoryMiB = 0
			},
			errMsg: "memoryMiB",
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
				run.TTLMs = new(int64(0))
			},
			errMsg: "ttlMs",
		},
		{
			name: "ttl maximum",
			change: func(run *RunManifest) {
				run.TTLMs = new(maxQueuedRunTTLMs + 1)
			},
			errMsg: "ttlMs",
		},
		{
			name: "disabled retry fields",
			change: func(run *RunManifest) {
				run.Retry.MaxAttempts = new(int64(1))
			},
			errMsg: "disabled retry",
		},
		{
			name: "enabled retry attempts",
			change: func(run *RunManifest) {
				run.Retry = validRetryManifest()
				run.Retry.MaxAttempts = new(int64(11))
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
			name: "retry delay order",
			change: func(run *RunManifest) {
				run.Retry = validRetryManifest()
				run.Retry.Backoff.MinMs = run.Retry.Backoff.MaxMs + 1
			},
			errMsg: "must not exceed",
		},
		{
			name: "retry delay maximum",
			change: func(run *RunManifest) {
				run.Retry = validRetryManifest()
				run.Retry.Backoff.MaxMs = maxRetryDelayMs + 1
			},
			errMsg: "maxMs",
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
			name: "cron empty",
			change: func(manifest *ScheduleManifest) {
				manifest.Cron = ""
			},
			errMsg: "1-1024 bytes",
		},
		{
			name: "cron syntax",
			change: func(manifest *ScheduleManifest) {
				manifest.Cron = "every day"
			},
			errMsg: "exactly 5 fields",
		},
		{
			name: "timezone normalization",
			change: func(manifest *ScheduleManifest) {
				manifest.Timezone = "utc"
			},
			errMsg: "IANA timezone",
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
				manifest.Workspace.ID = new("wsp_" + strings.Repeat("a", 26))
			},
			errMsg: "exactly one",
		},
		{
			name: "workspace id",
			change: func(manifest *ScheduleManifest) {
				manifest.Workspace = WorkspaceTarget{ID: new("workspace")}
			},
			errMsg: "workspace target id",
		},
		{
			name: "workspace key",
			change: func(manifest *ScheduleManifest) {
				manifest.Workspace = WorkspaceTarget{Key: new(" key ")}
			},
			errMsg: "workspace key domain",
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
		ID: new("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"),
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
				plan.Queues[0].ConcurrencyLimit = new(int64(0))
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
							Key: new("nightly-builder"),
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
				Kind:       DefinitionKindWorkspace,
				DeclaredID: "repo",
				Workspace: &WorkspaceInputManifest{
					ImageBuild: imagebuild.Build{
						FormatVersion: imagebuild.FormatVersion,
						Root:          "repo",
						Images: []imagebuild.Spec{{
							Key: "repo",
							Platform: imagebuild.Platform{
								OS:           "linux",
								Architecture: "x86_64",
							},
							Steps: []imagebuild.Step{
								{From: &imagebuild.From{Ref: "debian:bookworm-slim"}},
								{CopySourceFile: &imagebuild.CopySourceFile{
									Dst:  "/app/package.json",
									Path: "package.json",
								}},
							},
						}},
					},
					Resources: ResourcesManifest{
						MilliCPU:  2000,
						MemoryMiB: 4096,
					},
				},
			},
		},
		Queues: []QueueInput{
			{Name: "actor/chat", ConcurrencyLimit: new(int64(1))},
			{Name: "task/build"},
		},
	}
}

func validRetryManifest() RetryManifest {
	return RetryManifest{
		Enabled:     true,
		MaxAttempts: new(int64(3)),
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
