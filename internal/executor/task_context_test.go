package executor

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestRunTaskSourceProto(t *testing.T) {
	source, err := runTaskSourceProto(api.GitHubSource{
		Repository:    "helmrdotdev/helmr",
		Ref:           "refs/pull/42/head",
		SHA:           testResolvedSHA,
		RefKind:       api.GitHubRefKindPullRequest,
		RefName:       "42",
		FullRef:       "refs/pull/42/head",
		DefaultBranch: "main",
		PullRequest: &api.GitHubPullRequestMetadata{
			Number:  42,
			BaseRef: "main",
			BaseSHA: testResolvedSHA,
			HeadRef: "feature",
			HeadSHA: "0123456789abcdef0123456789abcdef01234568",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	githubSource := source.GetGithub()
	if githubSource == nil {
		t.Fatal("expected github source")
	}
	if githubSource.Repository != "helmrdotdev/helmr" || githubSource.RequestedRef != "refs/pull/42/head" || githubSource.ResolvedSha != testResolvedSHA {
		t.Fatalf("github source = %+v", githubSource)
	}
	if githubSource.GetRefKind() != "pull_request" {
		t.Fatalf("ref kind = %q", githubSource.GetRefKind())
	}
	if githubSource.GetPullRequest().GetBaseRef() != "main" {
		t.Fatalf("pull request = %+v", githubSource.GetPullRequest())
	}
}

func TestTaskContextJSON(t *testing.T) {
	jsonPayload, err := taskContextJSON("run-1", "deploy", testWorkerGitHubSource(), runTaskWorkspaceProto("/workspace"))
	if err != nil {
		t.Fatal(err)
	}
	if !containsAll(jsonPayload,
		`"id":"run-1"`,
		`"id":"deploy"`,
		`"kind":"github"`,
		`"repository":"helmrdotdev/helmr"`,
		`"requestedRef":"main"`,
		`"resolvedSha":"0123456789abcdef0123456789abcdef01234567"`,
		`"refKind":"branch"`,
		`"path":"/workspace"`,
		`"projectPath":"/workspace"`,
	) {
		t.Fatalf("json = %s", jsonPayload)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !contains(value, part) {
			return false
		}
	}
	return true
}

func contains(value, part string) bool {
	return len(part) == 0 || (len(value) >= len(part) && indexOf(value, part) >= 0)
}

func indexOf(value, part string) int {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return i
		}
	}
	return -1
}
