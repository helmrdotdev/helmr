package builder

import (
	"errors"
	"strings"

	"github.com/helmrdotdev/helmr/internal/hostconfig"
)

// EnvironmentDockerfile renders the project's build.builder steps on top of
// the pinned builder. Preparation has one documented context: root, working
// directory /, and root's own home and cache. It is built once and exported;
// install and analysis start from that exact image instead of repeating steps
// whose results may differ between runs. Copy sources come from the captured
// project source, which is this graph's build context.
func EnvironmentDockerfile(steps []hostconfig.Step) ([]byte, error) {
	lines := []string{
		"# syntax=" + dockerfileFrontend,
		"FROM " + BuilderContextName + " AS environment",
		"USER 0:0",
		"RUN [\"/bin/bash\",\"-euo\",\"pipefail\",\"-c\",\"install -d /root/.cache\"]",
		"WORKDIR /",
		"ENV HOME=/root TMPDIR=/tmp XDG_CACHE_HOME=/root/.cache",
	}
	for _, step := range steps {
		switch step.Kind {
		case "run":
			command, err := dockerRunJSON(step.Argv)
			if err != nil {
				return nil, err
			}
			lines = append(lines, "RUN "+command)
		case "copy":
			arguments, err := dockerRunJSON([]string{literalCopySource(step.Source), literalCopyDestination(step.Destination)})
			if err != nil {
				return nil, err
			}
			lines = append(lines, "COPY "+arguments)
		default:
			return nil, errors.New("build environment step is not resolved")
		}
	}
	return []byte(strings.Join(append(lines, ""), "\n")), nil
}

// literalCopySource and literalCopyDestination protect a validated literal
// path from the two ways COPY reinterprets its arguments. Sources are matched as patterns, so "[" is
// written as the class "[[]": a[1].txt selects that file and not a1.txt. Both
// sources and destinations undergo environment substitution, so "$" is written
// as "\$": $NAME.txt stays that name even when the image defines NAME. "*", "?"
// and "\" never reach here; validation refuses them. A directory source copies
// its contents, with or without a trailing slash.
func literalCopySource(source string) string {
	if source == "." {
		return "/"
	}
	return "/" + strings.ReplaceAll(literalCopyDestination(source), "[", "[[]")
}

func literalCopyDestination(destination string) string {
	return strings.ReplaceAll(destination, "$", `\$`)
}

// CapturedSourceIgnoreFile is written beside every generated Dockerfile as
// <Dockerfile>.dockerignore. BuildKit prefers it over a .dockerignore inside
// the context, so a project's own Docker ignore rules cannot reselect the
// source Helmr captured under .helmrignore.
const CapturedSourceIgnoreFile = "# Helmr captured this source; nothing is excluded again.\n"
