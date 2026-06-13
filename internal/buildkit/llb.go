package buildkit

import (
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/containerd/platforms"
	bundlev0 "github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
	"github.com/helmrdotdev/helmr/internal/safepath"
	"github.com/moby/buildkit/client/llb"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/tonistiigi/fsutil"
)

var buildInputHardExcludes = []string{".git/"}

const defaultRuntimeWorkdir = "/workspace"

type llbPlan struct {
	State       llb.State
	LocalMounts map[string]fsutil.FS
	Config      imageConfig
	Platform    string
}

type imageConfig struct {
	Architecture string     `json:"architecture"`
	OS           string     `json:"os"`
	Config       rootConfig `json:"config"`
}

type rootConfig struct {
	Env        []string `json:"Env,omitempty"`
	WorkingDir string   `json:"WorkingDir,omitempty"`
	User       string   `json:"User,omitempty"`
}

type imageAccumulator struct {
	env     []string
	workdir string
	user    string
}

func planImage(image *bundlev0.ImageSpec, subImages map[string]*bundlev0.ImageSpec, sourceRoot string, defaultPlatform string, cacheNamespace string) (llbPlan, error) {
	if image == nil {
		return llbPlan{}, errors.New("image spec is required")
	}
	cacheNamespace = safeNamespace(cacheNamespace)
	if cacheNamespace == "" || cacheNamespace == "_" {
		cacheNamespace = defaultCacheNS
	}
	planner := imagePlanner{
		sourceRoot:  sourceRoot,
		subImages:   subImages,
		localMounts: map[string]fsutil.FS{},
		cacheNS:     cacheNamespace,
	}
	state, config, err := planner.plan(image, nil)
	if err != nil {
		return llbPlan{}, err
	}
	osName, arch := imagePlatform(image, defaultPlatform)
	platform := osName + "/" + arch
	return llbPlan{
		State:       state,
		LocalMounts: planner.localMounts,
		Platform:    platform,
		Config: imageConfig{
			Architecture: arch,
			OS:           osName,
			Config: rootConfig{
				Env:        config.env,
				WorkingDir: valueOrDefault(config.workdir, defaultRuntimeWorkdir),
				User:       config.user,
			},
		},
	}, nil
}

type imagePlanner struct {
	sourceRoot  string
	subImages   map[string]*bundlev0.ImageSpec
	localMounts map[string]fsutil.FS
	cacheNS     string
}

func (p *imagePlanner) plan(image *bundlev0.ImageSpec, stack []string) (llb.State, imageAccumulator, error) {
	if len(image.Steps) == 0 {
		return llb.State{}, imageAccumulator{}, errors.New("image chain has no operations")
	}
	var state llb.State
	var hasState bool
	var acc imageAccumulator
	for index, step := range image.Steps {
		if step == nil {
			return llb.State{}, imageAccumulator{}, fmt.Errorf("step %d has no kind", index)
		}
		switch value := step.GetKind().(type) {
		case *bundlev0.ImageStep_From:
			if value.From == nil || strings.TrimSpace(value.From.Ref) == "" {
				return llb.State{}, imageAccumulator{}, fmt.Errorf("from step %d ref is required", index)
			}
			state = llb.Image(canonicalDockerRef(value.From.Ref))
			hasState = true
		case *bundlev0.ImageStep_CopySourceFile:
			if !hasState {
				return llb.State{}, imageAccumulator{}, fmt.Errorf("copy_source_file step %d has no base image", index)
			}
			context, err := p.sourceFileContext(index, value.CopySourceFile)
			if err != nil {
				return llb.State{}, imageAccumulator{}, err
			}
			state = state.File(llb.Copy(context.State, context.Selector, value.CopySourceFile.Dst, &llb.CopyInfo{CreateDestPath: true}))
		case *bundlev0.ImageStep_CopySourceDir:
			if !hasState {
				return llb.State{}, imageAccumulator{}, fmt.Errorf("copy_source_dir step %d has no base image", index)
			}
			context, err := p.sourceDirContext(index, value.CopySourceDir)
			if err != nil {
				return llb.State{}, imageAccumulator{}, err
			}
			state = state.File(llb.Copy(context.State, context.Selector, value.CopySourceDir.Dst, &llb.CopyInfo{CreateDestPath: true}))
		case *bundlev0.ImageStep_CopyFromImage:
			if !hasState {
				return llb.State{}, imageAccumulator{}, fmt.Errorf("copy_from_image step %d has no base image", index)
			}
			subImage, err := p.subImage(value.CopyFromImage, stack)
			if err != nil {
				return llb.State{}, imageAccumulator{}, err
			}
			subState, _, err := p.plan(subImage, append(stack, value.CopyFromImage.SrcImageKey))
			if err != nil {
				return llb.State{}, imageAccumulator{}, err
			}
			state = state.File(llb.Copy(subState, value.CopyFromImage.SrcPath, value.CopyFromImage.Dst, &llb.CopyInfo{CreateDestPath: true}))
		case *bundlev0.ImageStep_Env:
			if value.Env != nil {
				acc.env = append(acc.env, value.Env.Key+"="+value.Env.Value)
				if hasState {
					state = state.AddEnv(value.Env.Key, value.Env.Value)
				}
			}
		case *bundlev0.ImageStep_Workdir:
			if value.Workdir != nil {
				acc.workdir = resolveWorkdir(acc.workdir, value.Workdir.Path)
				if hasState {
					state = state.Dir(acc.workdir)
				}
			}
		case *bundlev0.ImageStep_User:
			if value.User != nil {
				acc.user = value.User.Name
				if hasState {
					state = state.With(llb.User(value.User.Name))
				}
			}
		case *bundlev0.ImageStep_Run:
			if !hasState {
				return llb.State{}, imageAccumulator{}, fmt.Errorf("run step %d has no base image", index)
			}
			options, err := p.runOptions(value.Run)
			if err != nil {
				return llb.State{}, imageAccumulator{}, err
			}
			state = state.Run(options...).Root()
		case nil:
			return llb.State{}, imageAccumulator{}, fmt.Errorf("step %d has no kind", index)
		default:
			return llb.State{}, imageAccumulator{}, fmt.Errorf("step %d has unsupported kind %T", index, value)
		}
	}
	if !hasState {
		return llb.State{}, imageAccumulator{}, errors.New("image chain has no operations")
	}
	return state, acc, nil
}

