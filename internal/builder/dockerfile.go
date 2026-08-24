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

// InstalledDockerfile is the only graph that runs user dependency lifecycle
// code. It exports only the resulting project tree as a producer-private OCI
// image so every later graph consumes the exact same installed bytes.
func InstalledDockerfile(builderImage string, install InstallPlan) ([]byte, error) {
	lines, err := installedDockerfileLines(builderImage, install)
	if err != nil {
		return nil, err
	}
	lines = append(lines,
		"FROM scratch AS installed-tree",
		"COPY --from=installed --chown=65532:65532 /workspace/project/ /workspace/project/",
		"",
	)
	return []byte(strings.Join(lines, "\n")), nil
}

// Dockerfile renders two networkless stages. The first is the last stage that
// executes tenant modules and exports only a closed prepared result. The
// second starts from a fresh builder image and performs static Program and
// bundle assembly from that result plus the exact installed-tree context.
func Dockerfile(builderImage string) ([]byte, error) {
	lines, err := materializedDockerfileLines(builderImage)
	if err != nil {
		return nil, err
	}
	prepare, err := dockerRunJSON([]string{
		"/usr/local/bin/bundle-builder",
		"--project", "/workspace/project",
		"--work", "/workspace/work",
		"--prepare-output", "/workspace/output/prepared",
		"--runtime-descriptor", "/opt/helmr/release/runtime.descriptor.json",
		"--runtime-metadata", "/opt/helmr/runtime/helmr/runtime.json",
		"--compiler-descriptor", "/nix/helmr/compiler.descriptor.json",
		"--node", "/opt/helmr/runtime/bin/node",
		"--node-loader", "/opt/helmr/runtime/lib/ld-linux-x86-64.so.2",
		"--node-library-path", "/opt/helmr/runtime/lib",
		"--config-evaluator", "/nix/helmr/config-evaluator.mjs",
		"--program-compiler", "/nix/helmr/program-compiler.mjs",
		"--encoder", "/usr/local/bin/mksquashfs",
	})
	if err != nil {
		return nil, err
	}
	finalizer, err := dockerRunJSON([]string{
		"/usr/local/bin/bundle-builder",
		"--prepared", "/workspace/prepared",
		"--program-project", "/workspace/program",
		"--work", "/workspace/work",
		"--bundle-output", "/workspace/output/bundle",
		"--workspace-images", "/workspace/images/images.json",
		"--expected-plan", "/workspace/images/build-plan.json",
		"--runtime-descriptor", "/opt/helmr/release/runtime.descriptor.json",
		"--runtime-metadata", "/opt/helmr/runtime/helmr/runtime.json",
		"--compiler-descriptor", "/nix/helmr/compiler.descriptor.json",
		"--encoder", "/usr/local/bin/mksquashfs",
	})
	if err != nil {
		return nil, err
	}
	lines = append(lines,
		"FROM materialized AS prepared",
		"RUN --network=none "+prepare,
		"FROM "+builderImage+" AS finalized",
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

func AnalysisDockerfile(builderImage string) ([]byte, error) {
	lines, err := materializedDockerfileLines(builderImage)
	if err != nil {
		return nil, err
	}
	command, err := dockerRunJSON([]string{
		"/usr/local/bin/bundle-builder", "--project", "/workspace/project",
		"--work", "/workspace/work", "--analysis-output", "/workspace/output/build-plan.json",
		"--runtime-descriptor", "/opt/helmr/release/runtime.descriptor.json",
		"--runtime-metadata", "/opt/helmr/runtime/helmr/runtime.json",
		"--compiler-descriptor", "/nix/helmr/compiler.descriptor.json",
		"--node", "/opt/helmr/runtime/bin/node",
		"--node-loader", "/opt/helmr/runtime/lib/ld-linux-x86-64.so.2",
		"--node-library-path", "/opt/helmr/runtime/lib",
		"--config-evaluator", "/nix/helmr/config-evaluator.mjs",
		"--program-compiler", "/nix/helmr/program-compiler.mjs",
		"--encoder", "/usr/local/bin/mksquashfs",
	})
	if err != nil {
		return nil, err
	}
	lines = append(lines,
		"FROM materialized AS analyzed",
		"RUN --network=none "+command,
		"FROM scratch AS analysis",
		"COPY --from=analyzed /workspace/output/build-plan.json /build-plan.json",
		"",
	)
	return []byte(strings.Join(lines, "\n")), nil
}

func materializedDockerfileLines(builderImage string) ([]string, error) {
	if err := ValidateBuilderImage(builderImage); err != nil {
		return nil, err
	}
	return []string{
		"# syntax=" + dockerfileFrontend,
		"FROM helmr_installed AS installed-tree",
		"FROM " + builderImage + " AS materialized",
		"USER 0:0",
		"RUN [\"/bin/bash\",\"-euo\",\"pipefail\",\"-c\",\"install -d -o 65532 -g 65532 /workspace/home /workspace/output /workspace/project /workspace/tmp /workspace/work\"]",
		"WORKDIR /workspace/project",
		"COPY --from=installed-tree --chown=0:0 /workspace/project/ /workspace/project/",
		"RUN [\"/bin/bash\",\"-euo\",\"pipefail\",\"-c\",\"chown -R 0:0 /workspace/project && chmod -R a-w /workspace/project && ln -s /workspace/project /opt/helmr/program\"]",
		"USER 65532:65532",
	}, nil
}

func installedDockerfileLines(builderImage string, install InstallPlan) ([]string, error) {
	if err := ValidateBuilderImage(builderImage); err != nil {
		return nil, err
	}
	installInstruction, err := installRunInstruction(install)
	if err != nil {
		return nil, err
	}
	return []string{
		"# syntax=" + dockerfileFrontend,
		"FROM " + builderImage + " AS installed",
		"USER 0:0",
		"RUN [\"/bin/bash\",\"-euo\",\"pipefail\",\"-c\",\"install -d -o 65532 -g 65532 /workspace/home /workspace/output /workspace/project /workspace/tmp /workspace/work\"]",
		"WORKDIR /workspace/project",
		"COPY --chown=65532:65532 . .",
		"USER 65532:65532",
		installInstruction,
	}, nil
}

func installRunInstruction(plan InstallPlan) (string, error) {
	secretIDs, err := NormalizeSecretIDs(plan.SecretIDs)
	if err != nil {
		return "", err
	}
	mounts := ""
	for _, id := range secretIDs {
		mounts += "--mount=type=secret,id=" + id + ",uid=65532,gid=65532,mode=0400,required=true "
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
		return "RUN " + mounts + command, nil
	}
	if len(plan.Argv) == 0 {
		return "", errors.New("install plan is empty")
	}
	command, err := dockerRunJSON(plan.Argv)
	if err != nil {
		return "", err
	}
	return "RUN " + mounts + command, nil
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
