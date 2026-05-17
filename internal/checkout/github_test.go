package checkout_test

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/checkout"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func TestCloneChecksOutPinnedSHA(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	gitPath := fakeGit(t)
	logPath := filepath.Join(tempDir, "git.log")
	destination := filepath.Join(tempDir, "checkout")

	t.Setenv("FAKE_GIT_LOG", logPath)
	t.Setenv("FAKE_EXPECT_SHA", testSHA)

	worktree, err := checkout.Clone(ctx, validSource("app"), destination, checkout.WithGitPath(gitPath))
	if err != nil {
		t.Fatalf("Clone() error = %v", err)
	}

	root, err := filepath.Abs(destination)
	if err != nil {
		t.Fatal(err)
	}
	if worktree.CheckoutRoot != root {
		t.Fatalf("CheckoutRoot = %q, want %q", worktree.CheckoutRoot, root)
	}
	if worktree.ProjectRoot != filepath.Join(root, "app") {
		t.Fatalf("ProjectRoot = %q", worktree.ProjectRoot)
	}
	if worktree.SHA != testSHA {
		t.Fatalf("SHA = %q, want %q", worktree.SHA, testSHA)
	}

	log := readFile(t, logPath)
	for _, want := range []string{
		"init --quiet " + root,
		"-C " + root + " remote add origin https://github.com/helmrdotdev/helmr.git",
		"-C " + root + " fetch --depth=1 origin " + testSHA,
		"-C " + root + " checkout --detach --quiet " + testSHA,
		"-C " + root + " rev-parse HEAD",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("git log missing %q:\n%s", want, log)
		}
	}
}

func TestCloneUsesTokenProviderWithoutTokenInArguments(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	gitPath := fakeGit(t)
	logPath := filepath.Join(tempDir, "git.log")
	homeLogPath := filepath.Join(tempDir, "git-home.log")
	secretToken := "secret-token"
	expectedAuth := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+secretToken))
	providerCalled := false

	t.Setenv("FAKE_GIT_LOG", logPath)
	t.Setenv("FAKE_GIT_HOME_LOG", homeLogPath)
	t.Setenv("FAKE_EXPECT_SHA", testSHA)
	t.Setenv("FAKE_REQUIRE_AUTH", "1")
	t.Setenv("FAKE_EXPECT_AUTH", expectedAuth)
	t.Setenv("FAKE_FORBIDDEN_TOKEN", secretToken)
	t.Setenv("GITHUB_TOKEN", secretToken)
	t.Setenv("GIT_TRACE", "1")
	t.Setenv("GIT_CURL_VERBOSE", "1")
	t.Setenv("GIT_ASKPASS", "bad-helper")
	t.Setenv("HOME", "/unsafe-home")

	_, err := checkout.Clone(
		ctx,
		validSource(""),
		filepath.Join(tempDir, "checkout"),
		checkout.WithGitPath(gitPath),
		checkout.WithTokenProvider(func(_ context.Context, source api.GitHubSource) (string, error) {
			providerCalled = true
			if source.Repository != "helmrdotdev/helmr" || source.SHA != testSHA {
				t.Fatalf("token provider source = %+v", source)
			}
			return secretToken, nil
		}),
	)
	if err != nil {
		t.Fatalf("Clone() error = %v", err)
	}
	if !providerCalled {
		t.Fatal("token provider was not called")
	}
	if strings.Contains(readFile(t, logPath), secretToken) {
		t.Fatal("git arguments included token")
	}
	for _, home := range strings.Fields(readFile(t, homeLogPath)) {
		if _, err := os.Stat(home); !os.IsNotExist(err) {
			t.Fatalf("git home was not cleaned up: %s: %v", home, err)
		}
	}
}

func TestCloneRedactsAuthHeaderFromGitErrors(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	secretToken := "secret-token"
	expectedAuth := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+secretToken))

	t.Setenv("FAKE_GIT_LOG", filepath.Join(tempDir, "git.log"))
	t.Setenv("FAKE_EXPECT_SHA", testSHA)
	t.Setenv("FAKE_FAIL_FETCH_WITH_AUTH", "1")

	_, err := checkout.Clone(
		ctx,
		validSource(""),
		filepath.Join(tempDir, "checkout"),
		checkout.WithGitPath(fakeGit(t)),
		checkout.WithTokenProvider(func(context.Context, api.GitHubSource) (string, error) {
			return secretToken, nil
		}),
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secretToken) || strings.Contains(err.Error(), expectedAuth) {
		t.Fatalf("error leaked token: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted git output]") {
		t.Fatalf("error was not redacted: %v", err)
	}
}

