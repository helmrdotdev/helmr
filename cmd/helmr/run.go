package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cli/format"
	"github.com/helmrdotdev/helmr/internal/cli/ui"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

var (
	runEventReconnectDelay           = time.Second
	runTerminalSnapshotRetryDelay    = 100 * time.Millisecond
	runTerminalSnapshotConvergeLimit = 5 * time.Second
)

func runCommand() *cobra.Command {
	var payloadFile string
	var payloadJSON string
	var payloadPairs []string
	var projectID string
	var environmentID string
	var deploymentID string
	var version string
	var queueName string
	var concurrencyKey string
	var priority int32
	var ttl string
	var maxDurationSeconds int32
	var metadataFile string
	var metadataJSON string
	var tags []string
	var retryFile string
	var retryJSON string
	var idempotencyKey string
	var idempotencyKeyTTL string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "run TASK",
		Short: "Create a remote run.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			payload, err := parsePayload(payloadFile, payloadJSON, payloadPairs)
			if err != nil {
				return err
			}
			if err := api.ValidateTaskID(args[0]); err != nil {
				return err
			}
			metadata, err := parseOptionalJSON(metadataFile, metadataJSON, "--metadata")
			if err != nil {
				return err
			}
			retry, err := parseOptionalJSON(retryFile, retryJSON, "--retry")
			if err != nil {
				return err
			}
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			projectID = strings.TrimSpace(projectID)
			if projectID != "" {
				if err := validateProjectFlag(projectID); err != nil {
					return err
				}
			}
			scope, err := runScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			options := api.CreateRunOptions{
				DeploymentID:       strings.TrimSpace(deploymentID),
				Version:            strings.TrimSpace(version),
				ConcurrencyKey:     strings.TrimSpace(concurrencyKey),
				Priority:           priority,
				TTL:                strings.TrimSpace(ttl),
				MaxDurationSeconds: maxDurationSeconds,
				Retry:              retry,
				Metadata:           metadata,
				Tags:               cleanTags(tags),
				IdempotencyKey:     strings.TrimSpace(idempotencyKey),
				IdempotencyKeyTTL:  strings.TrimSpace(idempotencyKeyTTL),
			}
			if queueName = strings.TrimSpace(queueName); queueName != "" {
				options.Queue = &api.RunQueueOption{Name: queueName}
			}
			run, err := control.CreateRun(cmd.Context(), api.CreateRunRequest{
				ProjectID:     scope.ProjectID,
				EnvironmentID: scope.EnvironmentID,
				TaskID:        args[0],
				Payload:       payload,
				Options:       options,
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), run)
			}
			fmt.Fprintln(cmd.OutOrStdout(), run.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&payloadFile, "payload-file", "", "Read payload JSON from a file.")
	cmd.Flags().StringVar(&payloadJSON, "payload-json", "", "Inline payload JSON literal.")
	cmd.Flags().StringArrayVar(&payloadPairs, "payload", nil, "Add a top-level string payload field as KEY=VALUE.")
	cmd.Flags().StringVarP(&projectID, "project", "p", "", "Project slug or ID for this run.")
	cmd.Flags().StringVarP(&environmentID, "env", "e", "", "Environment slug or ID for this run.")
	cmd.Flags().StringVar(&deploymentID, "deployment", "", "Deployment ID to pin for this run.")
	cmd.Flags().StringVar(&version, "version", "", "Deployment version to pin for this run.")
	cmd.Flags().StringVar(&queueName, "queue", "", "Queue name for this run.")
	cmd.Flags().StringVar(&concurrencyKey, "concurrency-key", "", "Concurrency key for this run.")
	cmd.Flags().Int32Var(&priority, "priority", 0, "Run priority offset in seconds.")
	cmd.Flags().StringVar(&ttl, "ttl", "", "Queued run time-to-live before execution starts, for example 10m or 1h.")
	cmd.Flags().Int32Var(&maxDurationSeconds, "max-duration-seconds", 0, "Maximum run duration in seconds.")
	cmd.Flags().StringVar(&metadataFile, "metadata-file", "", "Read metadata JSON from a file.")
	cmd.Flags().StringVar(&metadataJSON, "metadata-json", "", "Inline metadata JSON literal.")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "Add a run tag. Repeat for multiple tags.")
	cmd.Flags().StringVar(&retryFile, "retry-file", "", "Read retry policy JSON from a file.")
	cmd.Flags().StringVar(&retryJSON, "retry-json", "", "Inline retry policy JSON literal.")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for safe retries.")
	cmd.Flags().StringVar(&idempotencyKeyTTL, "idempotency-key-ttl", "", "Duration to retain the idempotency key, for example 30d or 24h.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.MarkFlagsMutuallyExclusive("deployment", "version")
	cmd.MarkFlagsMutuallyExclusive("metadata-file", "metadata-json")
	cmd.MarkFlagsMutuallyExclusive("retry-file", "retry-json")
	return cmd
}

