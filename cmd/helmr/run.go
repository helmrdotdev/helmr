package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/spf13/cobra"
)

var runFollowPollInterval = time.Second

func runCancelCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "cancel RUN",
		Short: "Cancel a run.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := runScopeForClient(controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := controlPlane.CancelRun(cmd.Context(), args[0], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), response)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "run_id: %s\n", response.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "run_status: %s\n", response.Status)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func runListCommand() *cobra.Command {
	var jsonOutput bool
	var jsonLines bool
	var projectID string
	var environmentID string
	var statuses []string
	var cursor string
	var limit int32
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List runs.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			response, err := controlPlane.ListRuns(cmd.Context(), client.ListRunsOptions{
				Statuses:      statuses,
				Cursor:        cursor,
				Limit:         limit,
				ProjectID:     strings.TrimSpace(projectID),
				EnvironmentID: strings.TrimSpace(environmentID),
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), response)
			}
			if jsonLines {
				return writeJSONLines(cmd.OutOrStdout(), response.Runs)
			}
			writeRunTable(cmd.OutOrStdout(), response.Runs)
			if response.NextCursor != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "\nNext cursor: %s\n", response.NextCursor)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.Flags().BoolVar(&jsonLines, "jsonl", false, "Emit one JSON run per line.")
	cmd.Flags().StringSliceVar(&statuses, "status", nil, "Filter by Run status; repeat or comma-separate values.")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continue listing from this cursor.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum Runs to return.")
	addScopeFlags(cmd, &projectID, &environmentID)
	return cmd
}

func runGetCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get RUN",
		Short: "Show run details.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := runScopeForClient(controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			run, err := controlPlane.GetRun(cmd.Context(), args[0], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), run)
			}
			writeRunDetails(cmd.OutOrStdout(), run)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func runCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Work with Runs.",
	}
	cmd.AddCommand(runListCommand(), runGetCommand(), runLogsCommand(), runEventsCommand(), runWaitCommand(), runCancelCommand())
	return cmd
}

func runLogsCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs RUN",
		Short: "Print the latest run logs.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := runScopeForClient(controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			logs, err := controlPlane.ListRunLogs(
				cmd.Context(),
				args[0],
				client.ListRunLogsOptions{RunScopeOptions: scope},
			)
			if err != nil {
				return err
			}
			if err := writeRunLogPage(cmd, logs); err != nil {
				return err
			}
			if follow {
				return followRunLogs(
					cmd.Context(),
					cmd,
					controlPlane,
					args[0],
					strings.TrimSpace(logs.NextCursor),
					scope,
				)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&follow, "follow", false, "Continue streaming new logs.")
	return cmd
}

func writeRunLogPage(cmd *cobra.Command, page api.RunLogPage) error {
	for _, record := range page.Logs {
		switch record.Kind {
		case string(workerapi.LogStreamStdout), string(workerapi.LogStreamStderr):
			content, err := base64.StdEncoding.DecodeString(record.ContentBase64)
			if err != nil {
				return fmt.Errorf("decode %s log: %w", record.Kind, err)
			}
			if record.Kind == string(workerapi.LogStreamStdout) {
				_, err = cmd.OutOrStdout().Write(content)
			} else {
				_, err = cmd.ErrOrStderr().Write(content)
			}
			if err != nil {
				return err
			}
		case string(workerapi.LogStreamStructured):
			if err := writeJSONLines(cmd.OutOrStdout(), []api.RunLogRecord{record}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown log kind %q", record.Kind)
		}
	}
	return nil
}

func runEventsCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var cursor string
	var limit int32
	var follow bool
	cmd := &cobra.Command{
		Use:   "events RUN",
		Short: "List run events.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := runScopeForClient(controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			if !follow {
				page, err := controlPlane.ListRunEvents(cmd.Context(), args[0], client.ListRunEventsOptions{Cursor: cursor, Limit: limit, RunScopeOptions: scope})
				if err != nil {
					return err
				}
				return writeJSONLines(cmd.OutOrStdout(), page.Events)
			}
			return followRunEvents(cmd, controlPlane, args[0], cursor, scope)
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&cursor, "cursor", "", "Return events after this cursor.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum events to return.")
	cmd.Flags().BoolVar(&follow, "follow", false, "Continue streaming new events.")
	return cmd
}

func runWaitCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var timeout string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "wait RUN",
		Short: "Wait for a run to finish.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, err := controlPlaneClient(cmd)
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
			scope, err := runScopeForClient(controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			run, err := waitForRun(ctx, controlPlane, args[0], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), run)
			}
			writeRunLifecycleResult(cmd, run)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&timeout, "timeout", "", "Maximum wait duration, for example 10m or 1h.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func runScopeForClient(controlPlane *client.Client, projectID string, environmentID string) (client.RunScopeOptions, error) {
	scope := client.RunScopeOptions{
		ProjectID:     strings.TrimSpace(projectID),
		EnvironmentID: strings.TrimSpace(environmentID),
	}
	if !controlPlane.UsesSessionScopedRoutes() {
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

func environmentScopeForClient(controlPlane *client.Client, projectID string, environmentID string) (client.EnvironmentScopeOptions, error) {
	scope := client.EnvironmentScopeOptions{
		ProjectID:     strings.TrimSpace(projectID),
		EnvironmentID: strings.TrimSpace(environmentID),
	}
	if !controlPlane.UsesSessionScopedRoutes() {
		if scope.ProjectID != "" || scope.EnvironmentID != "" {
			return client.EnvironmentScopeOptions{}, errors.New("--project and --env require helmr login; API keys are already environment scoped")
		}
		return client.EnvironmentScopeOptions{}, nil
	}
	if scope.ProjectID == "" || scope.EnvironmentID == "" {
		return client.EnvironmentScopeOptions{}, errors.New("--project and --env are required with helmr login")
	}
	return scope, nil
}

func workspaceScopeForClient(controlPlane *client.Client, projectID string, environmentID string) (client.WorkspaceScopeOptions, error) {
	environmentScope, err := environmentScopeForClient(controlPlane, projectID, environmentID)
	return client.WorkspaceScopeOptions(environmentScope), err
}

func writeRunLifecycleResult(cmd *cobra.Command, run api.RunSnapshotResponse) {
	fmt.Fprintf(cmd.OutOrStdout(), "run_id: %s\n", run.ID)
	fmt.Fprintf(cmd.OutOrStdout(), "run_status: %s\n", run.Status)
}

func addScopeFlags(cmd *cobra.Command, projectID *string, environmentID *string) {
	cmd.Flags().StringVarP(projectID, "project", "p", "", "Project slug or ID.")
	cmd.Flags().StringVarP(environmentID, "env", "e", "", "Environment slug or ID.")
}

func waitTimeoutSeconds(raw string, label string) (int32, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	duration, err := api.ParsePositiveDuration(raw, label)
	if err != nil {
		return 0, err
	}
	seconds := (duration + time.Second - time.Nanosecond) / time.Second
	if seconds > 1<<31-1 {
		return 0, fmt.Errorf("%s is too large", label)
	}
	return int32(seconds), nil
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
		return nil, nil
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

func followRunEvents(cmd *cobra.Command, controlPlane *client.Client, runID string, cursor string, scope client.RunScopeOptions) error {
	for {
		terminal := false
		page, err := controlPlane.ListRunEvents(
			cmd.Context(),
			runID,
			client.ListRunEventsOptions{
				Cursor: cursor, RunScopeOptions: scope,
			},
		)
		if err == nil {
			for _, event := range page.Events {
				if api.RunEventKindIsTerminal(event.Kind) {
					terminal = true
				}
				if writeErr := writeJSONLines(
					cmd.OutOrStdout(),
					[]api.RunEvent{event},
				); writeErr != nil {
					return writeErr
				}
			}
			if page.NextCursor != nil {
				cursor = *page.NextCursor
			}
		}
		if errors.Is(err, context.Canceled) || errors.Is(cmd.Context().Err(), context.Canceled) {
			return nil
		}
		if err != nil && runReadErrorIsFatal(err) {
			return err
		}
		if terminal {
			return nil
		}
		timer := time.NewTimer(runFollowPollInterval)
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

func followRunLogs(ctx context.Context, cmd *cobra.Command, controlPlane *client.Client, runID string, cursor string, scope client.RunScopeOptions) error {
	for {
		page, err := controlPlane.ListRunLogs(
			ctx,
			runID,
			client.ListRunLogsOptions{
				Cursor: cursor, RunScopeOptions: scope,
			},
		)
		if err == nil {
			if err := writeRunLogPage(cmd, page); err != nil {
				return err
			}
			if page.NextCursor != "" {
				cursor = page.NextCursor
			}
		}
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return nil
		}
		if err != nil && runReadErrorIsFatal(err) {
			return err
		}
		run, snapshotErr := controlPlane.GetRun(ctx, runID, scope)
		if snapshotErr == nil && api.RunStatusIsTerminal(run.Status) {
			drain, drainErr := controlPlane.ListRunLogs(
				ctx,
				runID,
				client.ListRunLogsOptions{
					Cursor: cursor, RunScopeOptions: scope,
				},
			)
			if drainErr == nil {
				return writeRunLogPage(cmd, drain)
			}
			if runReadErrorIsFatal(drainErr) {
				return drainErr
			}
			return nil
		}
		if snapshotErr != nil && runReadErrorIsFatal(snapshotErr) {
			return snapshotErr
		}
		timer := time.NewTimer(runFollowPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func waitForRun(ctx context.Context, controlPlane *client.Client, runID string, scope client.RunScopeOptions) (api.RunSnapshotResponse, error) {
	run, err := controlPlane.GetRun(ctx, runID, scope)
	if err != nil {
		return api.RunSnapshotResponse{}, err
	}
	if api.RunStatusIsTerminal(run.Status) {
		return run, nil
	}
	for {
		run, err = controlPlane.GetRun(ctx, runID, scope)
		if err != nil {
			return api.RunSnapshotResponse{}, err
		}
		if api.RunStatusIsTerminal(run.Status) {
			return run, nil
		}
		timer := time.NewTimer(runFollowPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return api.RunSnapshotResponse{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func runReadErrorIsFatal(err error) bool {
	var httpErr *httpclient.Error
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
