package builder

import (
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/hostconfig"
)

type renderedStage struct {
	name, base   string
	instructions []string
}

// renderedStages splits a generated Dockerfile into its stages. Generated
// graphs put every instruction on one line, so lines are instructions.
func renderedStages(t *testing.T, raw []byte) []renderedStage {
	t.Helper()
	var stages []renderedStage
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "FROM ") {
			fields := strings.Fields(line)
			if len(fields) != 4 || fields[2] != "AS" {
				t.Fatalf("stage line %q is not FROM base AS name", line)
			}
			stages = append(stages, renderedStage{name: fields[3], base: fields[1]})
			continue
		}
		if len(stages) == 0 {
			t.Fatalf("instruction %q precedes every stage", line)
		}
		last := &stages[len(stages)-1]
		last.instructions = append(last.instructions, line)
	}
	return stages
}

func stageNamed(t *testing.T, stages []renderedStage, name string) renderedStage {
	t.Helper()
	for _, stage := range stages {
		if stage.name == name {
			return stage
		}
	}
	t.Fatalf("stage %q is missing", name)
	return renderedStage{}
}

func TestEnvironmentRunsPreparationAsRootFromCapturedSourceInOrder(t *testing.T) {
	raw, err := EnvironmentDockerfile([]hostconfig.Step{
		{Kind: "copy", Source: "build/setup.sh", Destination: "/opt/setup.sh"},
		{Kind: "run", Argv: []string{"/bin/sh", "/opt/setup.sh", "with \"quotes\""}},
		{Kind: "copy", Source: "build/assets[1]/a[1].txt", Destination: "/opt/literal.txt"},
		{Kind: "copy", Source: "build/$NAME[1].txt", Destination: "/opt/$NAME/result.txt"},
		{Kind: "copy", Source: ".", Destination: "/opt/project"},
	})
	if err != nil {
		t.Fatal(err)
	}
	stages := renderedStages(t, raw)
	if len(stages) != 1 || stages[0].name != "environment" || stages[0].base != "helmr-builder" {
		t.Fatalf("stages = %+v", stages)
	}
	want := []string{
		"USER 0:0",
		`RUN ["/bin/bash","-euo","pipefail","-c","install -d /root/.cache"]`,
		"WORKDIR /",
		"ENV HOME=/root TMPDIR=/tmp XDG_CACHE_HOME=/root/.cache",
		`COPY ["/build/setup.sh","/opt/setup.sh"]`,
		`RUN ["/bin/sh","/opt/setup.sh","with \"quotes\""]`,
		`COPY ["/build/assets[[]1]/a[[]1].txt","/opt/literal.txt"]`,
		`COPY ["/build/\\$NAME[[]1].txt","/opt/\\$NAME/result.txt"]`,
		`COPY ["/","/opt/project"]`,
	}
	if strings.Join(stages[0].instructions, "\n") != strings.Join(want, "\n") {
		t.Fatalf("environment:\n%s", strings.Join(stages[0].instructions, "\n"))
	}
	for _, forbidden := range []string{"--from=", "type=secret", "PATH="} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("environment graph contains %q", forbidden)
		}
	}
}

func TestPreparationSeparatesCompilerInstallAndAnalysis(t *testing.T) {
	raw, err := PreparationDockerfile()
	if err != nil {
		t.Fatal(err)
	}
	stages := renderedStages(t, raw)
	for _, item := range []struct{ name, base string }{{"bundled", "scratch"}, {"runtime-installed", "helmr_environment"}, {"assembled", "helmr-builder"}, {"analyzed", "helmr_environment"}, {"preparation", "scratch"}} {
		if stageNamed(t, stages, item.name).base != item.base {
			t.Fatalf("wrong base for %s", item.name)
		}
	}
	bundled := strings.Join(stageNamed(t, stages, "bundled").instructions, "\n")
	if strings.Contains(bundled, "source=/nix,target=/nix") || !strings.Contains(bundled, "--mount=type=bind,from=helmr_installed") || !strings.Contains(bundled, "--network=none") {
		t.Fatal("bundling input boundary is not restricted and read-only")
	}
	analyzed := strings.Join(stageNamed(t, stages, "analyzed").instructions, "\n")
	if strings.Contains(analyzed, "from=helmr_installed") || !strings.Contains(analyzed, "/workspace/output/payload/") || !strings.Contains(analyzed, "--network=none "+canonicalToolMounts) {
		t.Fatal("analysis must receive only payload and canonical tools")
	}
	installed := strings.Join(stageNamed(t, stages, "runtime-installed").instructions, "\n")
	if !strings.Contains(installed, "--install-runtime") || !strings.Contains(installed, "rm -rf /workspace/project") {
		t.Fatal("runtime install must be fresh")
	}
}

