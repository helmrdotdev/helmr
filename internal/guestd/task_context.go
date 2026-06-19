package guestd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/proto/run/v0"
)

func adapterTaskContextJSON(request *runv0.RunTaskRequest) (string, error) {
	if request == nil {
		return "", errors.New("run task request is required")
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
	taskSessionID := strings.TrimSpace(request.TaskSessionId)
	if taskSessionID == "" {
		return "", errors.New("task context session.id is required")
	}
	run := map[string]any{
		"id": request.RunId,
	}
	if request.AttemptId != "" {
		run["attemptId"] = request.AttemptId
	}
	if request.AttemptNumber > 0 {
		run["attemptNumber"] = request.AttemptNumber
	}
	if request.RunLeaseId != "" {
		run["runLeaseId"] = request.RunLeaseId
	}
	if request.SnapshotVersion > 0 {
		run["snapshotVersion"] = request.SnapshotVersion
	}
	workspaceContext := map[string]string{
		"path":        workspace.Path,
		"projectPath": workspace.ProjectPath,
	}
	payload := map[string]any{
		"run":       run,
		"task":      map[string]string{"id": request.TaskId},
		"workspace": workspaceContext,
		"session": map[string]any{
			"id":        taskSessionID,
			"workspace": workspaceContext,
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode task context json: %w", err)
	}
	return string(encoded), nil
}