func cancelCommand() *cobra.Command {
	var reason string
	var force bool
	var idempotencyKey string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "cancel RUN",
		Short: "Cancel a run.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := resolveRunScope(cmd.Context(), control, args[0])
			if err != nil {
				return err
			}
			response, err := control.CancelRun(cmd.Context(), args[0], api.CancelRunRequest{
				Reason:         strings.TrimSpace(reason),
				Force:          force,
				IdempotencyKey: strings.TrimSpace(idempotencyKey),
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", response.Run.ID, response.Run.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Reason for the cancellation.")
	cmd.Flags().BoolVar(&force, "force", false, "Force cancellation without waiting for graceful shutdown.")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for safe retries.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func replayCommand() *cobra.Command {
	var version string
	var payloadFile string
	var payloadJSON string
	var payloadPairs []string
	var metadataFile string
	var metadataJSON string
	var tags []string
	var reason string
	var idempotencyKey string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "replay RUN",
		Short: "Replay a run.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			payload, err := parseOptionalPayload(payloadFile, payloadJSON, payloadPairs)
			if err != nil {
				return err
			}
			metadata, err := parseOptionalJSON(metadataFile, metadataJSON, "--metadata")
			if err != nil {
				return err
			}
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := resolveRunScope(cmd.Context(), control, args[0])
			if err != nil {
				return err
			}
			response, err := control.ReplayRun(cmd.Context(), args[0], api.ReplayRunRequest{
				Version:        strings.TrimSpace(version),
				Payload:        payload,
				Reason:         strings.TrimSpace(reason),
				IdempotencyKey: strings.TrimSpace(idempotencyKey),
				Metadata:       metadata,
				Tags:           optionalTags(tags),
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			fmt.Fprintln(cmd.OutOrStdout(), response.Run.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&version, "version", "", "Replay version: original, latest, or a deployment version.")
	cmd.Flags().StringVar(&payloadFile, "payload-file", "", "Read payload JSON from a file.")
	cmd.Flags().StringVar(&payloadJSON, "payload-json", "", "Inline payload JSON literal.")
	cmd.Flags().StringArrayVar(&payloadPairs, "payload", nil, "Add a top-level string payload field as KEY=VALUE.")
	cmd.Flags().StringVar(&metadataFile, "metadata-file", "", "Read metadata JSON from a file.")
	cmd.Flags().StringVar(&metadataJSON, "metadata-json", "", "Inline metadata JSON literal.")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "Replace run tags. Repeat for multiple tags.")
	cmd.Flags().StringVar(&reason, "reason", "", "Reason for the replay.")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for safe retries.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.MarkFlagsMutuallyExclusive("payload-file", "payload-json", "payload")
	cmd.MarkFlagsMutuallyExclusive("metadata-file", "metadata-json")
	return cmd
}

func psCommand() *cobra.Command {
	var jsonOutput bool
	var projectID string
	var environmentID string
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List runs.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			response, err := control.ListRuns(cmd.Context(), client.ListRunsOptions{
				Status:        "all",
				ProjectID:     strings.TrimSpace(projectID),
				EnvironmentID: strings.TrimSpace(environmentID),
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSONLines(cmd.OutOrStdout(), response.Runs)
			}
			ui.RunTable(cmd.OutOrStdout(), response.Runs)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON run per line.")
	cmd.Flags().StringVarP(&projectID, "project", "p", "", "Project slug or ID to list.")
	cmd.Flags().StringVarP(&environmentID, "env", "e", "", "Environment slug or ID to list.")
	return cmd
}

func showCommand() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "show RUN",
		Short: "Show run details.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := resolveRunScope(cmd.Context(), control, args[0])
			if err != nil {
				return err
			}
			run, err := control.GetRun(cmd.Context(), args[0], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), run)
			}
			ui.RunDetails(cmd.OutOrStdout(), run)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func logsCommand() *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs RUN",
		Short: "Print the latest run logs.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := resolveRunScope(cmd.Context(), control, args[0])
			if err != nil {
				return err
			}
			logs, err := control.GetRunLogs(cmd.Context(), args[0], scope)
			if err != nil {
				return err
			}
			if err := writeRunLogSnapshot(cmd, logs); err != nil {
				return err
			}
			if follow {
				cursor, err := strconv.ParseInt(strings.TrimSpace(logs.Cursor), 10, 64)
				if err != nil && strings.TrimSpace(logs.Cursor) != "" {
					return fmt.Errorf("parse log cursor: %w", err)
				}
				return followRunLogs(cmd, control, args[0], cursor, scope)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&follow, "follow", false, "Continue streaming new logs.")
	return cmd
}

func writeRunLogSnapshot(cmd *cobra.Command, logs api.LogSnapshotResponse) error {
	stdout, err := base64.StdEncoding.DecodeString(logs.StdoutBase64)
	if err != nil {
		return fmt.Errorf("decode stdout logs: %w", err)
	}
	stderr, err := base64.StdEncoding.DecodeString(logs.StderrBase64)
	if err != nil {
		return fmt.Errorf("decode stderr logs: %w", err)
	}
	if _, err := cmd.OutOrStdout().Write(stdout); err != nil {
		return err
	}
	if _, err := cmd.ErrOrStderr().Write(stderr); err != nil {
		return err
	}
	return nil
}

func eventsCommand() *cobra.Command {
	var cursor int64
	var limit int32
	var follow bool
	cmd := &cobra.Command{
		Use:   "events RUN",
		Short: "List run events.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := resolveRunScope(cmd.Context(), control, args[0])
			if err != nil {
				return err
			}
			if !follow {
				page, err := control.ListRunEvents(cmd.Context(), args[0], client.ListRunEventsOptions{Cursor: cursor, Limit: limit, RunScopeOptions: scope})
				if err != nil {
					return err
				}
				return format.JSONLines(cmd.OutOrStdout(), page.Events)
			}
			return followRunEvents(cmd, control, args[0], cursor, scope)
		},
	}
	cmd.Flags().Int64Var(&cursor, "cursor", 0, "Return events after this cursor.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum events to return.")
	cmd.Flags().BoolVar(&follow, "follow", false, "Continue streaming new events.")
	return cmd
}