func (p *imagePlanner) runOptions(run *bundlev0.Run) ([]llb.RunOption, error) {
	if run == nil || len(run.Argv) == 0 {
		return nil, errors.New("run argv is required")
	}
	if len(run.CacheMounts) > 0 && len(run.SecretMounts) > 0 {
		return nil, errors.New("run step cannot combine secret mounts with persistent cache mounts")
	}
	options := []llb.RunOption{llb.Args(run.Argv)}
	for _, mount := range run.CacheMounts {
		if strings.TrimSpace(mount.Dst) == "" {
			return nil, errors.New("cache mount dst is required")
		}
		if strings.TrimSpace(mount.CacheId) == "" {
			return nil, errors.New("cache mount cache_id is required")
		}
		sharing, err := cacheSharing(mount.Sharing)
		if err != nil {
			return nil, err
		}
		options = append(options, llb.AddMount(mount.Dst, llb.Scratch(), llb.AsPersistentCacheDir(p.cacheID(mount.CacheId), sharing)))
	}
	for _, mount := range run.SecretMounts {
		if mount.SecretRef == nil || strings.TrimSpace(mount.SecretRef.Name) == "" {
			return nil, errors.New("secret mount is missing secret_ref")
		}
		if strings.TrimSpace(mount.Dst) == "" {
			return nil, errors.New("secret mount dst is required")
		}
		dst := mount.Dst
		options = append(options, llb.AddSecretWithDest(mount.SecretRef.Name, &dst, llb.SecretFileOpt(0, 0, 0o400)))
	}
	return options, nil
}

func (p *imagePlanner) cacheID(id string) string {
	return p.cacheNS + "/" + safeSegment(id)
}

