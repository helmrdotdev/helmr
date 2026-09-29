package controlplane

import (
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/secretbinding"
)

func TestNormalizeComputerSecretPlacementsCanonicalizesAndRejectsConflicts(t *testing.T) {
	placements, err := normalizeComputerSecretPlacements([]secretbinding.Binding{
		{Name: "config", File: &secretbinding.File{Path: "/run/helmr-secrets/config.json"}},
		{Name: "github", Env: &secretbinding.Env{Name: "GITHUB_TOKEN", Mode: "raw"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(placements) != 2 ||
		!reflect.DeepEqual(placements[0], secretbinding.Placement{Name: "github", Kind: "env", Target: "GITHUB_TOKEN", Mode: "raw"}) ||
		!reflect.DeepEqual(placements[1], secretbinding.Placement{Name: "config", Kind: "file", Target: "/run/helmr-secrets/config.json", Mode: "raw"}) {
		t.Fatalf("placements = %#v", placements)
	}

	for name, input := range map[string][]secretbinding.Binding{
		"duplicate env": {
			{Name: "first", Env: &secretbinding.Env{Name: "TOKEN", Mode: "raw"}},
			{Name: "second", Env: &secretbinding.Env{Name: "TOKEN", Mode: "raw"}},
		},
		"nested file": {
			{Name: "first", File: &secretbinding.File{Path: "/run/secrets"}},
			{Name: "second", File: &secretbinding.File{Path: "/run/secrets/token"}},
		},
		"computer file": {
			{Name: "first", File: &secretbinding.File{Path: "/workspace/token"}},
		},
		"reserved env": {
			{Name: "first", Env: &secretbinding.Env{Name: "HELMR_RUN_ID", Mode: "raw"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeComputerSecretPlacements(input); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestValidateComputerKeyPreservesExactBytes(t *testing.T) {
	valid := " repository "
	if err := validateComputerKey(&valid); err == nil {
		t.Fatal("expected edge whitespace to be rejected without normalization")
	}
	exact := "répository"
	if err := validateComputerKey(&exact); err != nil {
		t.Fatalf("valid exact UTF-8 key: %v", err)
	}
}

func TestComputerPublicStatusUsesPublicSpelling(t *testing.T) {
	status, err := computerPublicStatus("recovery_required")
	if err != nil {
		t.Fatal(err)
	}
	if status != api.ComputerStatusAvailable || string(status) != "available" {
		t.Fatalf("recovery status = %q", status)
	}
}
