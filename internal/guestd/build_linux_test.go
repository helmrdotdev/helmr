//go:build linux

package guestd

import (
	"slices"
	"testing"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

func TestBuildInstallCommandUsesManagerFrozenSemantics(t *testing.T) {
	tests := []struct {
		name       deployment.PackageManagerName
		entrypoint deployment.ManagerEntrypoint
		want       []string
	}{
		{
			name: deployment.PackageManagerNPM,
			entrypoint: deployment.ManagerEntrypoint{
				Kind: deployment.ManagerEntrypointNode,
				Path: "/opt/helmr/manager/bin/npm-cli.js",
			},
			want: []string{
				"/opt/helmr/runtime/bin/node",
				"/opt/helmr/manager/bin/npm-cli.js",
				"ci",
				"--no-audit",
				"--no-fund",
			},
		},
		{
			name: deployment.PackageManagerPNPM,
			entrypoint: deployment.ManagerEntrypoint{
				Kind: deployment.ManagerEntrypointNode,
				Path: "/opt/helmr/manager/bin/pnpm.cjs",
			},
			want: []string{
				"/opt/helmr/runtime/bin/node",
				"/opt/helmr/manager/bin/pnpm.cjs",
				"install",
				"--frozen-lockfile",
				"--no-runtime",
				"--pm-on-fail=error",
			},
		},
		{
			name: deployment.PackageManagerBun,
			entrypoint: deployment.ManagerEntrypoint{
				Kind: deployment.ManagerEntrypointNative,
				Path: "/opt/helmr/manager/bin/bun",
			},
			want: []string{
				"/opt/helmr/manager/bin/bun",
				"install",
				"--frozen-lockfile",
			},
		},
	}
	for _, test := range tests {
		t.Run(string(test.name), func(t *testing.T) {
			command, err := buildInstallCommand(deployment.BuildManager{
				Entrypoint: test.entrypoint,
				PackageManager: deployment.PackageManager{
					Name: test.name, Version: "1.0.0",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(command.Argv, test.want) {
				t.Fatalf("install command = %q, want %q", command.Argv, test.want)
			}
			if command.CWD != "/work/project" {
				t.Fatalf("install cwd = %q", command.CWD)
			}
		})
	}
}

func TestBuildProcessEnvironmentDoesNotOverrideManagerConfig(t *testing.T) {
	want := []buildEnvironment{
		{Name: "HOME", Value: "/work/home"},
		{
			Name:  "PATH",
			Value: "/opt/helmr/manager/bin:/opt/helmr/runtime/bin:/nix/bin",
		},
		{Name: "TMPDIR", Value: "/tmp"},
		{Name: "XDG_CACHE_HOME", Value: "/work/home/cache"},
	}
	if environment := buildProcessEnvironment(); !slices.Equal(environment, want) {
		t.Fatalf("build environment = %+v, want %+v", environment, want)
	}
}

func TestBuildAliasesDoNotExposeHostOrManagerLoader(t *testing.T) {
	want := []buildAlias{
		{Path: "/bin/sh", Target: "/nix/bin/sh"},
		{Path: "/usr/bin/env", Target: "/nix/bin/env"},
	}
	if aliases := buildAliases(); !slices.Equal(aliases, want) {
		t.Fatalf("build aliases = %+v, want %+v", aliases, want)
	}
}
