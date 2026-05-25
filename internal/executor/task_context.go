package executor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
)

func runTaskSourceProto(source api.GitHubSource) (*runv0.RunTaskSource, error) {
	if strings.TrimSpace(source.Repository) == "" {
		return nil, fmt.Errorf("task context source.repository is required")
	}
	if strings.TrimSpace(source.Ref) == "" {
		return nil, fmt.Errorf("task context source.requested_ref is required")
	}
	if strings.TrimSpace(source.SHA) == "" {
		return nil, fmt.Errorf("task context source.resolved_sha is required")
	}
	githubSource := &runv0.RunTaskGitHubSource{
		Repository:   source.Repository,
		RequestedRef: source.Ref,
		ResolvedSha:  source.SHA,
	}
	if source.Subpath != "" {
		githubSource.Subpath = &source.Subpath
	}
	if source.RefKind != "" {
		refKind := string(source.RefKind)
		githubSource.RefKind = &refKind
	}
	if source.RefName != "" {
		githubSource.RefName = &source.RefName
	}
	if source.FullRef != "" {
		githubSource.FullRef = &source.FullRef
	}
	if source.DefaultBranch != "" {
		githubSource.DefaultBranch = &source.DefaultBranch
	}
	if source.PullRequest != nil {
		githubSource.PullRequest = &runv0.GitHubPullRequestMetadata{
			Number:  source.PullRequest.Number,
			BaseRef: source.PullRequest.BaseRef,
			BaseSha: source.PullRequest.BaseSHA,
			HeadRef: source.PullRequest.HeadRef,
			HeadSha: source.PullRequest.HeadSHA,
		}
	}
	return &runv0.RunTaskSource{
		Kind: &runv0.RunTaskSource_Github{Github: githubSource},
	}, nil
}

func runTaskWorkspaceProto(mountPath string) *runv0.RunTaskWorkspace {
	mountPath = strings.TrimSpace(mountPath)
	if mountPath == "" {
		mountPath = "/workspace"
	}
	return &runv0.RunTaskWorkspace{
		Path:        mountPath,
		ProjectPath: mountPath,
	}
}

func taskContextJSON(runID, taskID string, source api.GitHubSource, workspace *runv0.RunTaskWorkspace) (string, error) {
	if strings.TrimSpace(runID) == "" {
		return "", fmt.Errorf("task context run.id is required")
	}
	if strings.TrimSpace(taskID) == "" {
		return "", fmt.Errorf("task context task.id is required")
	}
	if workspace == nil {
		return "", fmt.Errorf("task context workspace is required")
	}
	payload := map[string]any{
		"run":  map[string]string{"id": runID},
		"task": map[string]string{"id": taskID},
		"source": map[string]any{
			"kind":         "github",
			"repository":   source.Repository,
			"requestedRef": source.Ref,
			"resolvedSha":  source.SHA,
		},
		"workspace": map[string]string{
			"path":        workspace.Path,
			"projectPath": workspace.ProjectPath,
		},
	}
	sourcePayload := payload["source"].(map[string]any)
	if source.Subpath != "" {
		sourcePayload["subpath"] = source.Subpath
	}
	if source.RefKind != "" {
		sourcePayload["refKind"] = string(source.RefKind)
	}
	if source.RefName != "" {
		sourcePayload["refName"] = source.RefName
	}
	if source.FullRef != "" {
		sourcePayload["fullRef"] = source.FullRef
	}
	if source.DefaultBranch != "" {
		sourcePayload["defaultBranch"] = source.DefaultBranch
	}
	if source.PullRequest != nil {
		sourcePayload["pullRequest"] = map[string]any{
			"number":  source.PullRequest.Number,
			"baseRef": source.PullRequest.BaseRef,
			"baseSha": source.PullRequest.BaseSHA,
			"headRef": source.PullRequest.HeadRef,
			"headSha": source.PullRequest.HeadSHA,
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode task context json: %w", err)
	}
	return string(encoded), nil
}
