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
	payload := map[string]any{
		"run":  map[string]string{"id": request.RunId},
		"task": map[string]string{"id": request.TaskId},
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
