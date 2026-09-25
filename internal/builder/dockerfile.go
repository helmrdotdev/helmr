package builder

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/distribution/reference"
)

const dockerfileFrontend = "docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e"

func ValidateBuilderImage(builderImage string) error {
	named, err := reference.ParseNormalizedNamed(builderImage)
	if err != nil || named.String() != builderImage {
		return errors.New("builder image must be a canonical fully qualified reference")
	}
	if _, ok := named.(reference.Canonical); !ok {
		return errors.New("builder image must be an exact lowercase sha256 reference")
	}
	return nil
}

// BuilderContext is the BuildKit named context every graph resolves
// helmr-builder through, so each stage starts from the exact pinned image.
func BuilderContext(builderImage string) (string, error) {
	if err := ValidateBuilderImage(builderImage); err != nil {
		return "", err
	}
	return "docker-image://" + builderImage, nil
}

// canonicalToolMounts shadow the two trees that hold Helmr's compiler, Runtime
// and builder with read-only views of the pinned image. A recipe runs as root
// in the environment, so whatever it left at these paths is never executed.
const canonicalToolMounts = "--mount=type=bind,from=" + BuilderContextName + ",source=/opt/helmr,target=/opt/helmr " +
	"--mount=type=bind,from=" + BuilderContextName + ",source=/nix,target=/nix "

// Named BuildKit contexts. helmr_environment is the one materialized build
// environment: the pinned builder itself, or the OCI layout produced once from
// the project's build.builder steps. helmr_config carries the resolved
// discovery config, which the target reads instead of importing the config.
const (
	BuilderContextName     = "helmr-builder"
	EnvironmentContextName = "helmr_environment"
	ConfigContextName      = "helmr_config"
)

// managedContextLines put a stage back under Helmr's own user-independent
// directories after the environment, whose preparation ran as root from /.
var managedContextLines = []string{
	"USER 0:0",
	"ENV HOME=/workspace/home TMPDIR=/workspace/tmp XDG_CACHE_HOME=/workspace/home/cache",
	"RUN [\"/bin/bash\",\"-euo\",\"pipefail\",\"-c\",\"install -d -o 65532 -g 65532 /workspace/home /workspace/output /workspace/project /workspace/tmp /workspace/work\"]",
	"WORKDIR /workspace/project",
}

// InstalledDockerfile is the only graph that runs user dependency lifecycle
// code. It exports only the resulting project tree as a producer-private OCI
// image so every later graph consumes the exact same installed bytes.
func InstalledDockerfile(install InstallPlan) ([]byte, error) {
	installInstruction, err := installRunInstruction(install)
	if err != nil {
		return nil, err
	}
	lines := []string{
		"# syntax=" + dockerfileFrontend,
		"FROM " + EnvironmentContextName + " AS installed",
	}
	lines = append(lines, managedContextLines...)
	lines = append(lines,
		"COPY --chown=65532:65532 . .",
		"USER 65532:65532",
		installInstruction,
		"FROM scratch AS installed-tree",
		"COPY --from=installed --chown=65532:65532 /workspace/project/ /workspace/project/",
		"",
	)
	return []byte(strings.Join(lines, "\n")), nil
}

// Dockerfile renders two networkless stages. The first is the last stage that
// executes tenant modules and exports only a closed prepared result. The
// second starts from the pinned builder image, never from the project's build
// environment, and performs static Program and bundle assembly from that
// result plus the exact installed-tree context.
func Dockerfile() ([]byte, error) {
	prepare, err := dockerRunJSON([]string{
		"/opt/helmr/bin/bundle-builder",
		"--project", "/workspace/project",
		"--work", "/workspace/work",
		"--prepare-output", "/workspace/output/prepared",
		"--runtime-descriptor", "/opt/helmr/release/runtime.descriptor.json",
		"--runtime-metadata", "/opt/helmr/runtime/helmr/runtime.json",
		"--compiler-descriptor", "/nix/helmr/compiler.descriptor.json",
		"--node", "/opt/helmr/runtime/bin/node",
		"--config", resolvedConfigPath,
		"--program-compiler", "/nix/helmr/program-compiler.mjs",
		"--encoder", "/opt/helmr/bin/mksquashfs",
	})
	if err != nil {
		return nil, err
	}
	finalizer, err := dockerRunJSON([]string{
		"/opt/helmr/bin/bundle-builder",
		"--prepared", "/workspace/prepared",
		"--program-project", "/workspace/program",
		"--work", "/workspace/work",
		"--bundle-output", "/workspace/output/bundle",
		"--workspace-images", "/workspace/images/images.json",
		"--mkfs", "/opt/helmr/bin/mke2fs",
		"--filesystem-config", "/opt/helmr/release/mke2fs.conf",
		"--expected-plan", "/workspace/images/build-plan.json",
		"--runtime-descriptor", "/opt/helmr/release/runtime.descriptor.json",
		"--runtime-metadata", "/opt/helmr/runtime/helmr/runtime.json",
		"--compiler-descriptor", "/nix/helmr/compiler.descriptor.json",
		"--encoder", "/opt/helmr/bin/mksquashfs",
	})
	if err != nil {
		return nil, err
	}
	lines := append(materializedDockerfileLines(),
		"FROM materialized AS prepared",
		"RUN --network=none "+canonicalToolMounts+prepare,
		"FROM "+BuilderContextName+" AS finalized",
		"USER 0:0",
		"RUN [\"/bin/bash\",\"-euo\",\"pipefail\",\"-c\",\"install -d -o 65532 -g 65532 /workspace/images /workspace/output /workspace/program /workspace/tmp /workspace/work && install -d -o 65532 -g 65532 /workspace/prepared\"]",
		"WORKDIR /workspace/program",
		"COPY --from=installed-tree --chown=65532:65532 /workspace/project/ /workspace/program/",
		"COPY --from=prepared --chown=65532:65532 /workspace/output/prepared/ /workspace/prepared/",
		"COPY --from=helmr_images --chown=65532:65532 / /workspace/images/",
		"USER 65532:65532",
		"RUN --network=none "+finalizer,
		"FROM scratch AS bundle",
		"COPY --from=finalized /workspace/output/bundle/ /",
		"",
	)
	return []byte(strings.Join(lines, "\n")), nil
}