func TestCloneRejectsNonEmptyDestination(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	destination := filepath.Join(tempDir, "checkout")
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destination, "existing"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(tempDir, "git.log")

	t.Setenv("FAKE_GIT_LOG", logPath)
	t.Setenv("FAKE_EXPECT_SHA", testSHA)

	_, err := checkout.Clone(ctx, validSource(""), destination, checkout.WithGitPath(fakeGit(t)))
	if err == nil || !strings.Contains(err.Error(), "destination must be empty") {
		t.Fatalf("Clone() error = %v, want destination must be empty", err)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("git was invoked before rejecting destination")
	}
}

func TestCloneRejectsSymlinkDestination(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	target := filepath.Join(tempDir, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(tempDir, "checkout")
	if err := os.Symlink(target, destination); err != nil {
		t.Fatal(err)
	}

	_, err := checkout.Clone(ctx, validSource(""), destination, checkout.WithGitPath(fakeGit(t)))
	if err == nil || !strings.Contains(err.Error(), "destination exists and is a symlink") {
		t.Fatalf("Clone() error = %v, want symlink destination", err)
	}
}

func TestCloneRejectsInvalidSource(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()

	tests := map[string]api.GitHubSource{
		"repository":       {Repository: "not-a-repo", SHA: testSHA},
		"sha missing":      {Repository: "helmrdotdev/helmr"},
		"sha short":        {Repository: "helmrdotdev/helmr", SHA: "abc"},
		"sha uppercase":    {Repository: "helmrdotdev/helmr", SHA: strings.ToUpper(testSHA)},
		"absolute subpath": {Repository: "helmrdotdev/helmr", SHA: testSHA, Subpath: "/app"},
		"escaping subpath": {Repository: "helmrdotdev/helmr", SHA: testSHA, Subpath: "../app"},
		"nul subpath":      {Repository: "helmrdotdev/helmr", SHA: testSHA, Subpath: "app\x00x"},
		"nul ref":          {Repository: "helmrdotdev/helmr", Ref: "main\x00x", SHA: testSHA},
	}

	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			logPath := filepath.Join(tempDir, name+".log")
			t.Setenv("FAKE_GIT_LOG", logPath)
			_, err := checkout.Clone(ctx, source, filepath.Join(tempDir, name), checkout.WithGitPath(fakeGit(t)))
			if err == nil {
				t.Fatal("Clone() error = nil")
			}
			if _, err := os.Stat(logPath); !os.IsNotExist(err) {
				t.Fatalf("git was invoked for invalid source")
			}
		})
	}
}

