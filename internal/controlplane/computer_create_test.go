package controlplane

import (
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/computer"
)

func TestNormalizeComputerSecretPlacementsCanonicalizesAndRejectsConflicts(t *testing.T) {
	placements, err := normalizeComputerSecretPlacements([]api.ComputerSecret{
		{Name: "config", File: &api.SecretFile{Path: "/run/helmr-secrets/config.json"}},
		{Name: "github", Env: &api.SecretEnv{Name: "GITHUB_TOKEN", Mode: "raw"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(placements) != 2 ||
		!reflect.DeepEqual(placements[0], computer.SecretPlacement{Name: "github", Kind: "env", Target: "GITHUB_TOKEN", Mode: "raw"}) ||
		!reflect.DeepEqual(placements[1], computer.SecretPlacement{Name: "config", Kind: "file", Target: "/run/helmr-secrets/config.json", Mode: "raw"}) {
		t.Fatalf("placements = %#v", placements)
	}

	for name, input := range map[string][]api.ComputerSecret{
		"duplicate env": {
			{Name: "first", Env: &api.SecretEnv{Name: "TOKEN", Mode: "raw"}},
			{Name: "second", Env: &api.SecretEnv{Name: "TOKEN", Mode: "raw"}},
		},
		"nested file": {
			{Name: "first", File: &api.SecretFile{Path: "/run/secrets"}},
			{Name: "second", File: &api.SecretFile{Path: "/run/secrets/token"}},
		},
		"computer file": {
			{Name: "first", File: &api.SecretFile{Path: "/computer/token"}},
		},
		"reserved env": {
			{Name: "first", Env: &api.SecretEnv{Name: "HELMR_RUN_ID", Mode: "raw"}},
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