func AnalysisDockerfile() ([]byte, error) {
	command, err := dockerRunJSON([]string{
		"/opt/helmr/bin/bundle-builder", "--project", "/workspace/project",
		"--work", "/workspace/work", "--analysis-output", "/workspace/output/build-plan.json",
		"--runtime-descriptor", "/opt/helmr/release/runtime.descriptor.json",
		"--runtime-metadata", "/opt/helmr/runtime/helmr/runtime.json",
		"--compiler-descriptor", "/nix/helmr/compiler.descriptor.json",
		"--node", "/opt/helmr/runtime/bin/node",
		"--config", resolvedConfigPath,
		"--program-compiler", "/nix/helmr/program-compiler.mjs",
		"--encoder", "/opt/helmr/bin/mksquashfs",
	})
	if err != nil {
		return nil, err
	}
	lines := append(materializedDockerfileLines(),
		"FROM materialized AS analyzed",
		"RUN --network=none "+canonicalToolMounts+command,
		"FROM scratch AS analysis",
		"COPY --from=analyzed /workspace/output/build-plan.json /build-plan.json",
		"",
	)
	return []byte(strings.Join(lines, "\n")), nil
}

// resolvedConfigPath is outside the installed project tree and owned by root;
// the CLI writes the file read-only. Declaration modules can read the
// discovery config but it is not theirs.
const resolvedConfigPath = "/workspace/config/config.json"

// materializedDockerfileLines starts declaration evaluation from the same
// materialized environment the install used, not from the install stage:
// prepared libraries are present for native imports, install-time mutations
// outside the project are not.
func materializedDockerfileLines() []string {
	lines := []string{
		"# syntax=" + dockerfileFrontend,
		"FROM helmr_installed AS installed-tree",
		"FROM " + EnvironmentContextName + " AS materialized",
	}
	lines = append(lines, managedContextLines...)
	return append(lines,
		"COPY --from=installed-tree --chown=0:0 /workspace/project/ /workspace/project/",
		"COPY --from="+ConfigContextName+" --chown=0:0 /config.json "+resolvedConfigPath,
		"RUN [\"/bin/bash\",\"-euo\",\"pipefail\",\"-c\",\"chown -R 0:0 /workspace/project && chmod -R a-w /workspace/project\"]",
		"USER 65532:65532",
	)
}

func installRunInstruction(plan InstallPlan) (string, error) {
	secretIDs, err := NormalizeSecretIDs(plan.SecretIDs)
	if err != nil {
		return "", err
	}
	var mounts strings.Builder
	for _, id := range secretIDs {
		mounts.WriteString("--mount=type=secret,id=" + id + ",uid=65532,gid=65532,mode=0400,required=true ")
	}
	if plan.CustomCommand != "" {
		if len(plan.CustomCommand) > 16<<10 || strings.IndexByte(plan.CustomCommand, 0) >= 0 {
			return "", errors.New("custom install command is invalid")
		}
		command, err := dockerRunJSON([]string{
			"/bin/bash", "-euo", "pipefail", "-c", plan.CustomCommand,
		})
		if err != nil {
			return "", err
		}
		return "RUN " + mounts.String() + command, nil
	}
	if len(plan.Argv) == 0 {
		return "", errors.New("install plan is empty")
	}
	command, err := dockerRunJSON(plan.Argv)
	if err != nil {
		return "", err
	}
	return "RUN " + mounts.String() + command, nil
}

func dockerRunJSON(argv []string) (string, error) {
	for _, argument := range argv {
		if argument == "" || strings.IndexByte(argument, 0) >= 0 || strings.ContainsAny(argument, "\r\n") {
			return "", fmt.Errorf("BuildKit command argument is invalid")
		}
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(argv); err != nil {
		return "", err
	}
	return strings.TrimSuffix(encoded.String(), "\n"), nil
}
