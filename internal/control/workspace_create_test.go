package control

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestNormalizeWorkspaceSecretPlacementsCanonicalizesAndRejectsConflicts(t *testing.T) {
	placements, err := normalizeWorkspaceSecretPlacements([]api.WorkspaceSecret{
		{Name: "config", File: "/run/helmr-secrets/config.json"},
		{Name: "github", Env: "GITHUB_TOKEN"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(placements) != 2 ||
		placements[0] != (workspaceSecretPlacement{Name: "github", Kind: "env", Target: "GITHUB_TOKEN"}) ||
		placements[1] != (workspaceSecretPlacement{Name: "config", Kind: "file", Target: "/run/helmr-secrets/config.json"}) {
		t.Fatalf("placements = %#v", placements)
	}

	for name, input := range map[string][]api.WorkspaceSecret{
		"duplicate env": {
			{Name: "first", Env: "TOKEN"},
			{Name: "second", Env: "TOKEN"},
		},
		"nested file": {
			{Name: "first", File: "/run/secrets"},
			{Name: "second", File: "/run/secrets/token"},
		},
		"workspace file": {
			{Name: "first", File: "/workspace/token"},
		},
		"reserved env": {
			{Name: "first", Env: "HELMR_RUN_ID"},
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
