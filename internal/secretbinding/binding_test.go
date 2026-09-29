package secretbinding

import (
	"reflect"
	"testing"
)

func TestNormalizedPlacementsCanonicalizesAndRejectsConflicts(t *testing.T) {
	placements, err := NormalizedPlacements([]Binding{
		{Name: "config", File: &File{Path: "/run/helmr-secrets/config.json"}},
		{Name: "github", Env: &Env{Name: "GITHUB_TOKEN", Mode: "raw"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(placements) != 2 ||
		!reflect.DeepEqual(placements[0], Placement{Name: "github", Kind: "env", Target: "GITHUB_TOKEN", Mode: "raw"}) ||
		!reflect.DeepEqual(placements[1], Placement{Name: "config", Kind: "file", Target: "/run/helmr-secrets/config.json", Mode: "raw"}) {
		t.Fatalf("placements = %#v", placements)
	}

	for name, input := range map[string][]Binding{
		"duplicate env": {
			{Name: "first", Env: &Env{Name: "TOKEN", Mode: "raw"}},
			{Name: "second", Env: &Env{Name: "TOKEN", Mode: "raw"}},
		},
		"nested file": {
			{Name: "first", File: &File{Path: "/run/secrets"}},
			{Name: "second", File: &File{Path: "/run/secrets/token"}},
		},
		"computer file": {
			{Name: "first", File: &File{Path: "/workspace/token"}},
		},
		"reserved env": {
			{Name: "first", Env: &Env{Name: "HELMR_RUN_ID", Mode: "raw"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NormalizedPlacements(input); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
