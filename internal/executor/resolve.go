package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	bundlev0 "github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
)

type ResolvedRun struct {
	RunID          string
	TaskID         string
	Bundle         *bundlev0.Bundle
	Payload        json.RawMessage
	Secrets        api.ResolvedSecrets
	TaskSource     api.TaskSourceArtifact
	Workspace      api.GitHubSource
	DeploymentTask api.WorkerDeploymentTask
	Restore        *api.WorkerRestore
	MaxDuration    time.Duration
	ActiveUsed     time.Duration
}

const maxActiveDurationMilliseconds = int64(1<<63-1) / int64(time.Millisecond)

func Resolve(run api.WorkerRun) (ResolvedRun, error) {
	if run.ID == "" {
		return ResolvedRun{}, errors.New("worker run id is required")
	}
	if run.TaskID == "" {
		return ResolvedRun{}, errors.New("worker run task_id is required")
	}
	payload := defaultJSON(run.Payload)
	if !json.Valid(payload) {
		return ResolvedRun{}, errors.New("worker run payload must be valid JSON")
	}
	if run.Restore == nil {
		if err := validateWorkerSourceArtifact("task_source", run.TaskSource); err != nil {
			return ResolvedRun{}, err
		}
		if err := validateWorkerGitHubSource("workspace", run.Workspace); err != nil {
			return ResolvedRun{}, err
		}
	}
	maxDurationSeconds := run.MaxDurationSeconds
	if maxDurationSeconds <= 0 {
		return ResolvedRun{}, errors.New("worker run max_duration_seconds is required")
	}
	if run.ActiveDurationMs < 0 {
		return ResolvedRun{}, errors.New("worker run active_duration_ms must be non-negative")
	}
	if run.ActiveDurationMs > maxActiveDurationMilliseconds {
		return ResolvedRun{}, fmt.Errorf("worker run active_duration_ms exceeds max %d", maxActiveDurationMilliseconds)
	}

	return ResolvedRun{
		RunID:          run.ID,
		TaskID:         run.TaskID,
		Payload:        payload,
		Secrets:        cloneSecrets(run.Secrets),
		TaskSource:     run.TaskSource,
		Workspace:      run.Workspace,
		DeploymentTask: run.DeploymentTask,
		Restore:        run.Restore,
		MaxDuration:    time.Duration(maxDurationSeconds) * time.Second,
		ActiveUsed:     time.Duration(run.ActiveDurationMs) * time.Millisecond,
	}, nil
}

func cloneSecrets(values api.ResolvedSecrets) api.ResolvedSecrets {
	if len(values) == 0 {
		return api.ResolvedSecrets{}
	}
	clone := make(api.ResolvedSecrets, len(values))
	for name, value := range values {
		clone[name] = append([]byte(nil), value...)
	}
	return clone
}

func defaultJSON(value json.RawMessage) json.RawMessage {
	if len(value) == 0 {
		return json.RawMessage(`{}`)
	}
	return value
}

func validateWorkerGitHubSource(field string, source api.GitHubSource) error {
	if strings.TrimSpace(source.Repository) == "" {
		return fmt.Errorf("worker run %s.repository is required", field)
	}
	if strings.TrimSpace(source.SHA) == "" {
		return fmt.Errorf("worker run %s.sha is required", field)
	}
	if strings.TrimSpace(source.Ref) == "" {
		return fmt.Errorf("worker run %s.ref is required", field)
	}
	if strings.ContainsRune(source.Subpath, '\x00') {
		return fmt.Errorf("worker run %s.subpath contains NUL", field)
	}
	return nil
}

func validateWorkerSourceArtifact(field string, artifact api.TaskSourceArtifact) error {
	if _, err := cas.ObjectKey("", strings.TrimSpace(artifact.Digest)); err != nil {
		return fmt.Errorf("worker run %s.digest is invalid: %w", field, err)
	}
	return nil
}
