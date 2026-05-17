package checkout

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
)

var githubRepositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// TokenProvider returns a GitHub token for fetching private repositories.
type TokenProvider func(context.Context, api.GitHubSource) (string, error)

type options struct {
	gitPath       string
	tokenProvider TokenProvider
}

// Option configures GitHub source materialization.
type Option func(*options)

// WithGitPath sets the git executable path. It is primarily useful for tests.
func WithGitPath(path string) Option {
	return func(opts *options) {
		opts.gitPath = path
	}
}

// WithTokenProvider configures an optional provider for private GitHub fetches.
func WithTokenProvider(provider TokenProvider) Option {
	return func(opts *options) {
		opts.tokenProvider = provider
	}
}

// Worktree is a materialized source checkout.
type Worktree struct {
	CheckoutRoot string
	ProjectRoot  string
	SHA          string
}

// Clone checks out source.SHA from source.Repository into destination.
func Clone(ctx context.Context, source api.GitHubSource, destination string, opts ...Option) (Worktree, error) {
	cfg := options{gitPath: "git"}
	for _, opt := range opts {
		opt(&cfg)
	}

	normalized, err := validateGitHubSource(source)
	if err != nil {
		return Worktree{}, err
	}

	root, err := prepareDestination(destination)
	if err != nil {
		return Worktree{}, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(root)
		}
	}()

	env, cleanupEnv, err := gitEnv("")
	if err != nil {
		return Worktree{}, err
	}
	defer func() {
		cleanupEnv()
	}()
	if cfg.tokenProvider != nil {
		token, err := cfg.tokenProvider(ctx, normalized)
		if err != nil {
			return Worktree{}, fmt.Errorf("get github checkout token: %w", err)
		}
		if token != "" {
			cleanupEnv()
			env, cleanupEnv, err = gitEnv(token)
			if err != nil {
				return Worktree{}, err
			}
		}
	}

	repositoryURL := "https://github.com/" + normalized.Repository + ".git"
	if err := runGit(ctx, cfg.gitPath, env, "init", "--quiet", root); err != nil {
		return Worktree{}, err
	}
	if err := runGit(ctx, cfg.gitPath, env, "-C", root, "remote", "add", "origin", repositoryURL); err != nil {
		return Worktree{}, err
	}
	if err := runGit(ctx, cfg.gitPath, env, "-C", root, "fetch", "--depth=1", "origin", normalized.SHA); err != nil {
		return Worktree{}, err
	}
	if err := runGit(ctx, cfg.gitPath, env, "-C", root, "checkout", "--detach", "--quiet", normalized.SHA); err != nil {
		return Worktree{}, err
	}

	head, err := gitOutput(ctx, cfg.gitPath, env, "-C", root, "rev-parse", "HEAD")
	if err != nil {
		return Worktree{}, err
	}
	if strings.TrimSpace(head) != normalized.SHA {
		return Worktree{}, fmt.Errorf("checkout head %q does not match source sha %q", strings.TrimSpace(head), normalized.SHA)
	}

	projectRoot := root
	if normalized.Subpath != "" {
		projectRoot, err = resolveSubpath(root, normalized.Subpath)
		if err != nil {
			return Worktree{}, err
		}
	}

	complete = true
	return Worktree{CheckoutRoot: root, ProjectRoot: projectRoot, SHA: normalized.SHA}, nil
}

func validateGitHubSource(source api.GitHubSource) (api.GitHubSource, error) {
	repository := strings.TrimSpace(source.Repository)
	if !githubRepositoryPattern.MatchString(repository) {
		return api.GitHubSource{}, errors.New(`source.repository must be "owner/repo"`)
	}

	sha := strings.TrimSpace(source.SHA)
	if !isFullGitSHA(sha) {
		return api.GitHubSource{}, errors.New("source.sha must be a full 40-character commit SHA")
	}

	ref := strings.TrimSpace(source.Ref)
	if strings.ContainsRune(ref, '\x00') {
		return api.GitHubSource{}, errors.New("source.ref contains NUL")
	}

	subpath, err := normalizeSubpath(source.Subpath)
	if err != nil {
		return api.GitHubSource{}, err
	}

	return api.GitHubSource{
		Repository: repository,
		Ref:        ref,
		SHA:        sha,
		Subpath:    subpath,
	}, nil
}

func prepareDestination(destination string) (string, error) {
	if strings.TrimSpace(destination) == "" {
		return "", errors.New("destination is required")
	}

	root, err := filepath.Abs(destination)
	if err != nil {
		return "", fmt.Errorf("resolve destination: %w", err)
	}

	info, err := os.Lstat(root)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("destination exists and is a symlink")
		}
		if !info.IsDir() {
			return "", errors.New("destination exists and is not a directory")
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			return "", fmt.Errorf("read destination: %w", err)
		}
		if len(entries) != 0 {
			return "", errors.New("destination must be empty")
		}
		return root, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat destination: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create destination: %w", err)
	}
	return root, nil
}

func runGit(ctx context.Context, gitPath string, env []string, args ...string) error {
	if _, err := gitOutput(ctx, gitPath, env, args...); err != nil {
		return err
	}
	return nil
}

func gitOutput(ctx context.Context, gitPath string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, gitPath, args...)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, sanitizeGitOutput(output))
	}
	return string(output), nil
}

func gitAuthEnv(token string) []string {
	header := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=" + header,
	}
}

func gitEnv(token string) ([]string, func(), error) {
	home, err := os.MkdirTemp("", "helmr-git-home-")
	if err != nil {
		return nil, nil, fmt.Errorf("create git home: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(home) }
	env := make([]string, 0, len(os.Environ())+12)
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if isUnsafeGitEnv(name) {
			continue
		}
		env = append(env, entry)
	}
	env = append(env,
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=true",
		"SSH_ASKPASS=true",
	)
	if token != "" {
		env = append(env, gitAuthEnv(token)...)
	}
	return env, cleanup, nil
}

func isUnsafeGitEnv(name string) bool {
	if name == "HOME" || name == "XDG_CONFIG_HOME" || name == "GITHUB_TOKEN" {
		return true
	}
	if strings.HasPrefix(name, "GIT_") || strings.HasPrefix(name, "SSH_ASKPASS") {
		return true
	}
	return false
}

func sanitizeGitOutput(output []byte) string {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return ""
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(strings.ToLower(line), "authorization:") {
			return "[redacted git output]"
		}
	}
	return text
}

func resolveSubpath(root string, subpath string) (string, error) {
	current := root
	for _, part := range strings.Split(subpath, "/") {
		if part == "" {
			continue
		}
		current = filepath.Join(current, filepath.FromSlash(part))
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("source subpath %q: %w", subpath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("source subpath %q contains symlink", subpath)
		}
	}
	info, err := os.Lstat(current)
	if err != nil {
		return "", fmt.Errorf("source subpath %q: %w", subpath, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("source subpath %q is not a directory", subpath)
	}
	return current, nil
}

func normalizeSubpath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "." {
		return "", nil
	}
	if strings.ContainsRune(raw, '\x00') {
		return "", errors.New("source.subpath contains NUL")
	}
	if strings.HasPrefix(raw, "/") {
		return "", errors.New("source.subpath must be relative")
	}
	clean := path.Clean(raw)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("source.subpath escapes repository root")
	}
	return clean, nil
}

func isFullGitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
