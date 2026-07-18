package deployment

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const maxImageEnvironmentValueForResultTest = (1 << 20) - 1

func TestBuildResultCanonicalRoundTrip(t *testing.T) {
	tests := []struct {
		name   string
		result BuildResult
	}{
		{name: "succeeded", result: testSucceededBuildResult(t)},
		{name: "failed", result: testFailedBuildResult()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := CanonicalBuildResult(test.result)
			if err != nil {
				t.Fatalf("CanonicalBuildResult: %v", err)
			}
			parsed, err := ParseBuildResult(raw)
			if err != nil {
				t.Fatalf("ParseBuildResult: %v", err)
			}
			reencoded, err := CanonicalBuildResult(parsed)
			if err != nil {
				t.Fatalf("CanonicalBuildResult(parsed): %v", err)
			}
			if string(reencoded) != string(raw) {
				t.Fatalf("reencoded result differs:\n%s\n%s", reencoded, raw)
			}
		})
	}
}

func TestParseBuildResultRequiresClosedCanonicalShape(t *testing.T) {
	raw, err := CanonicalBuildResult(testSucceededBuildResult(t))
	if err != nil {
		t.Fatal(err)
	}
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
				return mutateBuildResultJSON(t, raw, func(root map[string]any) {
					delete(root, "formatVersion")
				})
			},
			errMsg: "complete canonical v0 shape",
		},
		{
			name: "unknown root member",
			raw: func() []byte {
				return mutateBuildResultJSON(t, raw, func(root map[string]any) {
					root["error"] = map[string]any{
						"reasonCode": "invalid_plan",
						"message":    "invalid",
					}
				})
			},
			errMsg: "unknown field",
		},
		{
			name: "unknown artifact member",
			raw: func() []byte {
				return mutateBuildResultJSON(t, raw, func(root map[string]any) {
					images := root["workspaceImages"].([]any)
					images[0].(map[string]any)["artifact"].(map[string]any)["unknown"] = true
				})
			},
			errMsg: "unknown field",
		},
		{
			name: "null workspace images",
			raw: func() []byte {
				return mutateBuildResultJSON(t, raw, func(root map[string]any) {
					root["workspaceImages"] = nil
				})
			},
			errMsg: "workspaceImages must be an array",
		},
		{
			name: "wrong variant",
			raw: func() []byte {
				return mutateBuildResultJSON(t, raw, func(root map[string]any) {
					root["outcome"] = "failed"
				})
			},
			errMsg: "unknown field",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseBuildResult(test.raw())
			if err == nil || !strings.Contains(err.Error(), test.errMsg) {
				t.Fatalf("error = %v, want containing %q", err, test.errMsg)
			}
		})
	}
}

func TestValidateBuildSucceeded(t *testing.T) {
	tests := []struct {
		name   string
		change func(*BuildResult)
		errMsg string
	}{
		{
			name: "invalid plan",
			change: func(result *BuildResult) {
				result.Succeeded.Plan.Definitions[0].Task.Run.Queue = "missing"
			},
			errMsg: "build result plan",
		},
		{
			name: "missing receipt",
			change: func(result *BuildResult) {
				result.Succeeded.ProgramReceipt = nil
			},
			errMsg: "requires programReceipt",
		},
		{
			name: "invalid receipt",
			change: func(result *BuildResult) {
				result.Succeeded.ProgramReceipt.Code.Digest = "invalid"
			},
			errMsg: "programReceipt",
		},
		{
			name: "receipt declarations",
			change: func(result *BuildResult) {
				result.Succeeded.ProgramReceipt.Index.Declarations[0].DeclaredID = "other"
			},
			errMsg: "declarations do not match",
		},
		{
			name: "program Workspace architecture",
			change: func(result *BuildResult) {
				result.Succeeded.ProgramReceipt.Index.Architecture = ArchitectureAArch64
			},
			errMsg: "does not match program",
		},
		{
			name: "workspace image array",
			change: func(result *BuildResult) {
				result.Succeeded.WorkspaceImages = nil
			},
			errMsg: "workspaceImages must be an array",
		},
		{
			name: "workspace image count",
			change: func(result *BuildResult) {
				result.Succeeded.WorkspaceImages = []WorkspaceImage{}
			},
			errMsg: "do not match plan",
		},
		{
			name: "workspace declared id",
			change: func(result *BuildResult) {
				result.Succeeded.WorkspaceImages[0].DeclaredID = "other"
			},
			errMsg: "declaredId",
		},
		{
			name: "workspace digest",
			change: func(result *BuildResult) {
				result.Succeeded.WorkspaceImages[0].Artifact.Digest = "sha256:ABC"
			},
			errMsg: "lowercase SHA-256",
		},
		{
			name: "workspace size",
			change: func(result *BuildResult) {
				result.Succeeded.WorkspaceImages[0].Artifact.SizeBytes = 0
			},
			errMsg: "sizeBytes",
		},
		{
			name: "workspace media type",
			change: func(result *BuildResult) {
				result.Succeeded.WorkspaceImages[0].Artifact.MediaType = "application/octet-stream"
			},
			errMsg: "mediaType",
		},
		{
			name: "workspace architecture",
			change: func(result *BuildResult) {
				result.Succeeded.WorkspaceImages[0].Artifact.Architecture = ArchitectureAArch64
			},
			errMsg: "architecture",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := testSucceededBuildResult(t)
			test.change(&result)
			assertBuildResultError(t, result, test.errMsg)
		})
	}

}

