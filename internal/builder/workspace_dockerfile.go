package builder

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/distribution/reference"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/imagebuild"
)

type WorkspaceBuild struct {
	DeclaredID string
	Build      imagebuild.Build
}

func WorkspaceBuilds(plan deployment.BuildPlan) ([]WorkspaceBuild, error) {
	if err := deployment.ValidateBuildPlan(plan); err != nil {
		return nil, err
	}
	builds := make([]WorkspaceBuild, 0)
	for _, definition := range plan.Definitions {
		if definition.Sandbox == nil {
			continue
		}
		builds = append(builds, WorkspaceBuild{
			DeclaredID: definition.DeclaredID,
			Build:      definition.Sandbox.ImageBuild,
		})
	}
	sort.Slice(builds, func(left, right int) bool {
		return builds[left].DeclaredID < builds[right].DeclaredID
	})
	return builds, nil
}

// WorkspaceImageDockerfile renders a declared image graph into ordinary
// BuildKit stages. The digest-bound helmr_installed context is the only project
// source authority. Registry authentication, when needed, belongs to the
// caller's local Docker/BuildKit session rather than Helmr or Control Plane.
func WorkspaceImageDockerfile(
	build imagebuild.Build,
) ([]byte, string, error) {
	lines := []string{"# syntax=" + dockerfileFrontend, "FROM helmr_installed AS installed-tree"}
	if err := imagebuild.Validate(build, "x86_64"); err != nil {
		return nil, "", err
	}
	byKey := make(map[string]imagebuild.Spec, len(build.Images))
	for _, image := range build.Images {
		byKey[image.Key] = image
	}
	aliases := make(map[string]string, len(build.Images))
	visiting := make(map[string]bool, len(build.Images))
	var render func(string) error
	render = func(key string) error {
		if aliases[key] != "" {
			return nil
		}
		if visiting[key] {
			return fmt.Errorf("workspace image graph contains a cycle at %q", key)
		}
		visiting[key] = true
		image := byKey[key]
		for _, step := range image.Steps {
			if step.CopyFromImage != nil {
				if err := render(step.CopyFromImage.ImageKey); err != nil {
					return err
				}
			}
		}
		delete(visiting, key)
		alias := fmt.Sprintf("helmr_workspace_%d", len(aliases))
		aliases[key] = alias
		for index, step := range image.Steps {
			switch {
			case step.From != nil:
				if index != 0 {
					return errors.New("workspace image from step is not first")
				}
				if err := validateWorkspaceBase(*step.From); err != nil {
					return err
				}
				lines = append(lines, "FROM --platform=linux/amd64 "+step.From.Ref+" AS "+alias)
			case step.Run != nil:
				command, err := dockerRunJSON(step.Run.Argv)
				if err != nil {
					return err
				}
				lines = append(lines, "RUN "+command)
			case step.CopySourceFile != nil:
				instruction, err := dockerCopyJSON("installed-tree", installedSource(step.CopySourceFile.Path), step.CopySourceFile.Dst)
				if err != nil {
					return err
				}
				lines = append(lines, instruction)
			case step.CopySourceDir != nil:
				instruction, err := dockerCopyJSON("installed-tree", installedSource(step.CopySourceDir.Path)+"/", step.CopySourceDir.Dst)
				if err != nil {
					return err
				}
				lines = append(lines, instruction)
			case step.CopyFromImage != nil:
				instruction, err := dockerCopyJSON(aliases[step.CopyFromImage.ImageKey], step.CopyFromImage.SrcPath, step.CopyFromImage.Dst)
				if err != nil {
					return err
				}
				lines = append(lines, instruction)
			case step.Workdir != nil:
				lines = append(lines, "WORKDIR "+strconv.Quote(step.Workdir.Path))
			case step.User != nil:
				lines = append(lines, "USER "+step.User.Name)
			case step.Env != nil:
				lines = append(lines, "ENV "+step.Env.Key+"="+strconv.Quote(step.Env.Value))
			default:
				return errors.New("workspace image step is empty")
			}
		}
		return nil
	}
	if err := render(build.Root); err != nil {
		return nil, "", err
	}
	lines = append(lines, "")
	return []byte(strings.Join(lines, "\n")), aliases[build.Root], nil
}

func validateWorkspaceBase(from imagebuild.From) error {
	named, err := reference.ParseNormalizedNamed(from.Ref)
	if err != nil || named.String() != from.Ref {
		return errors.New("workspace image base must be a canonical fully qualified reference")
	}
	if _, ok := named.(reference.Canonical); !ok {
		return errors.New("workspace image base must be pinned by digest")
	}
	return nil
}

func installedSource(relative string) string {
	if relative == "." {
		return "/workspace/project"
	}
	return path.Join("/workspace/project", relative)
}

func dockerCopyJSON(from, source, destination string) (string, error) {
	copy, err := dockerRunJSON([]string{source, destination})
	if err != nil {
		return "", err
	}
	return "COPY --from=" + from + " " + copy, nil
}
