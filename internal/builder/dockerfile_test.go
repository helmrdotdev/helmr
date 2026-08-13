package builder

import (
	"strings"
	"testing"
)

func TestInstalledDockerfileIsTheOnlyGraphThatRunsUserInstall(t *testing.T) {
	image := "ghcr.io/helmrdotdev/helmr-bundle-builder@sha256:" + strings.Repeat("a", 64)
	raw, err := InstalledDockerfile(image, InstallPlan{Argv: []string{
		"npx", "--yes", "yarn@8.0.0", "install", "--immutable",
	}})
	if err != nil {
		t.Fatal(err)
	}
	document := string(raw)
	install := `RUN ["npx","--yes","yarn@8.0.0","install","--immutable"]`
	if !strings.Contains(document, "FROM "+image+" AS installed") ||
		!strings.Contains(document, install) ||
		!strings.Contains(document, "FROM scratch AS installed-tree") ||
		!strings.Contains(document, "COPY --from=installed --chown=65532:65532 /workspace/project/ /workspace/project/") {
		t.Fatalf("Dockerfile boundary is invalid:\n%s", document)
	}
	for _, forbidden := range []string{
		"docker.sock", "--privileged", "security.insecure", "--network=host", "--mount=type=ssh",
	} {
		if strings.Contains(document, forbidden) {
			t.Fatalf("Dockerfile contains forbidden surface %q", forbidden)
		}
	}
}

func TestFinalGraphsUseOnlyTheInstalledOCIContext(t *testing.T) {
	image := "ghcr.io/helmrdotdev/helmr-bundle-builder@sha256:" + strings.Repeat("a", 64)
	for name, render := range map[string]func(string) ([]byte, error){
		"analysis": AnalysisDockerfile,
		"final":    Dockerfile,
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := render(image)
			if err != nil {
				t.Fatal(err)
			}
			document := string(raw)
			for _, required := range []string{
				"FROM helmr_installed AS installed-tree",
				"FROM " + image + " AS materialized",
				"COPY --from=installed-tree --chown=0:0 /workspace/project/ /workspace/project/",
				`RUN ["/bin/bash","-euo","pipefail","-c","chown -R 0:0 /workspace/project && chmod -R a-w /workspace/project"]`,
				"RUN --network=none",
			} {
				if !strings.Contains(document, required) {
					t.Fatalf("Dockerfile is missing %q:\n%s", required, document)
				}
			}
			for _, forbidden := range []string{"npm ci", "pnpm install", "yarn install", "bun install", "COPY --chown=65532:65532 . ."} {
				if strings.Contains(document, forbidden) {
					t.Fatalf("downstream Dockerfile repeats producer operation %q:\n%s", forbidden, document)
				}
			}
		})
	}
	raw, err := Dockerfile(image)
	if err != nil {
		t.Fatal(err)
	}
	document := string(raw)
	for _, required := range []string{
		"FROM materialized AS prepared",
		`"--project","/workspace/project","--work","/workspace/work","--prepare-output","/workspace/output/prepared"`,
		"FROM " + image + " AS finalized",
		"COPY --from=installed-tree --chown=65532:65532 /workspace/project/ /workspace/program/",
		"COPY --from=prepared --chown=65532:65532 /workspace/output/prepared/ /workspace/prepared/",
		`"--prepared","/workspace/prepared","--program-project","/workspace/program"`,
	} {
		if !strings.Contains(document, required) {
			t.Fatalf("final Dockerfile is missing %q:\n%s", required, document)
		}
	}
	prepareRun := strings.Index(document, `"--prepare-output","/workspace/output/prepared"`)
	programCopy := strings.Index(document, "COPY --from=installed-tree --chown=65532:65532 /workspace/project/ /workspace/program/")
	finalRun := strings.Index(document, `"--prepared","/workspace/prepared"`)
	if prepareRun < 0 || programCopy <= prepareRun || finalRun <= programCopy {
		t.Fatalf("final Dockerfile does not isolate tenant execution from Program assembly:\n%s", document)
	}
}

func TestInstalledDockerfileQuotesCustomInstallAsOneBuildKitArgument(t *testing.T) {
	image := "registry.example/helmr/builder@sha256:" + strings.Repeat("b", 64)
	raw, err := InstalledDockerfile(image, InstallPlan{CustomCommand: `./prepare.sh "quoted value" && yarn install`})
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

func TestDockerfilesRejectMutableBuilderReference(t *testing.T) {
	mutable := "ghcr.io/helmrdotdev/helmr-bundle-builder:latest"
	if _, err := InstalledDockerfile(mutable, InstallPlan{Argv: []string{"npm", "ci"}}); err == nil {
		t.Fatal("InstalledDockerfile accepted a mutable builder tag")
	}
	if _, err := AnalysisDockerfile(mutable); err == nil {
		t.Fatal("AnalysisDockerfile accepted a mutable builder tag")
	}
	if _, err := Dockerfile(mutable); err == nil {
		t.Fatal("Dockerfile accepted a mutable builder tag")
	}
}
