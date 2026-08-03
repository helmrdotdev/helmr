package buildkit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/helmrdotdev/helmr/internal/imagebuild"
	"github.com/moby/buildkit/client/llb"
	"github.com/tonistiigi/fsutil"
)

func planDeclaredImage(
	build imagebuild.Build,
	sourceRoot string,
) (llbPlan, error) {
	if len(build.Images) == 0 {
		return llbPlan{}, errors.New("image build images must be non-empty")
	}
	architecture := build.Images[0].Platform.Architecture
	if err := imagebuild.Validate(build, architecture); err != nil {
		return llbPlan{}, err
	}
	images := make(map[string]imagebuild.Spec, len(build.Images))
	for _, image := range build.Images {
		images[image.Key] = image
	}
	planner := declaredImagePlanner{
		sourceRoot:  sourceRoot,
		images:      images,
		localMounts: map[string]fsutil.FS{},
	}
	state, config, err := planner.plan(build.Root, nil)
	if err != nil {
		return llbPlan{}, err
	}
	root := images[build.Root]
	return llbPlan{
		State:       state,
		LocalMounts: planner.localMounts,
		Platform:    root.Platform.OS + "/" + root.Platform.Architecture,
		Config: imageConfig{
			Architecture: root.Platform.Architecture,
			OS:           root.Platform.OS,
			Config: rootConfig{
				Env:        config.env,
				WorkingDir: valueOrDefault(config.workdir, defaultRuntimeWorkdir),
				User:       config.user,
			},
		},
	}, nil
}

type declaredImagePlanner struct {
	sourceRoot  string
	images      map[string]imagebuild.Spec
	localMounts map[string]fsutil.FS
	contextID   int
}

func (planner *declaredImagePlanner) plan(
	key string,
	stack []string,
) (llb.State, imageAccumulator, error) {
	image, ok := planner.images[key]
	if !ok {
		return llb.State{}, imageAccumulator{}, fmt.Errorf(
			"image %q is not defined",
			key,
		)
	}
	if slices.Contains(stack, key) {
		return llb.State{}, imageAccumulator{}, fmt.Errorf(
			"copyFromImage graph contains a cycle at %q",
			key,
		)
	}
	var state llb.State
	var config imageAccumulator
	hasState := false
	for index, step := range image.Steps {
		switch {
		case step.From != nil:
			state = llb.Image(canonicalDockerRef(step.From.Ref))
			hasState = true
		case step.CopySourceFile != nil:
			if !hasState {
				return llb.State{}, imageAccumulator{}, fmt.Errorf(
					"image %q copySourceFile step %d has no base image",
					key,
					index,
				)
			}
			source, err := planner.sourceFile(index, *step.CopySourceFile)
			if err != nil {
				return llb.State{}, imageAccumulator{}, err
			}
			state = state.File(llb.Copy(
				source.State,
				source.Selector,
				step.CopySourceFile.Dst,
				&llb.CopyInfo{CreateDestPath: true},
			))
		case step.CopySourceDir != nil:
			if !hasState {
				return llb.State{}, imageAccumulator{}, fmt.Errorf(
					"image %q copySourceDir step %d has no base image",
					key,
					index,
				)
			}
			source, err := planner.sourceDirectory(index, *step.CopySourceDir)
			if err != nil {
				return llb.State{}, imageAccumulator{}, err
			}
			state = state.File(llb.Copy(
				source.State,
				source.Selector,
				step.CopySourceDir.Dst,
				&llb.CopyInfo{CreateDestPath: true},
			))
		case step.CopyFromImage != nil:
			if !hasState {
				return llb.State{}, imageAccumulator{}, fmt.Errorf(
					"image %q copyFromImage step %d has no base image",
					key,
					index,
				)
			}
			source, _, err := planner.plan(
				step.CopyFromImage.ImageKey,
				append(stack, key),
			)
			if err != nil {
				return llb.State{}, imageAccumulator{}, err
			}
			state = state.File(llb.Copy(
				source,
				step.CopyFromImage.SrcPath,
				step.CopyFromImage.Dst,
				&llb.CopyInfo{CreateDestPath: true},
			))
		case step.Env != nil:
			config.env = append(config.env, step.Env.Key+"="+step.Env.Value)
			if hasState {
				state = state.AddEnv(step.Env.Key, step.Env.Value)
			}
		case step.Workdir != nil:
			config.workdir = resolveWorkdir(config.workdir, step.Workdir.Path)
			if hasState {
				state = state.Dir(config.workdir)
			}
		case step.User != nil:
			config.user = step.User.Name
			if hasState {
				state = state.With(llb.User(step.User.Name))
			}
		case step.Run != nil:
			if !hasState {
				return llb.State{}, imageAccumulator{}, fmt.Errorf(
					"image %q run step %d has no base image",
					key,
					index,
				)
			}
			state = state.Run(llb.Args(step.Run.Argv)).Root()
		default:
			return llb.State{}, imageAccumulator{}, fmt.Errorf(
				"image %q step %d has no operation",
				key,
				index,
			)
		}
	}
	if !hasState {
		return llb.State{}, imageAccumulator{}, fmt.Errorf(
			"image %q has no base image",
			key,
		)
	}
	return state, config, nil
}

func (planner *declaredImagePlanner) sourceFile(
	index int,
	source imagebuild.CopySourceFile,
) (localContext, error) {
	relative, path, err := resolveApplicationSourcePath(
		planner.sourceRoot,
		source.Path,
	)
	if err != nil {
		return localContext{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return localContext{}, fmt.Errorf("stat source file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return localContext{}, fmt.Errorf("source path is not a regular file: %s", path)
	}
	planner.contextID++
	name := fmt.Sprintf(
		"workspace_source_file_%d_%d",
		index,
		planner.contextID,
	)
	filesystem, err := fsutil.NewFS(planner.sourceRoot)
	if err != nil {
		return localContext{}, fmt.Errorf("create source file context: %w", err)
	}
	planner.localMounts[name] = filesystem
	return localContext{
		State: llb.Local(
			name,
			llb.IncludePatterns([]string{filepath.ToSlash(relative)}),
			llb.FollowPaths([]string{"."}),
			llb.SharedKeyHint(name),
		),
		Selector: "/" + filepath.ToSlash(relative),
	}, nil
}

func (planner *declaredImagePlanner) sourceDirectory(
	index int,
	source imagebuild.CopySourceDir,
) (localContext, error) {
	_, path, err := resolveApplicationSourcePath(
		planner.sourceRoot,
		source.Path,
	)
	if err != nil {
		return localContext{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return localContext{}, fmt.Errorf("stat source directory %s: %w", path, err)
	}
	if !info.IsDir() {
		return localContext{}, fmt.Errorf("source path is not a directory: %s", path)
	}
	planner.contextID++
	name := fmt.Sprintf(
		"workspace_source_directory_%d_%d",
		index,
		planner.contextID,
	)
	filesystem, err := fsutil.NewFS(path)
	if err != nil {
		return localContext{}, fmt.Errorf("create source directory context: %w", err)
	}
	planner.localMounts[name] = filesystem
	return localContext{
		State: llb.Local(
			name,
			llb.FollowPaths([]string{"."}),
			llb.SharedKeyHint(name),
		),
		Selector: "/",
	}, nil
}

var _ imagebuild.Engine = (*Builder)(nil)