func waitCommand() *cobra.Command {
	var timeout string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "wait RUN",
		Short: "Wait for a run to finish.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if strings.TrimSpace(timeout) != "" {
				waitTimeout, err := api.ParsePositiveDuration(timeout, "--timeout")
				if err != nil {
					return err
				}
				var cancel func()
				ctx, cancel = context.WithTimeout(ctx, waitTimeout)
				defer cancel()
			}
			scope, err := resolveRunScope(ctx, control, args[0])
			if err != nil {
				return err
			}
			run, err := waitForRun(ctx, control, args[0], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), run)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", run.ID, run.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&timeout, "timeout", "", "Maximum wait duration, for example 10m or 1h.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func runScopeForClient(control *client.Client, projectID string, environmentID string) (client.RunScopeOptions, error) {
	scope := client.RunScopeOptions{
		ProjectID:     strings.TrimSpace(projectID),
		EnvironmentID: strings.TrimSpace(environmentID),
	}
	if !control.UsesSessionScopedRoutes() {
		if scope.ProjectID != "" || scope.EnvironmentID != "" {
			return client.RunScopeOptions{}, errors.New("--project and --env require helmr login; API keys are already environment scoped")
		}
		return client.RunScopeOptions{}, nil
	}
	if scope.ProjectID == "" || scope.EnvironmentID == "" {
		return client.RunScopeOptions{}, errors.New("--project and --env are required with helmr login")
	}
	return scope, nil
}

func resolveRunScope(ctx context.Context, control *client.Client, runID string) (client.RunScopeOptions, error) {
	if !control.UsesSessionScopedRoutes() {
		return client.RunScopeOptions{}, nil
	}
	projects, err := control.ListProjects(ctx)
	if err != nil {
		return client.RunScopeOptions{}, err
	}
	for _, project := range projects.Projects {
		environments := project.Environments
		if len(environments) == 0 {
			detail, err := control.GetProject(ctx, project.ID)
			if err != nil {
				return client.RunScopeOptions{}, err
			}
			environments = detail.Environments
		}
		for _, environment := range environments {
			scope := client.RunScopeOptions{ProjectID: project.ID, EnvironmentID: environment.ID}
			if _, err := control.GetRun(ctx, runID, scope); err == nil {
				return scope, nil
			} else if !client.IsStatus(err, http.StatusNotFound) {
				return client.RunScopeOptions{}, err
			}
		}
	}
	return client.RunScopeOptions{}, fmt.Errorf("run %s was not found in any accessible project environment", runID)
}

