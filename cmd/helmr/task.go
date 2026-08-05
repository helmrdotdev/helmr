package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

func taskCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "Work with deployed Tasks.",
	}
	cmd.AddCommand(taskListCommand(), taskGetCommand(), taskStartCommand())
	return cmd
}

func taskStartCommand() *cobra.Command {
	var payloadFile string
	var payloadJSON string
	var payloadPairs []string
	var projectID string
	var environmentID string
	var queueName string
	var concurrencyKey string
	var priority int32
	var ttl string
	var metadataFile string
	var metadataJSON string
	var tags []string
	var retryFile string
	var retryJSON string
	var workspaceID string
	var idempotencyKey string
	var wait bool
	var follow bool
	var timeout string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "start TASK",
		Short: "Start a Task.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			payload, err := parsePayload(payloadFile, payloadJSON, payloadPairs)
			if err != nil {
				return err
			}
			if err := api.ValidateDefinitionID(args[0]); err != nil {
				return err
			}
			metadata, err := parseOptionalJSON(metadataFile, metadataJSON, "--metadata")
			if err != nil {
				return err
			}
			retryJSONValue, err := parseOptionalJSON(retryFile, retryJSON, "--retry")
			if err != nil {
				return err
			}
			var retry *api.StartActorRetryPolicy
			if len(retryJSONValue) != 0 {
				retry = new(api.StartActorRetryPolicy)
				if err := json.Unmarshal(retryJSONValue, retry); err != nil {
					return fmt.Errorf("parse --retry: %w", err)
				}
			}
			if jsonOutput && follow {
				return errors.New("--json cannot be combined with --follow")
			}
			if workspaceID == "" {
				return errors.New("--workspace is required")
			}
			if err := api.ValidateWorkspaceID(workspaceID); err != nil {
				return err
			}
			timeoutSeconds, err := waitTimeoutSeconds(timeout, "--timeout")
			if err != nil {
				return err
			}
			if timeoutSeconds > 0 && !wait && !follow {
				return errors.New("--timeout requires --wait or --follow")
			}
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			if projectID != "" {
				if err := validateProjectFlag(projectID); err != nil {
					return err
				}
			}
			scope, err := environmentScopeForClient(controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			request := api.StartTaskRequest{
				Payload: payload, IdempotencyKey: strings.TrimSpace(idempotencyKey),
				Workspace: api.WorkspaceIDTarget{ID: workspaceID},
				Queue:     strings.TrimSpace(queueName), Priority: priority,
				TTL: strings.TrimSpace(ttl), Retry: retry,
				Metadata: metadata, Tags: cleanTags(tags),
			}
			if concurrencyKey = strings.TrimSpace(concurrencyKey); concurrencyKey != "" {
				request.ConcurrencyKey = &concurrencyKey
			}
			started, err := controlPlane.StartTask(cmd.Context(), args[0], request, scope)
			if err != nil {
				return err
			}
			var deadline time.Time
			if timeoutSeconds > 0 {
				deadline = time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
			}
			if jsonOutput {
				if wait {
					waitCtx := cmd.Context()
					if timeoutSeconds > 0 {
						var cancel func()
						waitCtx, cancel = context.WithDeadline(waitCtx, deadline)
						defer cancel()
					}
					run, err := waitForRun(
						waitCtx,
						controlPlane,
						started.RunID,
						client.RunScopeOptions(scope),
					)
					if err != nil {
						return err
					}
					return writeJSON(cmd.OutOrStdout(), run)
				}
				return writeJSON(cmd.OutOrStdout(), started)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "run_id: %s\n", started.RunID)
			if follow {
				if started.RunID == "" {
					return errors.New("task start response did not include a run id to follow")
				}
				followCtx := cmd.Context()
				if timeoutSeconds > 0 {
					var cancel func()
					followCtx, cancel = context.WithDeadline(followCtx, deadline)
					defer cancel()
				}
				if err := followRunLogs(
					followCtx,
					cmd,
					controlPlane,
					started.RunID,
					"",
					client.RunScopeOptions(scope),
				); err != nil {
					return err
				}
				wait = true
			}
			if wait {
				waitCtx := cmd.Context()
				if timeoutSeconds > 0 {
					var cancel func()
					waitCtx, cancel = context.WithDeadline(waitCtx, deadline)
					defer cancel()
				}
				run, err := waitForRun(
					waitCtx,
					controlPlane,
					started.RunID,
					client.RunScopeOptions(scope),
				)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "run_status: %s\n", run.Status)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&payloadFile, "payload-file", "", "Read payload JSON from a file.")
	cmd.Flags().StringVar(&payloadJSON, "payload-json", "", "Inline payload JSON literal.")
	cmd.Flags().StringArrayVar(&payloadPairs, "payload", nil, "Add a top-level string payload field as KEY=VALUE.")
	cmd.Flags().StringVarP(&projectID, "project", "p", "", "Project slug or ID.")
	cmd.Flags().StringVarP(&environmentID, "env", "e", "", "Environment slug or ID.")
	cmd.Flags().StringVar(&queueName, "queue", "", "Queue name for the Run.")
	cmd.Flags().StringVar(&concurrencyKey, "concurrency-key", "", "Concurrency key for the Run.")
	cmd.Flags().Int32Var(&priority, "priority", 0, "Run priority offset in seconds.")
	cmd.Flags().StringVar(&ttl, "ttl", "", "Queued Run time-to-live, for example 10m or 1h.")
	cmd.Flags().StringVar(&metadataFile, "metadata-file", "", "Read metadata JSON from a file.")
	cmd.Flags().StringVar(&metadataJSON, "metadata-json", "", "Inline metadata JSON literal.")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "Add a Run tag. Repeat for multiple tags.")
	cmd.Flags().StringVar(&retryFile, "retry-file", "", "Read retry policy JSON from a file.")
	cmd.Flags().StringVar(&retryJSON, "retry-json", "", "Inline retry policy JSON literal.")
	cmd.Flags().StringVar(&workspaceID, "workspace", "", "Existing Workspace ID (required).")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for this Task start.")
	cmd.Flags().BoolVar(&wait, "wait", false, "Wait for the Run to finish.")
	cmd.Flags().BoolVar(&follow, "follow", false, "Stream Run logs until the Run finishes.")
	cmd.Flags().StringVar(&timeout, "timeout", "", "Maximum wait duration, for example 10m or 1h.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.MarkFlagsMutuallyExclusive("metadata-file", "metadata-json")
	cmd.MarkFlagsMutuallyExclusive("retry-file", "retry-json")
	return cmd
}

func taskListCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Tasks in the current Deployment.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := controlPlane.ListTasks(cmd.Context(), scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), response)
			}
			for _, task := range response.Tasks {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", task.ID, task.DeploymentID)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func taskGetCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get TASK",
		Short: "Show Task details.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			task, err := controlPlane.GetTask(cmd.Context(), args[0], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), task)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Task:       %s\n", task.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "Deployment: %s\n", task.DeploymentID)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}
