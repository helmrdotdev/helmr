package deployment

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestDeploymentBundleCanonicalRoundTrip(t *testing.T) {
	bundle := testDeploymentBundle(t)
	raw, err := CanonicalDeploymentBundle(bundle)
	if err != nil {
		t.Fatalf("CanonicalDeploymentBundle: %v", err)
	}
	parsed, err := ParseDeploymentBundle(raw)
	if err != nil {
		t.Fatalf("ParseDeploymentBundle: %v", err)
	}
	reencoded, err := CanonicalDeploymentBundle(parsed)
	if err != nil {
		t.Fatalf("CanonicalDeploymentBundle(parsed): %v", err)
	}
	if string(reencoded) != string(raw) {
		t.Fatalf("reencoded bundle differs:\n%s\n%s", reencoded, raw)
	}
	digest, err := DeploymentBundleDigest(raw)
	if err != nil {
		t.Fatalf("DeploymentBundleDigest: %v", err)
	}
	if !sha256DigestPattern.MatchString(digest) {
		t.Fatalf("bundle digest = %q", digest)
	}
}

func TestParseDeploymentBundleRequiresClosedCanonicalShape(t *testing.T) {
	raw := canonicalTestDeploymentBundle(t)
	tests := []struct {
		name   string
		raw    func() []byte
		errMsg string
	}{
		{
			name:   "noncanonical",
			raw:    func() []byte { return append([]byte(" "), raw...) },
			errMsg: "canonical",
		},
		{
			name: "unknown root member",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					root["unknown"] = true
				})
			},
			errMsg: "unknown field",
		},
		{
			name: "missing object",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					root["objects"] = root["objects"].([]any)[:1]
				})
			},
			errMsg: "objects do not match",
		},
		{
			name: "extra runtime object",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					runtime := root["runtime"].(map[string]any)
					objects := root["objects"].([]any)
					root["objects"] = append(objects, runtime["artifact"])
				})
			},
			errMsg: "objects do not match",
		},
		{
			name: "conflicting object metadata",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					root["objects"].([]any)[0].(map[string]any)["sizeBytes"] = float64(9)
				})
			},
			errMsg: "conflicts with its reference",
		},
		{
			name: "wrong object order",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					objects := root["objects"].([]any)
					objects[0], objects[1] = objects[1], objects[0]
				})
			},
			errMsg: "canonical digest order",
		},
		{
			name: "runtime included in tenant closure through alias",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					root["program"].(map[string]any)["artifact"] = root["runtime"].(map[string]any)["artifact"]
				})
			},
			errMsg: "program artifact mediaType",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseDeploymentBundle(test.raw()); err == nil || !strings.Contains(err.Error(), test.errMsg) {
				t.Fatalf("ParseDeploymentBundle error = %v, want %q", err, test.errMsg)
			}
		})
	}
}

func TestDeploymentBundleRejectsMutableOrManagedImageInputs(t *testing.T) {
	tests := []struct {
		name   string
		change func(*DeploymentBundle)
		want   string
	}{
		{
			name: "mutable tag",
			change: func(bundle *DeploymentBundle) {
				bundle.Plan.Definitions[2].Sandbox.ImageBuild.Images[0].Steps[0].From.Ref = "debian:bookworm-slim"
			},
			want: "must use a lowercase SHA-256 digest",
		},
		{
			name: "managed registry authentication",
			change: func(bundle *DeploymentBundle) {
				bundle.Plan.Definitions[2].Sandbox.ImageBuild.Images[0].Steps[0].From.Auth = testRegistryAuth()
			},
			want: "retains managed registry authentication",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bundle := testDeploymentBundle(t)
			test.change(&bundle)
			if err := ValidateDeploymentBundle(bundle); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateDeploymentBundle error = %v, want %q", err, test.want)
			}
		})
	}
}

func testDeploymentBundle(t *testing.T) DeploymentBundle {
	t.Helper()
	plan := testBuildPlan()
	plan.Definitions[2].Sandbox.ImageBuild.Images[0].Steps[0].From.Ref =
		"docker.io/library/debian@sha256:" + strings.Repeat("1", 64)
	program := ProgramOutput{
		Artifact: ProgramDescriptor{
			Digest:    "sha256:" + strings.Repeat("a", 64),
			SizeBytes: 4096,
			MediaType: ProgramArtifactMediaType,
		},
		Index: testProgramIndex(t),
	}
	workspaceImage := BundleWorkspaceImage{
		DeclaredID: "repo",
		Artifact: BundleWorkspaceImageArtifact{
			Architecture: ArchitectureX8664,
			Digest:       "sha256:" + strings.Repeat("d", 64),
			MediaType:    WorkspaceImageArtifactMediaType,
			SizeBytes:    4096,
		},
	}
	bundle := DeploymentBundle{
		Contract: DeploymentBundleContract,
		Platform: DeploymentBundlePlatform{
			Architecture: ArchitectureX8664,
			OS:           DeploymentBundleTargetOS,
		},
		BuildPolicyDigest: "sha256:" + strings.Repeat("b", 64),
		Plan:              plan,
		Runtime: DeploymentBundleRuntime{
			Contract: RuntimeContract,
			Artifact: BundleObject{
				Digest:    "sha256:" + strings.Repeat("f", 64),
				SizeBytes: 4096,
				MediaType: RuntimeArtifactMediaType,
			},
		},
		Program:         program,
		WorkspaceImages: []BundleWorkspaceImage{workspaceImage},
		Objects: []BundleObject{
			{Digest: program.Artifact.Digest, SizeBytes: program.Artifact.SizeBytes, MediaType: program.Artifact.MediaType},
			{Digest: workspaceImage.Artifact.Digest, SizeBytes: workspaceImage.Artifact.SizeBytes, MediaType: workspaceImage.Artifact.MediaType},
		},
	}
	SortDeploymentBundleObjects(bundle.Objects)
	return bundle
}

func canonicalTestDeploymentBundle(t *testing.T) []byte {
	t.Helper()
	raw, err := CanonicalDeploymentBundle(testDeploymentBundle(t))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mutateDeploymentBundleJSON(
	t *testing.T,
	raw []byte,
	mutate func(map[string]any),
) []byte {
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

func testRegistryAuth() *imagebuild.RegistryAuth {
	return &imagebuild.RegistryAuth{
		Username:       "builder",
		PasswordSecret: "registry-password",
	}
}
