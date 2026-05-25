package ghapp

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	ghapi "github.com/google/go-github/v75/github"
	"github.com/helmrdotdev/helmr/internal/api"
)

var pullRequestRefPattern = regexp.MustCompile(`^(?:refs/)?pull/(\d+)/head$`)

func enrichResolvedSource(ctx context.Context, client *ghapi.Client, owner, repo string, source api.GitHubSource) (api.GitHubSource, error) {
	repository, _, err := client.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return api.GitHubSource{}, fmt.Errorf("load github repository metadata: %w", err)
	}
	resolved, err := resolveGitHubRef(ctx, client, owner, repo, source.Ref)
	if err != nil {
		return api.GitHubSource{}, err
	}
	source.SHA = resolved.sha
	source.RefKind = resolved.kind
	source.RefName = resolved.refName
	source.FullRef = resolved.fullRef
	source.DefaultBranch = strings.TrimSpace(repository.GetDefaultBranch())
	source.PullRequest = resolved.pullRequest
	return source, nil
}

type refResolution struct {
	kind        api.GitHubRefKind
	refName     string
	fullRef     string
	sha         string
	pullRequest *api.GitHubPullRequestMetadata
}

func resolveGitHubRef(ctx context.Context, client *ghapi.Client, owner, repo, requestedRef string) (refResolution, error) {
	requestedRef = strings.TrimSpace(requestedRef)
	if requestedRef == "" {
		return refResolution{}, invalidSource("source.ref is required")
	}
	if isFullGitSHA(requestedRef) {
		return resolveLiteralSHARef(ctx, client, owner, repo, requestedRef)
	}
	if number, ok := parsePullRequestRef(requestedRef); ok {
		return resolvePullRequestRef(ctx, client, owner, repo, number)
	}
	if strings.HasPrefix(requestedRef, "refs/heads/") {
		name := strings.TrimPrefix(requestedRef, "refs/heads/")
		sha, err := resolveGitRef(ctx, client, owner, repo, requestedRef)
		if err != nil {
			return refResolution{}, err
		}
		return refResolution{
			kind:    api.GitHubRefKindBranch,
			refName: name,
			fullRef: requestedRef,
			sha:     sha,
		}, nil
	}
	if strings.HasPrefix(requestedRef, "refs/tags/") {
		name := strings.TrimPrefix(requestedRef, "refs/tags/")
		sha, err := resolveGitRef(ctx, client, owner, repo, requestedRef)
		if err != nil {
			return refResolution{}, err
		}
		return refResolution{
			kind:    api.GitHubRefKindTag,
			refName: name,
			fullRef: requestedRef,
			sha:     sha,
		}, nil
	}
	if strings.HasPrefix(requestedRef, "refs/") {
		sha, err := resolveGitRef(ctx, client, owner, repo, requestedRef)
		if err != nil {
			return refResolution{}, err
		}
		return refResolution{
			kind:    api.GitHubRefKindUnknown,
			refName: requestedRef,
			fullRef: requestedRef,
			sha:     sha,
		}, nil
	}
	if sha, err := resolveGitRef(ctx, client, owner, repo, "refs/heads/"+requestedRef); err == nil {
		return refResolution{
			kind:    api.GitHubRefKindBranch,
			refName: requestedRef,
			fullRef: "refs/heads/" + requestedRef,
			sha:     sha,
		}, nil
	}
	if sha, err := resolveGitRef(ctx, client, owner, repo, "refs/tags/"+requestedRef); err == nil {
		return refResolution{
			kind:    api.GitHubRefKindTag,
			refName: requestedRef,
			fullRef: "refs/tags/" + requestedRef,
			sha:     sha,
		}, nil
	}
	commit, _, err := client.Repositories.GetCommit(ctx, owner, repo, requestedRef, nil)
	if err != nil {
		return refResolution{}, err
	}
	sha := strings.TrimSpace(strings.ToLower(commit.GetSHA()))
	if !isFullGitSHA(sha) {
		return refResolution{}, fmt.Errorf("github resolved ref %q for %s/%s to invalid commit sha %q", requestedRef, owner, repo, sha)
	}
	return refResolution{
		kind:    api.GitHubRefKindUnknown,
		refName: requestedRef,
		fullRef: requestedRef,
		sha:     sha,
	}, nil
}

func resolveLiteralSHARef(ctx context.Context, client *ghapi.Client, owner, repo, requestedRef string) (refResolution, error) {
	sha := strings.ToLower(strings.TrimSpace(requestedRef))
	commit, _, err := client.Repositories.GetCommit(ctx, owner, repo, sha, nil)
	if err != nil {
		if IsNotFound(err) {
			return refResolution{}, invalidSource(fmt.Sprintf("source.ref %q does not exist", sha))
		}
		return refResolution{}, err
	}
	resolvedSHA, err := normalizeResolvedSHA(commit.GetSHA())
	if err != nil {
		return refResolution{}, fmt.Errorf("github resolved ref %q for %s/%s: %w", requestedRef, owner, repo, err)
	}
	return refResolution{
		kind:    api.GitHubRefKindSHA,
		refName: sha,
		fullRef: sha,
		sha:     resolvedSHA,
	}, nil
}

func resolvePullRequestRef(ctx context.Context, client *ghapi.Client, owner, repo string, number int) (refResolution, error) {
	sha, err := resolveGitRef(ctx, client, owner, repo, fmt.Sprintf("refs/pull/%d/head", number))
	if err != nil {
		return refResolution{}, err
	}
	pullRequest, err := fetchPullRequestMetadata(ctx, client, owner, repo, number)
	if err != nil {
		return refResolution{}, err
	}
	return refResolution{
		kind:        api.GitHubRefKindPullRequest,
		refName:     strconv.Itoa(number),
		fullRef:     fmt.Sprintf("refs/pull/%d/head", number),
		sha:         sha,
		pullRequest: pullRequest,
	}, nil
}

func parsePullRequestRef(ref string) (int, bool) {
	matches := pullRequestRefPattern.FindStringSubmatch(strings.TrimSpace(ref))
	if len(matches) != 2 {
		return 0, false
	}
	number, err := strconv.Atoi(matches[1])
	if err != nil || number <= 0 {
		return 0, false
	}
	return number, true
}

func fetchPullRequestMetadata(ctx context.Context, client *ghapi.Client, owner, repo string, number int) (*api.GitHubPullRequestMetadata, error) {
	pullRequest, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("load github pull request #%d: %w", number, err)
	}
	metadata := &api.GitHubPullRequestMetadata{
		Number:  int32(number),
		BaseRef: strings.TrimSpace(pullRequest.GetBase().GetRef()),
		BaseSHA: strings.TrimSpace(strings.ToLower(pullRequest.GetBase().GetSHA())),
		HeadRef: strings.TrimSpace(pullRequest.GetHead().GetRef()),
		HeadSHA: strings.TrimSpace(strings.ToLower(pullRequest.GetHead().GetSHA())),
	}
	if metadata.BaseRef == "" || metadata.HeadRef == "" {
		return nil, fmt.Errorf("github pull request #%d is missing base or head ref metadata", number)
	}
	return metadata, nil
}

func normalizeResolvedSHA(sha string) (string, error) {
	sha = strings.TrimSpace(strings.ToLower(sha))
	if !isFullGitSHA(sha) {
		return "", fmt.Errorf("github resolved commit sha %q is invalid", sha)
	}
	return sha, nil
}
