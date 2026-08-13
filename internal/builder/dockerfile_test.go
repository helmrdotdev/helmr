package builder

import (
	"strings"
	"testing"
)

func TestDockerfileKeepsUserInstallBeforeNetworklessFinalizer(t *testing.T) {
	image := "ghcr.io/helmrdotdev/helmr-bundle-builder@sha256:" + strings.Repeat("a", 64)
	raw, err := Dockerfile(image, InstallPlan{Argv: []string{
		"npx", "--yes", "yarn@8.0.0", "install", "--immutable",
	}})
	if err != nil {
		t.Fatal(err)
	}
	document := string(raw)
	install := `RUN ["npx","--yes","yarn@8.0.0","install","--immutable"]`
	finalizer := `RUN --network=none ["/usr/local/bin/bundle-builder"`
	if !strings.Contains(document, "FROM "+image+" AS installed") ||
		!strings.Contains(document, install) ||
		!strings.Contains(document, finalizer) ||
		!strings.Contains(document, "/workspace/output") ||
		!strings.Contains(document, "--bundle-output\",\"/workspace/output/bundle") ||
		!strings.Contains(document, "COPY --from=finalized /workspace/output/bundle/ /") ||
		strings.Index(document, install) >= strings.Index(document, finalizer) {
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

func TestDockerfileQuotesCustomInstallAsOneBuildKitArgument(t *testing.T) {
	image := "registry.example/helmr/builder@sha256:" + strings.Repeat("b", 64)
	raw, err := Dockerfile(image, InstallPlan{CustomCommand: `./prepare.sh "quoted value" && yarn install`})
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

func TestDockerfileRejectsMutableBuilderReference(t *testing.T) {
	if _, err := Dockerfile("ghcr.io/helmrdotdev/helmr-bundle-builder:latest", InstallPlan{Argv: []string{"npm", "ci"}}); err == nil {
		t.Fatal("Dockerfile accepted a mutable builder tag")
	}
}
