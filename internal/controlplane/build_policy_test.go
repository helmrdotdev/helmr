package controlplane

import (
	"bytes"
	"encoding/hex"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/helmrdotdev/helmr/internal/deployment"
)

func controlPlaneBuildPolicy(t *testing.T) *deployment.BuildPolicy {
	t.Helper()
	keyring, fingerprints := controlPlaneNodeReleaseKeyring(t)
	raw, err := deployment.ComposeBuildPolicy(
		deployment.RuntimeInputs{
			Harness: platformInput("runtime", 4096),
		},
		deployment.ToolchainInputs{
			Base:     platformInput("toolchain", 4096),
			Compiler: controlPlaneCompilerInputs(),
		},
		keyring,
		fingerprints,
	)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := deployment.ParseBuildPolicy(raw)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

var controlPlaneNodeReleaseKeys struct {
	sync.Once
	keyring      []byte
	fingerprints []string
	err          error
}

func controlPlaneNodeReleaseKeyring(t *testing.T) ([]byte, []string) {
	t.Helper()
	controlPlaneNodeReleaseKeys.Do(func() {
		entity, err := openpgp.NewEntity("Helmr Test", "", "test@helmr.dev", nil)
		if err != nil {
			controlPlaneNodeReleaseKeys.err = err
			return
		}
		var keyring bytes.Buffer
		if err := entity.Serialize(&keyring); err != nil {
			controlPlaneNodeReleaseKeys.err = err
			return
		}
		controlPlaneNodeReleaseKeys.keyring = keyring.Bytes()
		controlPlaneNodeReleaseKeys.fingerprints = append(
			controlPlaneNodeReleaseKeys.fingerprints,
			strings.ToUpper(hex.EncodeToString(entity.PrimaryKey.Fingerprint)),
		)
		for _, subkey := range entity.Subkeys {
			controlPlaneNodeReleaseKeys.fingerprints = append(
				controlPlaneNodeReleaseKeys.fingerprints,
				strings.ToUpper(hex.EncodeToString(subkey.PublicKey.Fingerprint)),
			)
		}
		slices.Sort(controlPlaneNodeReleaseKeys.fingerprints)
	})
	if controlPlaneNodeReleaseKeys.err != nil {
		t.Fatal(controlPlaneNodeReleaseKeys.err)
	}
	return slices.Clone(controlPlaneNodeReleaseKeys.keyring), slices.Clone(controlPlaneNodeReleaseKeys.fingerprints)
}

func platformInput(label string, size int64) deployment.ArtifactDescriptor {
	return deployment.ArtifactDescriptor{
		Digest:    controlPlaneDigest(label),
		MediaType: deployment.PlatformTreeInputMediaType,
		SizeBytes: size,
	}
}

func controlPlaneCompilerInputs() deployment.CompilerInputs {
	return deployment.CompilerInputs{
		APIVersion: "helmr.compiler.v0",
		ConfigEvaluator: deployment.CompilerEntrypoint{
			APIVersion: deployment.ConfigEvaluatorContract,
			Digest:     controlPlaneDigest("config evaluator"),
			Entrypoint: "/nix/helmr/config-evaluator.mjs",
		},
		Esbuild: deployment.EsbuildInputs{
			APIPackageDigest: controlPlaneDigest("esbuild api"),
			BinaryDigest:     controlPlaneDigest("esbuild binary"),
			BinaryPath:       "/nix/helmr/esbuild",
			PackagePath:      "/nix/node_modules/esbuild",
			Version:          "0.28.1",
		},
		OptionsContractDigest: controlPlaneDigest("compiler options contract"),
		Output: deployment.CompilerOutputContract{
			Aggregate:    "analysis-only",
			FinalModules: "independent",
			SourceMaps:   "external",
		},
		ProgramCompiler: deployment.CompilerEntrypoint{
			APIVersion: "helmr.compiler.v0",
			Digest:     controlPlaneDigest("program compiler"),
			Entrypoint: "/nix/helmr/program-compiler.mjs",
		},
		Source: deployment.CompilerSourceContract{
			DeclarationExtensions: []string{".cjs", ".cts", ".js", ".jsx", ".mjs", ".mts", ".ts", ".tsx"},
			PackageDependencies:   "external",
			Semantics:             "pinned-esbuild",
			WorkspaceDependencies: "bundled",
		},
	}
}

func controlPlaneDigest(label string) string {
	character := "1"
	if label != "" {
		character = string("123456789abcdef"[len(label)%15])
	}
	return "sha256:" + strings.Repeat(character, 64)
}

func TestValidatePlatformCandidateBinding(t *testing.T) {
	source := deployment.PlatformSource{
		Digest: controlPlaneDigest("node source"), Origin: "https://nodejs.org/node",
		SizeBytes: 1,
	}
	runtime := deployment.InspectedPlatformArtifact{
		Runtime: &deployment.RuntimeArtifactDescriptor{
			NodeModuleABI: "137",
			NodeVersion:   "24.16.0",
			Source:        source,
		},
	}
	toolchain := deployment.InspectedPlatformArtifact{
		Toolchain: &deployment.ToolchainArtifactDescriptor{
			NodeModuleABI: "137",
			NodeSource:    source,
			NodeVersion:   "24.16.0",
			RuntimeDigest: controlPlaneDigest("runtime"),
		},
	}
	if err := validatePlatformCandidateBinding(
		runtime,
		toolchain,
		controlPlaneDigest("runtime"),
	); err != nil {
		t.Fatal(err)
	}
	toolchain.Toolchain.NodeModuleABI = "138"
	if err := validatePlatformCandidateBinding(
		runtime,
		toolchain,
		controlPlaneDigest("runtime"),
	); err == nil {
		t.Fatal("mismatched Node ABI was accepted")
	}
}
