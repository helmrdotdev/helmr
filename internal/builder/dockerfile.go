package builder

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var builderImagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*@sha256:[0-9a-f]{64}$`)

// Dockerfile renders the only supported BuildKit graph. User lifecycle code
// runs in the installed stage; the mandatory Helmr finalizer runs without a
// network and emits only the verified bundle directory into the scratch stage.
func Dockerfile(builderImage string, install InstallPlan) ([]byte, error) {
	if !builderImagePattern.MatchString(builderImage) {
		return nil, errors.New("builder image must be an exact lowercase sha256 reference")
	}
	installInstruction, err := installRunInstruction(install)
	if err != nil {
		return nil, err
	}
	finalizer, err := dockerRunJSON([]string{
		"/usr/local/bin/bundle-builder",
		"--project", "/workspace/project",
		"--work", "/workspace/work",
		"--bundle-output", "/workspace/output/bundle",
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
	document := strings.Join([]string{
		"# syntax=docker/dockerfile:1.7",
		"FROM " + builderImage + " AS installed",
		"USER 0:0",
		"RUN [\"/bin/bash\",\"-euo\",\"pipefail\",\"-c\",\"install -d -o 65532 -g 65532 /workspace/home /workspace/output /workspace/project /workspace/tmp /workspace/work\"]",
		"WORKDIR /workspace/project",
		"COPY --chown=65532:65532 . .",
		"USER 65532:65532",
		installInstruction,
		"FROM installed AS finalized",
		"RUN --network=none " + finalizer,
		"FROM scratch AS bundle",
		"COPY --from=finalized /workspace/output/bundle/ /",
		"",
	}, "\n")
	return []byte(document), nil
}

func installRunInstruction(plan InstallPlan) (string, error) {
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
		return "RUN " + command, nil
	}
	if len(plan.Argv) == 0 {
		return "", errors.New("install plan is empty")
	}
	command, err := dockerRunJSON(plan.Argv)
	if err != nil {
		return "", err
	}
	return "RUN " + command, nil
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
