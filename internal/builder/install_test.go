package builder

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSelectInstallPlanRespectsProducerChoiceWithoutVersionAdmission(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		lockfile string
		want     []string
	}{
		{
			name: "npm future", selector: "npm@99.4.0", lockfile: "package-lock.json",
			want: []string{"corepack", "npm@99.4.0", "ci", "--no-audit", "--no-fund"},
		},
		{
			name: "pnpm integrity", selector: "pnpm@42.1.0+sha256.deadbeef", lockfile: "pnpm-lock.yaml",
			want: []string{"corepack", "pnpm@42.1.0+sha256.deadbeef", "install", "--frozen-lockfile"},
		},
		{
			name: "bun", selector: "bun@7.0.0-beta.2", lockfile: "bun.lock",
			want: []string{"/usr/local/bin/bun-for-version", "7.0.0-beta.2", "install", "--frozen-lockfile"},
		},
		{
			name: "yarn", selector: "yarn@8.0.0", lockfile: "yarn.lock",
			want: []string{"corepack", "yarn@8.0.0", "install", "--immutable"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeInstallProject(t, test.selector, test.lockfile)
			plan, err := SelectInstallPlan(root, "")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(plan.Argv, test.want) || plan.CustomCommand != "" {
				t.Fatalf("plan = %+v, want %q", plan, test.want)
			}
		})
	}
}

func TestSelectInstallPlanUsesCustomCommandWithoutInspectingManager(t *testing.T) {
	root := writeInstallProject(t, "made-up@1.0.0", "")
	plan, err := SelectInstallPlan(root, "  ./scripts/prepare.sh --offline  ")
	if err != nil {
		t.Fatal(err)
	}
	if plan.CustomCommand != "./scripts/prepare.sh --offline" || plan.Argv != nil {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestSelectInstallPlanRejectsOnlyAmbiguousOrUnsafeProducerMetadata(t *testing.T) {
	root := writeInstallProject(t, "", "pnpm-lock.yaml")
	if err := os.WriteFile(filepath.Join(root, "yarn.lock"), []byte("lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SelectInstallPlan(root, ""); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous lockfile error = %v", err)
	}

	root = writeInstallProject(t, "npm@"+strings.Repeat("a", 513), "")
	if _, err := SelectInstallPlan(root, ""); err == nil || !strings.Contains(err.Error(), "exact SemVer") {
		t.Fatalf("unsafe selector error = %v", err)
	}

	root = writeInstallProject(t, "bun@latest", "")
	if _, err := SelectInstallPlan(root, ""); err == nil || !strings.Contains(err.Error(), "exact SemVer") {
		t.Fatalf("non-exact Bun selector error = %v", err)
	}

	for _, selector := range []string{
		"npm@latest",
		"pnpm@^10.0.0",
		"pnpm@https://packages.example/pnpm.tgz",
		"yarn@4.x",
		"npm@1.0.0-01",
		"bun@1.3.10+sha256.deadbeef",
	} {
		root = writeInstallProject(t, selector, "")
		if _, err := SelectInstallPlan(root, ""); err == nil {
			t.Fatalf("mutable or unsupported selector %q was accepted", selector)
		}
	}
}

func TestNormalizeSecretIDsKeepsOnlyCanonicalNames(t *testing.T) {
	got, err := NormalizeSecretIDs([]string{"NPM_TOKEN", "GITHUB_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"GITHUB_TOKEN", "NPM_TOKEN"}) {
		t.Fatalf("secret IDs = %q", got)
	}
	for _, values := range [][]string{{"npm-token"}, {"TOKEN", "TOKEN"}, {""}} {
		if _, err := NormalizeSecretIDs(values); err == nil {
			t.Fatalf("invalid secret IDs were accepted: %q", values)
		}
	}
}

func writeInstallProject(t *testing.T, selector, lockfile string) string {
	t.Helper()
	root := t.TempDir()
	manifest := `{"name":"fixture","private":true}`
	if selector != "" {
		manifest = `{"name":"fixture","packageManager":"` + selector + `","private":true}`
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if lockfile != "" {
		if err := os.WriteFile(filepath.Join(root, lockfile), []byte("lock"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
