package executor

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/proto/run/v0"
)

func taskContextJSON(runID, taskID string, workspace *runv0.RunTaskWorkspace) (string, error) {
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