func parsePayload(file string, raw string, pairs []string) (json.RawMessage, error) {
	file = strings.TrimSpace(file)
	raw = strings.TrimSpace(raw)
	if file != "" && (raw != "" || len(pairs) > 0) {
		return nil, errors.New("--payload-file cannot be combined with --payload-json or --payload")
	}
	if raw != "" && len(pairs) > 0 {
		return nil, errors.New("--payload-json cannot be combined with --payload")
	}
	if file != "" {
		contents, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read --payload-file: %w", err)
		}
		payload := json.RawMessage(contents)
		if !json.Valid(payload) {
			return nil, errors.New("--payload-file must contain valid JSON")
		}
		return payload, nil
	}
	if raw != "" {
		payload := json.RawMessage(raw)
		if !json.Valid(payload) {
			return nil, errors.New("--payload-json must be valid JSON")
		}
		return payload, nil
	}
	if len(pairs) == 0 {
		return json.RawMessage(`{}`), nil
	}
	object := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, err := splitKeyValue(pair, "payload")
		if err != nil {
			return nil, err
		}
		object[key] = value
	}
	payload, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	return payload, nil
}

func parseOptionalPayload(file string, raw string, pairs []string) (json.RawMessage, error) {
	if strings.TrimSpace(file) == "" && strings.TrimSpace(raw) == "" && len(pairs) == 0 {
		return nil, nil
	}
	return parsePayload(file, raw, pairs)
}

func parseOptionalJSON(file string, raw string, label string) (json.RawMessage, error) {
	file = strings.TrimSpace(file)
	raw = strings.TrimSpace(raw)
	if file != "" && raw != "" {
		return nil, fmt.Errorf("%s-file cannot be combined with %s-json", label, label)
	}
	if file != "" {
		contents, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %s-file: %w", label, err)
		}
		value := json.RawMessage(contents)
		if !json.Valid(value) {
			return nil, fmt.Errorf("%s-file must contain valid JSON", label)
		}
		return value, nil
	}
	if raw == "" {
		return nil, nil
	}
	value := json.RawMessage(raw)
	if !json.Valid(value) {
		return nil, fmt.Errorf("%s-json must be valid JSON", label)
	}
	return value, nil
}

func cleanTags(tags []string) []string {
	cleaned := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag != "" {
			cleaned = append(cleaned, tag)
		}
	}
	return cleaned
}

func optionalTags(tags []string) []string {
	if tags == nil {
		return nil
	}
	return cleanTags(tags)
}

func followRunEvents(cmd *cobra.Command, control *client.Client, runID string, cursor int64, scope client.RunScopeOptions) error {
	for {
		terminal := false
		err := control.FollowRunEvents(cmd.Context(), runID, cursor, func(event api.RunEvent) error {
			if parsed, parseErr := strconv.ParseInt(event.ID, 10, 64); parseErr == nil && parsed > cursor {
				cursor = parsed
			}
			if api.RunEventKindIsTerminal(event.Kind) {
				terminal = true
			}
			return format.JSONLines(cmd.OutOrStdout(), []api.RunEvent{event})
		}, scope)
		if errors.Is(err, context.Canceled) || errors.Is(cmd.Context().Err(), context.Canceled) {
			return nil
		}
		if err != nil && runEventStreamErrorIsFatal(err) {
			return err
		}
		if terminal {
			return nil
		}
		timer := time.NewTimer(runEventReconnectDelay)
		select {
		case <-cmd.Context().Done():
			timer.Stop()
			if errors.Is(cmd.Context().Err(), context.Canceled) {
				return nil
			}
			return cmd.Context().Err()
		case <-timer.C:
		}
	}
}

