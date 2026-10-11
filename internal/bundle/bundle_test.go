package bundle

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestDeploymentBundleCanonicalRoundTrip(t *testing.T) {
	bundle := testDeploymentBundle(t)
	raw, err := Canonical(bundle)
	if err != nil {
		t.Fatalf("CanonicalDeploymentBundle: %v", err)
	}
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatalf("ParseDeploymentBundle: %v", err)
	}
	reencoded, err := Canonical(parsed)
	if err != nil {
		t.Fatalf("CanonicalDeploymentBundle(parsed): %v", err)
	}
	if string(reencoded) != string(raw) {
		t.Fatalf("reencoded bundle differs:\n%s\n%s", reencoded, raw)
	}
	digest, err := Digest(raw)
	if err != nil {
		t.Fatalf("DeploymentBundleDigest: %v", err)
	}
	if !sha256sum.ValidDigest(digest) {
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
			name: "producer Computer build instructions",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					definitions := root["plan"].(map[string]any)["definitions"].([]any)
					deploymentBundleJSONDefinition(t, definitions, "computer")["manifest"].(map[string]any)["imageBuild"] = map[string]any{}
				})
			},
			errMsg: "unknown field",
		},
		{
			name: "program Runtime digest mismatch",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					program := root["program"].(map[string]any)
					program["metadata"].(map[string]any)["runtimeDigest"] =
						"sha256:" + strings.Repeat("e", 64)
				})
			},
			errMsg: "Runtime digest does not match runtime",
		},
		{
			name: "deployment plan trigger differs from Program Index",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					definitions := root["plan"].(map[string]any)["definitions"].([]any)
					definitions[0].(map[string]any)["manifest"].(map[string]any)["triggers"].(map[string]any)["daily"].(map[string]any)["cron"] = "0 10 * * *"
				})
			},
			errMsg: "program metadata does not match deployment plan",
		},
		{
			name: "deployment plan setup differs from Program Index",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					definitions := root["plan"].(map[string]any)["definitions"].([]any)
					definitions[0].(map[string]any)["manifest"].(map[string]any)["setup"] = false
				})
			},
			errMsg: "program metadata does not match deployment plan",
		},
		{
			name: "deployment plan Computer digest differs from Computer Image",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					definitions := root["plan"].(map[string]any)["definitions"].([]any)
					deploymentBundleJSONDefinition(t, definitions, "computer")["manifest"].(map[string]any)["seed"].(map[string]any)["artifactDigest"] =
						"sha256:" + strings.Repeat("e", 64)
				})
			},
			errMsg: "artifact does not match plan",
		},
		{
			name: "deployment plan Computer media type is not final",
			raw: func() []byte {
				return mutateDeploymentBundleJSON(t, raw, func(root map[string]any) {
					definitions := root["plan"].(map[string]any)["definitions"].([]any)
					deploymentBundleJSONDefinition(t, definitions, "computer")["manifest"].(map[string]any)["seed"].(map[string]any)["mediaType"] =
						"application/octet-stream"
				})
			},
			errMsg: "unsupported Computer seed contract",
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
			if _, err := Parse(test.raw()); err == nil || !strings.Contains(err.Error(), test.errMsg) {
				t.Fatalf("ParseDeploymentBundle error = %v, want %q", err, test.errMsg)
			}
		})
	}
}

