package builder

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/distribution/reference"

	"github.com/helmrdotdev/helmr/internal/version"
)

const dockerfileFrontend = "docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e"

func validateBuilderImage(builderImage string) error {
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
	if err := validateBuilderImage(builderImage); err != nil {
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
	"RUN [\"/bin/bash\",\"-euo\",\"pipefail\",\"-c\",\"rm -rf /workspace/project /workspace/home /workspace/output /workspace/tmp /workspace/work && install -d -o 65532 -g 65532 /workspace/home /workspace/output /workspace/project /workspace/tmp /workspace/work\"]",
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

// Dockerfile performs static finalization of the exact prepared payload.
func Dockerfile() ([]byte, error) {
	finalizer, err := dockerRunJSON([]string{
		"/opt/helmr/bin/bundle-builder", "--prepared", "/workspace/prepared",
		"--work", "/workspace/work", "--bundle-output", "/workspace/output/bundle",
		"--computer-images", "/workspace/images/images.json", "--mkfs", "/opt/helmr/bin/mke2fs",
		"--filesystem-config", "/opt/helmr/release/mke2fs.conf", "--expected-plan", "/workspace/images/build-plan.json",
		"--runtime-descriptor", "/opt/helmr/release/runtime.descriptor.json",
		"--runtime-metadata", "/opt/helmr/runtime/helmr/runtime.json",
		"--compiler-descriptor", "/nix/helmr/compiler.descriptor.json", "--encoder", "/opt/helmr/bin/mksquashfs",
	})
	if err != nil {
		return nil, err
	}
	lines := []string{
		"# syntax=" + dockerfileFrontend,
		"FROM " + BuilderContextName + " AS finalized",
	}
	lines = append(lines, managedContextLines...)
	lines = append(lines,
		"COPY --from=helmr_prepared --chown=65532:65532 / /workspace/prepared/",
		"COPY --from=helmr_images --chown=65532:65532 / /workspace/images/",
		"USER 65532:65532", "RUN --network=none "+finalizer,
		"FROM scratch AS bundle", "COPY --from=finalized /workspace/output/bundle/ /", "",
	)
	return []byte(strings.Join(lines, "\n")), nil
}

// PreparationDockerfile confines source transformation to the compiler closure,
// installs runtime packages once, and analyzes only the selective payload.
func PreparationDockerfile() ([]byte, error) {
	bundle, err := dockerRunJSON([]string{
		"/opt/helmr/runtime/bin/node", "--no-strip-types", "--no-global-search-paths",
		"/nix/helmr/program-compiler.mjs", "--bundle", "/workspace/project", resolvedConfigPath, version.Node(), "/workspace/output/bundle",
	})
	if err != nil {
		return nil, err
	}
	install, err := dockerRunJSON([]string{
		"/opt/helmr/runtime/bin/node", "--no-strip-types", "--no-global-search-paths", "/nix/helmr/program-compiler.mjs", "--install-runtime",
	})
	if err != nil {
		return nil, err
	}
	assemble, err := dockerRunJSON([]string{
		"/opt/helmr/runtime/bin/node", "--no-strip-types", "--no-global-search-paths",
		"/nix/helmr/program-compiler.mjs", "--assemble", "/workspace/bundle", "/workspace/installed", "/workspace/output/payload",
	})
	if err != nil {
		return nil, err
	}
	analyze, err := dockerRunJSON([]string{
		"/opt/helmr/bin/bundle-builder", "--project", "/workspace/project",
		"--work", "/workspace/work", "--prepare-output", "/workspace/output/prepared",
		"--runtime-descriptor", "/opt/helmr/release/runtime.descriptor.json",
		"--runtime-metadata", "/opt/helmr/runtime/helmr/runtime.json",
		"--compiler-descriptor", "/nix/helmr/compiler.descriptor.json",
		"--node", "/opt/helmr/runtime/bin/node", "--config", resolvedConfigPath,
		"--bundle-manifest", "/workspace/bundle.json",
		"--program-compiler", "/nix/helmr/program-compiler.mjs",
	})
	if err != nil {
		return nil, err
	}
	lines := []string{
		"# syntax=" + dockerfileFrontend,
		"FROM scratch AS bundled",
		"COPY --from=" + BuilderContextName + " /opt/helmr/runtime/bin/ /opt/helmr/runtime/bin/",
		"COPY --from=" + BuilderContextName + " /opt/helmr/runtime/lib/ /opt/helmr/runtime/lib/",
		"COPY --from=" + BuilderContextName + " /nix/helmr/ /nix/helmr/",
		"COPY --from=" + BuilderContextName + " --chown=65532:65532 --chmod=0755 /opt/helmr/empty/ /workspace/output/",
		"COPY --from=" + BuilderContextName + " --chown=65532:65532 --chmod=0755 /opt/helmr/empty/ /workspace/tmp/",
		"COPY --from=" + ConfigContextName + " --chown=0:0 --chmod=0444 /config.json " + resolvedConfigPath,
		"ENV TMPDIR=/workspace/tmp HOME=/workspace/tmp",
		"WORKDIR /workspace/project", "USER 65532:65532", "RUN --network=none --mount=type=bind,from=helmr_installed,source=/workspace/project,target=/workspace/project " + bundle,
		"FROM " + EnvironmentContextName + " AS runtime-installed",
	}
	lines = append(lines, managedContextLines...)
	lines = append(lines,
		"ENV NODE_OPTIONS= NODE_PATH= PATH=/opt/helmr/runtime/bin:/usr/local/bin:/usr/bin:/bin",
		"COPY --from=bundled --chown=65532:65532 /workspace/output/bundle/install/ /workspace/project/",
		"USER 65532:65532",
		"RUN --mount=type=cache,target=/workspace/npm-cache,uid=65532,gid=65532 "+canonicalToolMounts+install,
		"FROM "+BuilderContextName+" AS assembled",
	)
	lines = append(lines, managedContextLines...)
	lines = append(lines,
		"COPY --from=bundled --chown=0:0 /workspace/output/bundle/ /workspace/bundle/",
		"COPY --from=runtime-installed --chown=0:0 /workspace/project/ /workspace/installed/",
		"USER 65532:65532", "RUN --network=none "+assemble,
		"FROM "+EnvironmentContextName+" AS analyzed",
	)
	lines = append(lines, managedContextLines...)
	lines = append(lines,
		"COPY --from=assembled --chown=0:0 /workspace/output/payload/ /workspace/project/",
		"COPY --from=bundled --chown=0:0 /workspace/output/bundle/bundle.json /workspace/bundle.json",
		"COPY --from="+ConfigContextName+" --chown=0:0 --chmod=0444 /config.json "+resolvedConfigPath,
		`RUN ["/bin/bash","-euo","pipefail","-c","chmod -R a-w /workspace/project"]`,
		"USER 65532:65532", "RUN --network=none "+canonicalToolMounts+analyze,
		"FROM scratch AS preparation",
		"COPY --from=analyzed /workspace/output/prepared/ /",
		"",
	)
	return []byte(strings.Join(lines, "\n")), nil
}

const resolvedConfigPath = "/workspace/config.json"

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
