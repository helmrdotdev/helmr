package deployment

import (
	"encoding/json"
	"strings"
	"testing"

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
			name: "producer sandbox build instructions",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					definitions := root["plan"].(map[string]any)["definitions"].([]any)
					deploymentBundleJSONDefinition(t, definitions, "sandbox")["manifest"].(map[string]any)["imageBuild"] = map[string]any{}
				})
			},
			errMsg: "unknown field",
		},
		{
			name: "program Runtime digest mismatch",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					program := root["program"].(map[string]any)
					program["index"].(map[string]any)["runtimeDigest"] =
						"sha256:" + strings.Repeat("e", 64)
				})
			},
			errMsg: "Runtime digest does not match runtime",
		},
		{
			name: "deployment plan queue differs from Program Index",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					queues := root["plan"].(map[string]any)["queues"].([]any)
					queues[0].(map[string]any)["concurrencyLimit"] = float64(2)
				})
			},
			errMsg: "program index does not match deployment plan",
		},
		{
			name: "deployment plan locator differs from Program Index",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					definitions := root["plan"].(map[string]any)["definitions"].([]any)
					definitions[0].(map[string]any)["locator"].(map[string]any)["exportName"] = "other"
				})
			},
			errMsg: "program index does not match deployment plan",
		},
		{
			name: "deployment plan sandbox digest differs from Workspace Image",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					definitions := root["plan"].(map[string]any)["definitions"].([]any)
					deploymentBundleJSONDefinition(t, definitions, "sandbox")["manifest"].(map[string]any)["image"].(map[string]any)["artifactDigest"] =
						"sha256:" + strings.Repeat("e", 64)
				})
			},
			errMsg: "artifact does not match plan",
		},
		{
			name: "deployment plan sandbox media type is not final",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					definitions := root["plan"].(map[string]any)["definitions"].([]any)
					deploymentBundleJSONDefinition(t, definitions, "sandbox")["manifest"].(map[string]any)["image"].(map[string]any)["mediaType"] =
						"application/octet-stream"
				})
			},
			errMsg: "sandbox image mediaType",
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

func TestDeploymentBundleAdmissionRequiresExactRuntimeRelease(t *testing.T) {
	bundle := testDeploymentBundle(t)
	admission := DeploymentBundleAdmission{
		Runtime: RuntimeDescriptor{
			Architecture:    bundle.Platform.Architecture,
			Digest:          bundle.Runtime.Artifact.Digest,
			FormatVersion:   RuntimeDescriptorFormatVersion,
			MediaType:       bundle.Runtime.Artifact.MediaType,
			RuntimeContract: bundle.Runtime.Contract,
			SizeBytes:       bundle.Runtime.Artifact.SizeBytes,
		},
	}
	if err := admission.Admit(bundle); err != nil {
		t.Fatalf("Admit: %v", err)
	}

	changed := bundle
	changed.Runtime.Artifact.Digest = "sha256:" + strings.Repeat("2", 64)
	changed.Program.Index.RuntimeDigest = changed.Runtime.Artifact.Digest
	if err := ValidateDeploymentBundle(changed); err != nil {
		t.Fatalf("ValidateDeploymentBundle: %v", err)
	}
	if err := admission.Admit(changed); err == nil ||
		err.Error() != "deployment bundle Runtime is not supported" {
		t.Fatalf("Admit error = %v", err)
	}
}

func testDeploymentBundle(t *testing.T) DeploymentBundle {
	t.Helper()
	program := ProgramOutput{
		Artifact: ProgramDescriptor{
			Digest:    "sha256:" + strings.Repeat("a", 64),
			SizeBytes: 4096,
			MediaType: ProgramArtifactMediaType,
		},
		Index: testProgramIndex(t),
	}
	program.Index.RuntimeDigest = "sha256:" + strings.Repeat("f", 64)
	plan := DeploymentPlan{
		FormatVersion: DeploymentPlanFormatVersion,
		Definitions:   append([]ProgramIndexDeclaration(nil), program.Index.Declarations...),
		Queues:        cloneQueueInputs(program.Index.Queues),
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
		Plan: plan,
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

func deploymentBundleJSONDefinition(
	t *testing.T,
	definitions []any,
	kind string,
) map[string]any {
	t.Helper()
	for _, raw := range definitions {
		definition := raw.(map[string]any)
		if definition["kind"] == kind {
			return definition
		}
	}
	t.Fatalf("deployment bundle has no %s definition", kind)
	return nil
}
