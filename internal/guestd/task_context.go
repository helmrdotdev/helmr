package guestd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
)

func adapterTaskContextJSON(request *runv0.RunTaskRequest) (string, error) {
	if request == nil {
		return "", errors.New("run task request is required")
	}
	githubSource := request.GetSource().GetGithub()
	if githubSource == nil {
		return "", errors.New("task context source is required")
	}
	if strings.TrimSpace(githubSource.Repository) == "" {
		return "", errors.New("task context source.repository is required")
	}
	if strings.TrimSpace(githubSource.RequestedRef) == "" {
		return "", errors.New("task context source.requested_ref is required")
	}
	if strings.TrimSpace(githubSource.ResolvedSha) == "" {
		return "", errors.New("task context source.resolved_sha is required")
	}
	workspace := request.GetWorkspace()
	if workspace == nil {
		return "", errors.New("task context workspace is required")
	}
	if strings.TrimSpace(workspace.Path) == "" {
		return "", errors.New("task context workspace.path is required")
	}
	if strings.TrimSpace(workspace.ProjectPath) == "" {
		return "", errors.New("task context workspace.project_path is required")
	}
	sourcePayload := map[string]any{
		"kind":         "github",
		"repository":   githubSource.Repository,
		"requestedRef": githubSource.RequestedRef,
		"resolvedSha":  githubSource.ResolvedSha,
	}
	if subpath := strings.TrimSpace(githubSource.GetSubpath()); subpath != "" {
		sourcePayload["subpath"] = subpath
	}
	if kind := strings.TrimSpace(githubSource.GetRefKind()); kind != "" {
		sourcePayload["refKind"] = kind
	}
	if refName := strings.TrimSpace(githubSource.GetRefName()); refName != "" {
		sourcePayload["refName"] = refName
	}
	if fullRef := strings.TrimSpace(githubSource.GetFullRef()); fullRef != "" {
		sourcePayload["fullRef"] = fullRef
	}
	if defaultBranch := strings.TrimSpace(githubSource.GetDefaultBranch()); defaultBranch != "" {
		sourcePayload["defaultBranch"] = defaultBranch
	}
	if pullRequest := githubSource.GetPullRequest(); pullRequest != nil {
		sourcePayload["pullRequest"] = map[string]any{
			"number":  pullRequest.Number,
			"baseRef": pullRequest.BaseRef,
			"baseSha": pullRequest.BaseSha,
			"headRef": pullRequest.HeadRef,
			"headSha": pullRequest.HeadSha,
		}
	}
	payload := map[string]any{
		"run":    map[string]string{"id": request.RunId},
		"task":   map[string]string{"id": request.TaskId},
		"source": sourcePayload,
		"workspace": map[string]string{
			"path":        workspace.Path,
			"projectPath": workspace.ProjectPath,
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode task context json: %w", err)
	}
	return string(encoded), nil
}
