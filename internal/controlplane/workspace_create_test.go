package controlplane

import (
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func TestNormalizeWorkspaceSecretPlacementsCanonicalizesAndRejectsConflicts(t *testing.T) {
	placements, err := normalizeWorkspaceSecretPlacements([]api.WorkspaceSecret{
		{Name: "config", File: &api.SecretFile{Path: "/run/helmr-secrets/config.json"}},
		{Name: "github", Env: &api.SecretEnv{Name: "GITHUB_TOKEN", Mode: "raw"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(placements) != 2 ||
		!reflect.DeepEqual(placements[0], workspace.SecretPlacement{Name: "github", Kind: "env", Target: "GITHUB_TOKEN", Mode: "raw"}) ||
		!reflect.DeepEqual(placements[1], workspace.SecretPlacement{Name: "config", Kind: "file", Target: "/run/helmr-secrets/config.json", Mode: "raw"}) {
		t.Fatalf("placements = %#v", placements)
	}

	for name, input := range map[string][]api.WorkspaceSecret{
		"duplicate env": {
			{Name: "first", Env: &api.SecretEnv{Name: "TOKEN", Mode: "raw"}},
			{Name: "second", Env: &api.SecretEnv{Name: "TOKEN", Mode: "raw"}},
		},
		"nested file": {
			{Name: "first", File: &api.SecretFile{Path: "/run/secrets"}},
			{Name: "second", File: &api.SecretFile{Path: "/run/secrets/token"}},
		},
		"workspace file": {
			{Name: "first", File: &api.SecretFile{Path: "/workspace/token"}},
		},
		"reserved env": {
			{Name: "first", Env: &api.SecretEnv{Name: "HELMR_RUN_ID", Mode: "raw"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeWorkspaceSecretPlacements(input); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestValidateWorkspaceKeyPreservesExactBytes(t *testing.T) {
	valid := " repository "
	if err := validateWorkspaceKey(&valid); err == nil {
		t.Fatal("expected edge whitespace to be rejected without normalization")
	}
	exact := "répository"
	if err := validateWorkspaceKey(&exact); err != nil {
		t.Fatalf("valid exact UTF-8 key: %v", err)
	}
}

func TestWorkspacePublicStatusUsesPublicSpelling(t *testing.T) {
	status, err := workspacePublicStatus("recovery_required")
	if err != nil {
		t.Fatal(err)
	}
	if status != api.WorkspaceStatusRecoveryRequired || string(status) != "recovery_required" {
		t.Fatalf("recovery status = %q", status)
	}
}