func TestCloneVerifiesHeadMatchesSHA(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()
	wrongSHA := "1111111111111111111111111111111111111111"

	t.Setenv("FAKE_GIT_LOG", filepath.Join(tempDir, "git.log"))
	t.Setenv("FAKE_EXPECT_SHA", testSHA)
	t.Setenv("FAKE_GIT_HEAD", wrongSHA)

	_, err := checkout.Clone(ctx, validSource(""), filepath.Join(tempDir, "checkout"), checkout.WithGitPath(fakeGit(t)))
	if err == nil || !strings.Contains(err.Error(), "does not match source sha") {
		t.Fatalf("Clone() error = %v, want HEAD mismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(tempDir, "checkout")); !os.IsNotExist(statErr) {
		t.Fatalf("failed checkout was not cleaned up: %v", statErr)
	}
}

func TestCloneRejectsMissingSubpath(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()

	t.Setenv("FAKE_GIT_LOG", filepath.Join(tempDir, "git.log"))
	t.Setenv("FAKE_EXPECT_SHA", testSHA)

	_, err := checkout.Clone(ctx, validSource("missing"), filepath.Join(tempDir, "checkout"), checkout.WithGitPath(fakeGit(t)))
	if err == nil || !strings.Contains(err.Error(), `source subpath "missing"`) {
		t.Fatalf("Clone() error = %v, want missing subpath", err)
	}
}

func TestCloneRejectsSymlinkSubpath(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()

	t.Setenv("FAKE_GIT_LOG", filepath.Join(tempDir, "git.log"))
	t.Setenv("FAKE_EXPECT_SHA", testSHA)
	t.Setenv("FAKE_SYMLINK_APP", "1")

	_, err := checkout.Clone(ctx, validSource("app"), filepath.Join(tempDir, "checkout"), checkout.WithGitPath(fakeGit(t)))
	if err == nil || !strings.Contains(err.Error(), `source subpath "app" contains symlink`) {
		t.Fatalf("Clone() error = %v, want symlink subpath", err)
	}
}

func TestCloneRejectsIntermediateSymlinkSubpath(t *testing.T) {
	ctx := context.Background()
	tempDir := t.TempDir()

	t.Setenv("FAKE_GIT_LOG", filepath.Join(tempDir, "git.log"))
	t.Setenv("FAKE_EXPECT_SHA", testSHA)
	t.Setenv("FAKE_SYMLINK_PARENT", "1")

	_, err := checkout.Clone(ctx, validSource("link/app"), filepath.Join(tempDir, "checkout"), checkout.WithGitPath(fakeGit(t)))
	if err == nil || !strings.Contains(err.Error(), `source subpath "link/app" contains symlink`) {
		t.Fatalf("Clone() error = %v, want intermediate symlink subpath", err)
	}
}

func validSource(subpath string) api.GitHubSource {
	return api.GitHubSource{
		Repository: "helmrdotdev/helmr",
		Ref:        "main",
		SHA:        testSHA,
		Subpath:    subpath,
	}
}

func fakeGit(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu

if [ -n "${FAKE_FORBIDDEN_TOKEN:-}" ]; then
	if [ "${GITHUB_TOKEN:-}" = "${FAKE_FORBIDDEN_TOKEN}" ]; then
		echo "token found in env" >&2
		exit 87
	fi
	for arg in "$@"; do
		case "$arg" in
			*"${FAKE_FORBIDDEN_TOKEN}"*) echo "token found in argv" >&2; exit 88 ;;
		esac
	done
fi

if [ "${FAKE_REQUIRE_AUTH:-}" = "1" ]; then
	if [ "${GIT_TRACE:-}" != "" ] || [ "${GIT_CURL_VERBOSE:-}" != "" ]; then
		echo "unsafe git trace env was preserved" >&2
		exit 86
	fi
	if [ "${GIT_ASKPASS:-}" != "true" ] || [ "${HOME:-}" = "/unsafe-home" ]; then
		echo "git env was not sanitized" >&2
		exit 85
	fi
	if [ "${GIT_CONFIG_VALUE_0:-}" != "${FAKE_EXPECT_AUTH:-}" ]; then
		echo "missing expected auth" >&2
		exit 89
	fi
fi

printf '%s\n' "$*" >> "$FAKE_GIT_LOG"
if [ -n "${FAKE_GIT_HOME_LOG:-}" ]; then
	printf '%s\n' "$HOME" >> "$FAKE_GIT_HOME_LOG"
fi

root=""
if [ "${1:-}" = "init" ]; then
	root="${3:-}"
	mkdir -p "$root/.git"
	exit 0
fi

if [ "${1:-}" = "-C" ]; then
	root="$2"
	shift 2
fi

case "${1:-}" in
	remote)
		exit 0
		;;
	fetch)
		if [ "${FAKE_FAIL_FETCH_WITH_AUTH:-}" = "1" ]; then
			echo "${GIT_CONFIG_VALUE_0:-missing auth}" >&2
			exit 84
		fi
		exit 0
		;;
	checkout)
		if [ "${FAKE_SYMLINK_APP:-}" = "1" ]; then
			mkdir -p "$root/real-app"
			ln -s real-app "$root/app"
		elif [ "${FAKE_SYMLINK_PARENT:-}" = "1" ]; then
			mkdir -p "$root/outside/app"
			ln -s outside "$root/link"
		else
			mkdir -p "$root/app"
			: > "$root/app/file.txt"
		fi
		printf '%s\n' "${FAKE_GIT_HEAD:-${FAKE_EXPECT_SHA}}" > "$root/.git/HEAD_VALUE"
		exit 0
		;;
	rev-parse)
		cat "$root/.git/HEAD_VALUE"
		exit 0
		;;
	*)
		echo "unexpected git command: $*" >&2
		exit 99
		;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