func TestInstallIsTheOnlyStageWithNetworkSecretsAndProjectSource(t *testing.T) {
	raw, err := InstalledDockerfile(InstallPlan{
		Argv: []string{"corepack", "yarn@8.0.0", "install", "--immutable"}, SecretIDs: []string{"NPM_TOKEN"},
	})
	if err != nil {
		t.Fatal(err)
	}
	stages := renderedStages(t, raw)
	installed := stageNamed(t, stages, "installed")
	want := []string{
		"USER 0:0",
		"ENV HOME=/workspace/home TMPDIR=/workspace/tmp XDG_CACHE_HOME=/workspace/home/cache",
		`RUN ["/bin/bash","-euo","pipefail","-c","rm -rf /workspace/project /workspace/home /workspace/output /workspace/tmp /workspace/work && install -d -o 65532 -g 65532 /workspace/home /workspace/output /workspace/project /workspace/tmp /workspace/work"]`,
		"WORKDIR /workspace/project",
		"COPY --chown=65532:65532 . .",
		"USER 65532:65532",
		`RUN --mount=type=secret,id=NPM_TOKEN,uid=65532,gid=65532,mode=0400,required=true ["corepack","yarn@8.0.0","install","--immutable"]`,
	}
	if strings.Join(installed.instructions, "\n") != strings.Join(want, "\n") {
		t.Fatalf("install stage:\n%s", strings.Join(installed.instructions, "\n"))
	}
	tree := stageNamed(t, stages, "installed-tree")
	if tree.base != "scratch" || len(tree.instructions) != 1 ||
		tree.instructions[0] != "COPY --from=installed --chown=65532:65532 /workspace/project/ /workspace/project/" {
		t.Fatalf("exported tree = %+v", tree)
	}
	for _, forbidden := range []string{"docker.sock", "--privileged", "security.insecure", "--network=host", "--mount=type=ssh"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("Dockerfile contains forbidden surface %q", forbidden)
		}
	}
}

func TestFinalizerNeverSeesTheEnvironmentOrExecutesTenantStages(t *testing.T) {
	raw, err := Dockerfile()
	if err != nil {
		t.Fatal(err)
	}
	stages := renderedStages(t, raw)
	finalized := stageNamed(t, stages, "finalized")
	if finalized.base != "helmr-builder" {
		t.Fatalf("finalizer starts from %q, want the pinned builder", finalized.base)
	}
	copies := 0
	for _, instruction := range finalized.instructions {
		if !strings.HasPrefix(instruction, "COPY") {
			continue
		}
		copies++
		if !strings.Contains(instruction, "--from=helmr_prepared ") &&
			!strings.Contains(instruction, "--from=helmr_images ") {
			t.Fatalf("finalizer takes unexpected input: %s", instruction)
		}
	}
	if copies != 2 {
		t.Fatalf("finalizer has %d inputs, want prepared output and images", copies)
	}
	last := finalized.instructions[len(finalized.instructions)-1]
	if !strings.HasPrefix(last, `RUN --network=none ["/opt/helmr/bin/bundle-builder","--prepared","/workspace/prepared"`) {
		t.Fatalf("finalizer run = %s", last)
	}
	if strings.Contains(last, "--node") || strings.Contains(last, "--config") {
		t.Fatalf("finalizer can execute or configure modules: %s", last)
	}
	bundle := stageNamed(t, stages, "bundle")
	if bundle.base != "scratch" || len(bundle.instructions) != 1 || bundle.instructions[0] != "COPY --from=finalized /workspace/output/bundle/ /" {
		t.Fatalf("bundle export = %+v", bundle)
	}
}

func TestInstalledDockerfileQuotesCustomInstallAsOneBuildKitArgument(t *testing.T) {
	raw, err := InstalledDockerfile(InstallPlan{CustomCommand: `./prepare.sh "quoted value" && yarn install`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		string(raw),
		`RUN ["/bin/bash","-euo","pipefail","-c","./prepare.sh \"quoted value\" && yarn install"]`,
	) {
		t.Fatalf("custom command was not JSON-quoted:\n%s", raw)
	}
}

func TestBuilderContextRequiresAnExactDigest(t *testing.T) {
	pinned := "ghcr.io/helmrdotdev/bundle-builder@sha256:" + strings.Repeat("a", 64)
	context, err := BuilderContext(pinned)
	if err != nil || context != "docker-image://"+pinned {
		t.Fatalf("context = %q, %v", context, err)
	}
	for _, mutable := range []string{"ghcr.io/helmrdotdev/bundle-builder:latest", "bundle-builder@sha256:" + strings.Repeat("a", 64), ""} {
		if _, err := BuilderContext(mutable); err == nil {
			t.Fatalf("mutable or non-canonical builder reference %q was accepted", mutable)
		}
	}
}
