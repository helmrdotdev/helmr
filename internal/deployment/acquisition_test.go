package deployment

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestNodeModuleABIScansConditionalHeaderForNumericDistributionDefault(t *testing.T) {
	header := strings.Join([]string{
		"#if defined(NODE_EMBEDDER_MODULE_VERSION)",
		"#define NODE_MODULE_VERSION NODE_EMBEDDER_MODULE_VERSION",
		"#else",
		"#define NODE_MODULE_VERSION 137",
		"#endif",
	}, "\n")
	path := filepath.Join(t.TempDir(), "node_version.h")
	if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
		t.Fatal(err)
	}
	version, err := nodeModuleABI(path)
	if err != nil {
		t.Fatal(err)
	}
	if version != "137" {
		t.Fatalf("module ABI = %q, want 137", version)
	}
}

func TestNodeModuleABIRejectsMissingAmbiguousOrNonCanonicalDefinition(t *testing.T) {
	for _, test := range []struct {
		name   string
		header string
	}{
		{name: "missing", header: "#define NODE_MAJOR_VERSION 24\n"},
		{name: "symbolic only", header: "#define NODE_MODULE_VERSION NODE_EMBEDDER_MODULE_VERSION\n"},
		{name: "leading zero", header: "#define NODE_MODULE_VERSION 0137\n"},
		{name: "conflicting", header: "#define NODE_MODULE_VERSION 137\n#define NODE_MODULE_VERSION 138\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "node_version.h")
			if err := os.WriteFile(path, []byte(test.header), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := nodeModuleABI(path); err == nil {
				t.Fatal("invalid module ABI header succeeded")
			}
		})
	}
}

func TestConformanceFailureClassifiesOnlyVerifiedInvalidResultAsDeterministic(t *testing.T) {
	invalid := conformanceFailure(
		&verifierInvalidError{diagnostic: "fixture failed"},
		nil,
	)
	var deterministic interface {
		PlatformAcquisitionFailureReason() workerapi.PlatformAcquisitionFailureReason
	}
	if !errors.As(invalid, &deterministic) ||
		deterministic.PlatformAcquisitionFailureReason() !=
			workerapi.PlatformAcquisitionConformanceFailed {
		t.Fatalf("invalid result classification = %v", invalid)
	}

	infrastructure := conformanceFailure(errors.New("validator unavailable"), nil)
	if errors.As(infrastructure, &deterministic) {
		t.Fatalf("validator outage was terminalized: %v", infrastructure)
	}

	closeFailure := conformanceFailure(
		&verifierInvalidError{diagnostic: "fixture failed"},
		errors.New("snapshot close failed"),
	)
	if errors.As(closeFailure, &deterministic) {
		t.Fatalf("snapshot close failure was terminalized: %v", closeFailure)
	}
}