func valueOrDefault(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func resolveWorkdir(current string, next string) string {
	next = strings.TrimSpace(next)
	if next == "" {
		return current
	}
	if strings.HasPrefix(next, "/") {
		return pathpkg.Clean(next)
	}
	base := strings.TrimSpace(current)
	if base == "" {
		base = "/"
	}
	return pathpkg.Clean(pathpkg.Join(base, next))
}

func cacheSharing(value string) (llb.CacheMountSharingMode, error) {
	switch strings.TrimSpace(value) {
	case "", "shared":
		return llb.CacheMountShared, nil
	case "private":
		return llb.CacheMountPrivate, nil
	case "locked":
		return llb.CacheMountLocked, nil
	default:
		return 0, fmt.Errorf("unsupported cache mount sharing %q", value)
	}
}

type localContext struct {
	State    llb.State
	Selector string
}

func (p *imagePlanner) sourceFileContext(index int, value *bundlev0.CopySourceFile) (localContext, error) {
	if value == nil || value.SrcRef == nil {
		return localContext{}, errors.New("source file ref is missing")
	}
	relative, path, err := resolveSourcePath(p.sourceRoot, value.SrcRef.Path)
	if err != nil {
		return localContext{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return localContext{}, fmt.Errorf("stat source file %s: %w", path, err)
	}
	if info.IsDir() {
		return localContext{}, fmt.Errorf("source ref path is not a file: %s", path)
	}
	name := fmt.Sprintf("source_file_%d", index)
	fs, err := fsutil.NewFS(p.sourceRoot)
	if err != nil {
		return localContext{}, fmt.Errorf("create source file context: %w", err)
	}
	p.localMounts[name] = fs
	selector := "/" + filepath.ToSlash(relative)
	return localContext{
		State: llb.Local(name,
			llb.IncludePatterns([]string{filepath.ToSlash(relative)}),
			llb.ExcludePatterns(buildInputHardExcludes),
			llb.FollowPaths([]string{"."}),
			llb.SharedKeyHint(name),
		),
		Selector: selector,
	}, nil
}

func (p *imagePlanner) sourceDirContext(index int, value *bundlev0.CopySourceDir) (localContext, error) {
	if value == nil || value.SrcRef == nil {
		return localContext{}, errors.New("source dir ref is missing")
	}
	_, path, err := resolveSourcePath(p.sourceRoot, value.SrcRef.Path)
	if err != nil {
		return localContext{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return localContext{}, fmt.Errorf("stat source dir %s: %w", path, err)
	}
	if !info.IsDir() {
		return localContext{}, fmt.Errorf("source ref path is not a directory: %s", path)
	}
	name := fmt.Sprintf("source_dir_%d", index)
	fs, err := fsutil.NewFS(path)
	if err != nil {
		return localContext{}, fmt.Errorf("create source dir context: %w", err)
	}
	p.localMounts[name] = fs
	excludes := append([]string{}, value.SrcRef.Ignore...)
	excludes = append(excludes, value.Ignore...)
	excludes = append(excludes, buildInputHardExcludes...)
	return localContext{
		State:    llb.Local(name, llb.ExcludePatterns(excludes), llb.FollowPaths([]string{"."}), llb.SharedKeyHint(name)),
		Selector: "/",
	}, nil
}

func (p *imagePlanner) subImage(copy *bundlev0.CopyFromImage, stack []string) (*bundlev0.ImageSpec, error) {
	if copy == nil || strings.TrimSpace(copy.SrcImageKey) == "" {
		return nil, errors.New("copy_from_image src_image_key is required")
	}
	for _, key := range stack {
		if key == copy.SrcImageKey {
			return nil, fmt.Errorf("copy_from_image sub-image graph contains a cycle at %s", copy.SrcImageKey)
		}
	}
	image := p.subImages[copy.SrcImageKey]
	if image == nil {
		return nil, fmt.Errorf("copy_from_image sub-image ImageSpec is missing for %s", copy.SrcImageKey)
	}
	return image, nil
}

func resolveSourcePath(root, raw string) (string, string, error) {
	if strings.TrimSpace(root) == "" {
		return "", "", errors.New("source root is required")
	}
	relative, err := normalizeRelative(raw)
	if err != nil {
		return "", "", err
	}
	if isBuildInputHardExcluded(relative) {
		return "", "", fmt.Errorf("source ref points at a hard-excluded path: %s", relative)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	rootCanonical, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", "", fmt.Errorf("canonicalize source root %s: %w", root, err)
	}
	if err := rejectSymlinkComponents(rootCanonical, relative); err != nil {
		return "", "", err
	}
	target := filepath.Join(rootCanonical, relative)
	targetCanonical, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", "", fmt.Errorf("canonicalize source ref %s: %w", target, err)
	}
	if !strings.HasPrefix(targetCanonical, rootCanonical+string(filepath.Separator)) && targetCanonical != rootCanonical {
		return "", "", fmt.Errorf("source ref path escapes root: %s", raw)
	}
	return relative, targetCanonical, nil
}

func rejectSymlinkComponents(root, relative string) error {
	path := root
	for _, component := range strings.Split(filepath.ToSlash(relative), "/") {
		if component == "" || component == "." {
			continue
		}
		path = filepath.Join(path, component)
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("stat source ref %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("source ref path is a symlink: %s", path)
		}
	}
	return nil
}

func normalizeRelative(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", errors.New("source ref path is empty")
	}
	clean, err := safepath.CleanLocal(raw, safepath.CleanOptions{AllowDot: true})
	if err != nil {
		return "", fmt.Errorf("source ref path escapes root: %s", raw)
	}
	return clean, nil
}

func isBuildInputHardExcluded(relative string) bool {
	for _, component := range strings.Split(filepath.ToSlash(relative), "/") {
		if component == ".git" {
			return true
		}
	}
	return false
}

func canonicalDockerRef(ref string) string {
	first, _, ok := strings.Cut(ref, "/")
	if !ok {
		return "docker.io/library/" + ref
	}
	if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
		return ref
	}
	return "docker.io/" + ref
}

func imagePlatform(image *bundlev0.ImageSpec, fallback string) (string, string) {
	if image.Platform == nil {
		platform, err := platforms.Parse(fallback)
		if err != nil {
			platform = platforms.MustParse(defaultPlatform)
		}
		platform = platforms.Normalize(platform)
		return platform.OS, platform.Architecture
	}
	osName := image.Platform.Os
	if osName == "" {
		osName = "linux"
	}
	arch := image.Platform.Architecture
	if arch == "" {
		arch = "amd64"
	}
	return osName, arch
}

func platformSpec(value string) (ocispecs.Platform, error) {
	platform, err := platforms.Parse(value)
	if err != nil {
		return ocispecs.Platform{}, fmt.Errorf("parse build platform %q: %w", value, err)
	}
	return platforms.Normalize(platform), nil
}