func TestValidateWorkspaceOnlyBuildResult(t *testing.T) {
	result := testSucceededBuildResult(t)
	result.Succeeded.Plan.Definitions = result.Succeeded.Plan.Definitions[3:]
	result.Succeeded.Plan.Queues = []QueueInput{}
	result.Succeeded.ProgramReceipt = nil
	if err := ValidateBuildResultContract(result); err != nil {
		t.Fatalf("workspace-only result: %v", err)
	}

	receipt := testProgramReceipt(t)
	result.Succeeded.ProgramReceipt = &receipt
	assertBuildResultError(t, result, "must not contain programReceipt")
}

func TestValidateBuildResultTarget(t *testing.T) {
	result := testSucceededBuildResult(t)
	runtimeDigest := result.Succeeded.ProgramReceipt.Index.RuntimeDigest
	if err := ValidateBuildResultTarget(
		result,
		runtimeDigest,
		ArchitectureX8664,
	); err != nil {
		t.Fatalf("ValidateBuildResultTarget: %v", err)
	}

	tests := []struct {
		name          string
		runtimeDigest string
		architecture  RuntimeArchitecture
		errMsg        string
	}{
		{
			name:          "runtime",
			runtimeDigest: "sha256:" + strings.Repeat("e", 64),
			architecture:  ArchitectureX8664,
			errMsg:        "runtime digest does not match",
		},
		{
			name:          "architecture",
			runtimeDigest: runtimeDigest,
			architecture:  ArchitectureAArch64,
			errMsg:        "program architecture does not match",
		},
		{
			name:          "invalid runtime",
			runtimeDigest: "latest",
			architecture:  ArchitectureX8664,
			errMsg:        "lowercase SHA-256",
		},
		{
			name:          "invalid architecture",
			runtimeDigest: runtimeDigest,
			architecture:  "amd64",
			errMsg:        "unsupported",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateBuildResultTarget(result, test.runtimeDigest, test.architecture)
			if err == nil || !strings.Contains(err.Error(), test.errMsg) {
				t.Fatalf("error = %v, want containing %q", err, test.errMsg)
			}
		})
	}

	workspaceOnly := testSucceededBuildResult(t)
	workspaceOnly.Succeeded.Plan.Definitions = workspaceOnly.Succeeded.Plan.Definitions[3:]
	workspaceOnly.Succeeded.Plan.Queues = []QueueInput{}
	workspaceOnly.Succeeded.ProgramReceipt = nil
	workspaceOnly.Succeeded.Plan.Definitions[0].Workspace.Architecture = ArchitectureAArch64
	workspaceOnly.Succeeded.Plan.Definitions[0].Workspace.ImageBuild.Images[0].Platform.Architecture = "aarch64"
	workspaceOnly.Succeeded.WorkspaceImages[0].Artifact.Architecture = ArchitectureAArch64
	err := ValidateBuildResultTarget(
		workspaceOnly,
		runtimeDigest,
		ArchitectureX8664,
	)
	if err == nil || !strings.Contains(err.Error(), "workspace \"repo\" architecture does not match") {
		t.Fatalf("workspace target error = %v", err)
	}
}

