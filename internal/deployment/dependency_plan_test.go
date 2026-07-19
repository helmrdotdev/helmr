package deployment

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDependencyPlanRoundTrip(t *testing.T) {
	for _, manager := range []PackageManagerName{
		PackageManagerBun,
		PackageManagerNPM,
	} {
		for _, architecture := range []RuntimeArchitecture{
			ArchitectureAArch64,
			ArchitectureX8664,
		} {
			t.Run(string(manager)+"/"+string(architecture), func(t *testing.T) {
				plan := dependencyPlanFixture(t, manager, architecture)
				raw, err := CanonicalDependencyPlan(plan)
				if err != nil {
					t.Fatal(err)
				}
				parsed, err := ParseDependencyPlan(raw)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(parsed, plan) {
					t.Fatalf("parsed plan = %#v, want %#v", parsed, plan)
				}
				if _, err := DependencyPlanDigest(parsed); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestDependencyPlanUsesClosedBunCommands(t *testing.T) {
	arm := dependencyPlanFixture(t, PackageManagerBun, ArchitectureAArch64)
	x86 := dependencyPlanFixture(t, PackageManagerBun, ArchitectureX8664)

	if !containsExact(arm.Resolution.Argv, "--cpu=arm64") ||
		!containsExact(x86.Resolution.Argv, "--cpu=x64") {
		t.Fatalf("Bun CPU projections are not architecture-specific")
	}
	if !containsExact(arm.Resolution.Argv, "--ignore-scripts") ||
		containsExact(arm.Lifecycle.Argv, "--ignore-scripts") ||
		!containsExact(arm.Lifecycle.Argv, "--concurrent-scripts=1") {
		t.Fatalf("Bun resolution/lifecycle commands are not closed")
	}
}

func TestDependencyPlanUsesManagedRuntimeForNPM(t *testing.T) {
	plan := dependencyPlanFixture(t, PackageManagerNPM, ArchitectureAArch64)
	prefix := []string{
		"/opt/helmr/runtime/bin/node",
		managerNPMEntrypoint,
	}
	common := []string{
		"--omit=dev",
		"--audit=false",
		"--fund=false",
		"--update-notifier=false",
		"--progress=false",
		"--install-strategy=hoisted",
		"--legacy-peer-deps=false",
		"--strict-peer-deps=false",
		"--userconfig=/work/config/npmrc",
		"--globalconfig=/work/config/global-npmrc",
		"--cache=/work/offline-store",
		"--registry=http://127.0.0.1:4873",
		"--workspaces=true",
		"--include-workspace-root=true",
	}
	want := map[string]struct {
		got  PlanCommand
		argv []string
		cwd  string
	}{
		"probe": {
			got:  plan.Probe,
			argv: append(append([]string(nil), prefix...), "--version"),
			cwd:  "/work",
		},
		"handshake": {
			got:  plan.Handshake,
			argv: append(append([]string(nil), prefix...), "ci", "--help"),
			cwd:  "/work",
		},
		"resolution": {
			got: plan.Resolution,
			argv: append(
				append(
					append([]string(nil), prefix...),
					"ci",
					"--ignore-scripts=true",
				),
				common...,
			),
			cwd: "/work/project",
		},
		"lifecycle": {
			got: plan.Lifecycle,
			argv: append(
				append(
					append([]string(nil), prefix...),
					"ci",
					"--ignore-scripts=false",
					"--offline=true",
					"--foreground-scripts=true",
				),
				common...,
			),
			cwd: "/work/project",
		},
	}
	for name, command := range want {
		if !reflect.DeepEqual(command.got.Argv, command.argv) ||
			command.got.CWD != command.cwd {
			t.Fatalf("%s = %#v, want argv %#v cwd %q", name, command.got, command.argv, command.cwd)
		}
	}
}

func TestDependencyPlanRejectsTemplateDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*DependencyPlan)
	}{
		{
			name: "environment",
			mutate: func(plan *DependencyPlan) {
				plan.Environment[1].Value += ":/usr/bin"
			},
		},
		{
			name: "aliases",
			mutate: func(plan *DependencyPlan) {
				plan.Aliases = plan.Aliases[:1]
			},
		},
		{
			name: "limits",
			mutate: func(plan *DependencyPlan) {
				plan.Limits.PIDs++
			},
		},
		{
			name: "mount",
			mutate: func(plan *DependencyPlan) {
				plan.Mounts.Manager = "/manager"
			},
		},
		{
			name: "operation",
			mutate: func(plan *DependencyPlan) {
				plan.Resolution.Argv = append(
					plan.Resolution.Argv,
					"--unknown",
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := dependencyPlanFixture(
				t,
				PackageManagerBun,
				ArchitectureAArch64,
			)
			test.mutate(&plan)
			if err := ValidateDependencyPlan(plan); err == nil {
				t.Fatal("ValidateDependencyPlan accepted template drift")
			}
		})
	}
}

func TestDependencyPlanRejectsNonCanonicalAndOpenDocuments(t *testing.T) {
	plan := dependencyPlanFixture(t, PackageManagerBun, ArchitectureAArch64)
	raw, err := CanonicalDependencyPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDependencyPlan(append([]byte(" "), raw...)); err == nil {
		t.Fatal("ParseDependencyPlan accepted non-canonical JSON")
	}

	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	object["extra"] = true
	open, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDependencyPlan(open); err == nil {
		t.Fatal("ParseDependencyPlan accepted an unknown member")
	}
}

func TestNewDependencyPlanRequiresOneArchitecture(t *testing.T) {
	capsule := managerCapsuleFixture(PackageManagerBun, ArchitectureAArch64)
	toolchain := dependencyPlanToolchain(ArchitectureX8664)
	if _, err := NewDependencyPlan(
		capsule,
		toolchain,
		DependencyMaterializerVersion,
	); err == nil {
		t.Fatal("NewDependencyPlan accepted mismatched architectures")
	}
}

func TestDependencyPlanHasStableDigest(t *testing.T) {
	plan := dependencyPlanFixture(t, PackageManagerBun, ArchitectureAArch64)
	raw, err := CanonicalDependencyPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"formatVersion":0`)) {
		t.Fatalf("canonical plan = %s", raw)
	}
	digest, err := DependencyPlanDigest(plan)
	if err != nil {
		t.Fatal(err)
	}
	const want = "sha256:59269e2a254dd6693c04ad2693d8fd314abca74dc915e1cfe899b980e6e704b0"
	if digest != want {
		t.Fatalf("dependency plan digest = %q, want %q", digest, want)
	}
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
		t.Fatalf("dependency plan digest = %q", digest)
	}
}

func dependencyPlanFixture(
	t *testing.T,
	manager PackageManagerName,
	architecture RuntimeArchitecture,
) DependencyPlan {
	t.Helper()
	plan, err := NewDependencyPlan(
		managerCapsuleFixture(manager, architecture),
		dependencyPlanToolchain(architecture),
		DependencyMaterializerVersion,
	)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func dependencyPlanToolchain(architecture RuntimeArchitecture) Toolchain {
	return Toolchain{
		Architecture:         architecture,
		FormatVersion:        ToolsetFormatVersion,
		ManagedRuntimeDigest: "sha256:" + strings.Repeat("3", 64),
		ToolchainClosure: ManagerArtifact{
			Digest:    "sha256:" + strings.Repeat("4", 64),
			MediaType: ToolchainMediaType,
			SizeBytes: 1,
		},
	}
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