func followRunLogs(cmd *cobra.Command, control *client.Client, runID string, cursor int64, scope client.RunScopeOptions) error {
	handleChunk := func(chunk api.RunLogChunk) error {
		parsedCursor, parseErr := strconv.ParseInt(chunk.ID, 10, 64)
		if parseErr != nil {
			return fmt.Errorf("parse log chunk cursor: %w", parseErr)
		}
		content, err := base64.StdEncoding.DecodeString(chunk.ContentBase64)
		if err != nil {
			return fmt.Errorf("decode log chunk: %w", err)
		}
		switch chunk.Stream {
		case string(api.WorkerLogStreamStdout):
			_, err = cmd.OutOrStdout().Write(content)
		case string(api.WorkerLogStreamStderr):
			_, err = cmd.ErrOrStderr().Write(content)
		default:
			err = fmt.Errorf("unknown log stream %q", chunk.Stream)
		}
		if err != nil {
			return err
		}
		if parsedCursor > cursor {
			cursor = parsedCursor
		}
		return nil
	}
	for {
		err := control.FollowRunLogs(cmd.Context(), runID, cursor, handleChunk, scope)
		if errors.Is(err, context.Canceled) || errors.Is(cmd.Context().Err(), context.Canceled) {
			return nil
		}
		if err != nil && runEventStreamErrorIsFatal(err) {
			return err
		}
		run, snapshotErr := control.GetRun(cmd.Context(), runID, scope)
		if snapshotErr == nil && api.RunStatusIsTerminal(run.Status) {
			drainErr := control.FollowRunLogs(cmd.Context(), runID, cursor, handleChunk, scope)
			if drainErr != nil && runEventStreamErrorIsFatal(drainErr) {
				return drainErr
			}
			return nil
		}
		if snapshotErr != nil && runEventStreamErrorIsFatal(snapshotErr) {
			return snapshotErr
		}
		timer := time.NewTimer(runEventReconnectDelay)
		select {
		case <-cmd.Context().Done():
			timer.Stop()
			if errors.Is(cmd.Context().Err(), context.Canceled) {
				return nil
			}
			return cmd.Context().Err()
		case <-timer.C:
		}
	}
}

func waitForRun(ctx context.Context, control *client.Client, runID string, scope client.RunScopeOptions) (api.RunResponse, error) {
	run, err := control.GetRun(ctx, runID, scope)
	if err != nil {
		return api.RunResponse{}, err
	}
	if api.RunStatusIsTerminal(run.Status) {
		return run, nil
	}
	var cursor int64
	for {
		streamCtx, cancel := context.WithCancel(ctx)
		terminal := false
		err := control.FollowRunEvents(streamCtx, runID, cursor, func(event api.RunEvent) error {
			if parsed, parseErr := strconv.ParseInt(event.ID, 10, 64); parseErr == nil && parsed > cursor {
				cursor = parsed
			}
			if api.RunEventKindIsTerminal(event.Kind) {
				terminal = true
				cancel()
			}
			return nil
		}, scope)
		cancel()
		if ctx.Err() != nil {
			return api.RunResponse{}, ctx.Err()
		}
		if terminal {
			return waitForTerminalRunSnapshot(ctx, control, runID, scope)
		}
		if err != nil && !errors.Is(err, context.Canceled) && runEventStreamErrorIsFatal(err) {
			return api.RunResponse{}, err
		}
		run, err = control.GetRun(ctx, runID, scope)
		if err != nil {
			return api.RunResponse{}, err
		}
		if api.RunStatusIsTerminal(run.Status) {
			return run, nil
		}
		timer := time.NewTimer(runEventReconnectDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return api.RunResponse{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func waitForTerminalRunSnapshot(ctx context.Context, control *client.Client, runID string, scope client.RunScopeOptions) (api.RunResponse, error) {
	convergeCtx, cancel := context.WithTimeout(ctx, runTerminalSnapshotConvergeLimit)
	defer cancel()
	var lastErr error
	for {
		run, err := control.GetRun(convergeCtx, runID, scope)
		if err != nil {
			lastErr = err
		} else if api.RunStatusIsTerminal(run.Status) {
			return run, nil
		}
		timer := time.NewTimer(runTerminalSnapshotRetryDelay)
		select {
		case <-convergeCtx.Done():
			timer.Stop()
			if lastErr != nil {
				return api.RunResponse{}, fmt.Errorf("run %s reached a terminal event but the terminal snapshot did not converge: %w: last error: %v", runID, convergeCtx.Err(), lastErr)
			}
			return api.RunResponse{}, fmt.Errorf("run %s reached a terminal event but the terminal snapshot did not converge: %w", runID, convergeCtx.Err())
		case <-timer.C:
		}
	}
}

func runEventStreamErrorIsFatal(err error) bool {
	var httpErr *client.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode >= 400 && httpErr.StatusCode < 500
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return true
	}
	var typeErr *json.UnmarshalTypeError
	return errors.As(err, &typeErr) || errors.Is(err, bufio.ErrTooLong)
}

func splitKeyValue(raw string, label string) (string, string, error) {
	key, value, ok := strings.Cut(raw, "=")
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)
	if !ok || key == "" || value == "" {
		return "", "", fmt.Errorf("%s must be KEY=VALUE", label)
	}
	return key, value, nil
}

func validateProjectFlag(project string) error {
	if strings.Contains(project, "=") {
		return errors.New("--project must be a project slug or ID; use --payload KEY=VALUE for payload fields")
	}
	return nil
}