func TestValidateBuildResultAppliesPlanSizeBound(t *testing.T) {
	plan := BuildPlan{
		FormatVersion: BuildPlanFormatVersion,
		Definitions:   make([]DefinitionInput, 17),
		Queues:        []QueueInput{},
	}
	images := make([]WorkspaceImage, len(plan.Definitions))
	value := strings.Repeat("x", maxImageEnvironmentValueForResultTest)
	for index := range plan.Definitions {
		id := fmt.Sprintf("workspace-%02d", index)
		plan.Definitions[index] = DefinitionInput{
			Kind:       DefinitionKindWorkspace,
			DeclaredID: id,
			Workspace: &WorkspaceInputManifest{
				ImageBuild: builder.ImageBuild{
					FormatVersion: builder.ImageBuildFormatVersion,
					Root:          id,
					Images: []builder.ImageSpec{{
						Key: id,
						Platform: builder.ImagePlatform{
							OS:           "linux",
							Architecture: "x86_64",
						},
						Steps: []builder.ImageStep{
							{From: &builder.ImageFrom{Ref: "alpine:3.23"}},
							{Env: &builder.ImageEnv{Key: "X", Value: value}},
						},
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
				Architecture: ArchitectureX8664,
			},
		}
		images[index] = WorkspaceImage{
			DeclaredID: id,
			Artifact: WorkspaceImageArtifact{
				Digest:       "sha256:" + strings.Repeat("d", 64),
				SizeBytes:    4096,
				MediaType:    WorkspaceImageArtifactMediaType,
				Architecture: ArchitectureX8664,
			},
		}
	}
	result := BuildResult{
		FormatVersion: BuildResultFormatVersion,
		Outcome:       BuildOutcomeSucceeded,
		Succeeded: &BuildSucceeded{
			Plan:            plan,
			WorkspaceImages: images,
		},
	}
	assertBuildResultError(t, result, "build plan size")
}

func TestValidateBuildFailed(t *testing.T) {
	tests := []struct {
		name   string
		change func(*BuildResult)
		errMsg string
	}{
		{
			name: "reason",
			change: func(result *BuildResult) {
				result.Failed.Error.ReasonCode = "network_failed"
			},
			errMsg: "reasonCode",
		},
		{
			name: "blank message",
			change: func(result *BuildResult) {
				result.Failed.Error.Message = " \n "
			},
			errMsg: "nonblank UTF-8",
		},
		{
			name: "message bound",
			change: func(result *BuildResult) {
				result.Failed.Error.Message = strings.Repeat("x", maxBuildFailureMessageBytes+1)
			},
			errMsg: "nonblank UTF-8",
		},
		{
			name: "outcome value",
			change: func(result *BuildResult) {
				result.Outcome = BuildOutcomeSucceeded
			},
			errMsg: "requires success data",
		},
		{
			name: "ambiguous value",
			change: func(result *BuildResult) {
				result.Succeeded = &BuildSucceeded{}
			},
			errMsg: "exactly one outcome",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := testFailedBuildResult()
			test.change(&result)
			assertBuildResultError(t, result, test.errMsg)
		})
	}
}

func TestParseBuildResultSizeBound(t *testing.T) {
	raw := make([]byte, maxBuildResultBytes+1)
	if _, err := ParseBuildResult(raw); err == nil {
		t.Fatal("ParseBuildResult accepted oversized input")
	}
}

func testSucceededBuildResult(t *testing.T) BuildResult {
	t.Helper()
	plan := testBuildPlan()
	receipt := testProgramReceipt(t)
	receipt.Index.Declarations = buildPlanProgramDeclarations(plan)
	return BuildResult{
		FormatVersion: BuildResultFormatVersion,
		Outcome:       BuildOutcomeSucceeded,
		Succeeded: &BuildSucceeded{
			Plan:           plan,
			ProgramReceipt: &receipt,
			WorkspaceImages: []WorkspaceImage{{
				DeclaredID: "repo",
				Artifact: WorkspaceImageArtifact{
					Digest:       "sha256:" + strings.Repeat("d", 64),
					SizeBytes:    4096,
					MediaType:    WorkspaceImageArtifactMediaType,
					Architecture: ArchitectureX8664,
				},
			}},
		},
	}
}

func testFailedBuildResult() BuildResult {
	return BuildResult{
		FormatVersion: BuildResultFormatVersion,
		Outcome:       BuildOutcomeFailed,
		Failed: &BuildFailed{
			Error: BuildError{
				ReasonCode: BuildFailureInvalidPlan,
				Message:    "build plan is invalid",
			},
		},
	}
}

func mutateBuildResultJSON(t *testing.T, raw []byte, mutate func(map[string]any)) []byte {
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

func assertBuildResultError(t *testing.T, result BuildResult, want string) {
	t.Helper()
	err := ValidateBuildResultContract(result)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want containing %q", err, want)
	}
}