func TestDeploymentBundleAdmissionRequiresExactRuntimeRelease(t *testing.T) {
	bundle := testDeploymentBundle(t)
	admission := Admission{
		Runtime: artifact.RuntimeDescriptor{
			Architecture:    bundle.Platform.Architecture,
			Digest:          bundle.Runtime.Artifact.Digest,
			FormatVersion:   artifact.RuntimeDescriptorFormatVersion,
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
	changed.Program.Metadata.RuntimeDigest = changed.Runtime.Artifact.Digest
	if err := validate(changed); err != nil {
		t.Fatalf("ValidateDeploymentBundle: %v", err)
	}
	if err := admission.Admit(changed); err == nil ||
		err.Error() != "deployment bundle Runtime is not supported" {
		t.Fatalf("Admit error = %v", err)
	}
}

func TestDeploymentBundleObjectClosureAllowsSharedComputerSeedObject(t *testing.T) {
	bundle := testDeploymentBundle(t)
	shared := bundle.ComputerSeeds[0]
	shared.DeclaredID = "repo-copy"
	bundle.ComputerSeeds = append(bundle.ComputerSeeds, shared)

	if err := validateBundleObjectClosure(bundle); err != nil {
		t.Fatalf("validateBundleObjectClosure: %v", err)
	}

	conflicting := bundle
	conflicting.ComputerSeeds = append(
		[]ComputerSeed(nil), bundle.ComputerSeeds...,
	)
	conflicting.ComputerSeeds[1].Artifact.SizeBytes++
	if err := validateBundleObjectClosure(conflicting); err == nil ||
		!strings.Contains(err.Error(), "conflicting reference metadata") {
		t.Fatalf("validateBundleObjectClosure error = %v", err)
	}
}

func testDeploymentBundle(t *testing.T) Manifest {
	t.Helper()
	program := artifact.ProgramOutput{
		Artifact: artifact.ProgramDescriptor{
			Digest:    "sha256:" + strings.Repeat("a", 64),
			SizeBytes: 4096,
			MediaType: artifact.ProgramArtifactMediaType,
		},
		Metadata: artifacttest.ProgramMetadata(t),
	}
	program.Metadata.RuntimeDigest = "sha256:" + strings.Repeat("f", 64)
	plan := Plan{
		FormatVersion: definition.DeploymentPlanFormatVersion,
		Definitions:   append([]artifact.ProgramDefinition(nil), program.Metadata.Definitions...),
	}
	computerImage := ComputerSeed{
		DeclaredID: "repo",
		Artifact: ComputerSeedArtifact{
			Profile:      definition.ComputerSeedProfile,
			Architecture: definition.ArchitectureX8664,
			Digest:       "sha256:" + strings.Repeat("d", 64),
			MediaType:    definition.ComputerSeedMediaType,
			SizeBytes:    4096,
		},
	}
	bundle := Manifest{
		Contract: Contract,
		Platform: Platform{
			Architecture: definition.ArchitectureX8664,
			OS:           TargetOS,
		},
		Plan: plan,
		Runtime: Runtime{
			Contract: definition.RuntimeContract,
			Artifact: Object{
				Digest:    "sha256:" + strings.Repeat("f", 64),
				SizeBytes: 4096,
				MediaType: artifact.RuntimeArtifactMediaType,
			},
		},
		Program:       program,
		ComputerSeeds: []ComputerSeed{computerImage},
		Objects: []Object{
			{Digest: program.Artifact.Digest, SizeBytes: program.Artifact.SizeBytes, MediaType: program.Artifact.MediaType},
			{Digest: computerImage.Artifact.Digest, SizeBytes: computerImage.Artifact.SizeBytes, MediaType: computerImage.Artifact.MediaType},
		},
	}
	SortObjects(bundle.Objects)
	return bundle
}

func canonicalTestDeploymentBundle(t *testing.T) []byte {
	t.Helper()
	raw, err := Canonical(testDeploymentBundle(t))
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

func TestDeploymentBundleBindsDiskProfileAndConfig(t *testing.T) {
	for _, kind := range []string{"profile", "config", "media type"} {
		t.Run(kind, func(t *testing.T) {
			bundle := testDeploymentBundle(t)
			switch kind {
			case "profile":
				bundle.ComputerSeeds[0].Artifact.Profile = "other"
			case "config":
				bundle.ComputerSeeds[0].Artifact.Config.User = "root"
			case "media type":
				bundle.ComputerSeeds[0].Artifact.MediaType = "application/octet-stream"
			}
			if err := validate(bundle); err == nil {
				t.Fatal("mismatched disk contract accepted")
			}
		})
	}
}
