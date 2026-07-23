package buildkit

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/containerd/platforms"
	"github.com/helmrdotdev/helmr/internal/safepath"
	"github.com/moby/buildkit/client/llb"
	ocispecs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/tonistiigi/fsutil"
)

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

type localContext struct {
	State    llb.State
	Selector string
}

func valueOrDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func resolveWorkdir(current, next string) string {
	next = strings.TrimSpace(next)
	if next == "" {
		return current
	}
	if strings.HasPrefix(next, "/") {
		return path.Clean(next)
	}
	base := strings.TrimSpace(current)
	if base == "" {
		base = "/"
	}
	return path.Clean(path.Join(base, next))
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

func resolveApplicationSourcePath(root, raw string) (string, string, error) {
	if strings.TrimSpace(root) == "" {
		return "", "", errors.New("source root is required")
	}
	relative, err := normalizeRelative(raw)
	if err != nil {
		return "", "", err
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
	if !strings.HasPrefix(targetCanonical, rootCanonical+string(filepath.Separator)) &&
		targetCanonical != rootCanonical {
		return "", "", fmt.Errorf("source ref path escapes root: %s", raw)
	}
	return relative, targetCanonical, nil
}

func rejectSymlinkComponents(root, relative string) error {
	current := root
	for component := range strings.SplitSeq(filepath.ToSlash(relative), "/") {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("stat source ref %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("source ref path is a symlink: %s", current)
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

func platformSpec(value string) (ocispecs.Platform, error) {
	platform, err := platforms.Parse(value)
	if err != nil {
		return ocispecs.Platform{}, fmt.Errorf("parse build platform %q: %w", value, err)
	}
	return platforms.Normalize(platform), nil
}
