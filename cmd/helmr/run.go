package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cli/format"
	"github.com/helmrdotdev/helmr/internal/cli/ui"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/spf13/cobra"
)

func runCommand() *cobra.Command {
	var repository string
	var ref string
	var subpath string
	var payloadFile string
	var payloadJSON string
	var payloadPairs []string
	var secretPairs []string
	var projectID string
	var environmentID string
	var deploymentID string
	var version string
	var maxDurationSeconds int32
	cmd := &cobra.Command{
		Use:   "run TASK --repo OWNER/REPO --ref REF",
		Short: "Create a remote GitHub-backed run.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			payload, err := parsePayload(payloadFile, payloadJSON, payloadPairs)
			if err != nil {
				return err
			}
			secrets, err := parseSecrets(secretPairs)
			if err != nil {
				return err
			}
			repository = strings.TrimSpace(repository)
			if repository == "" {
				return errors.New("--repo is required")
			}
			ref = strings.TrimSpace(ref)
			if ref == "" {
				return errors.New("--ref is required")
			}
			if err := api.ValidateTaskID(args[0]); err != nil {
				return err
			}
			workspace := api.RunWorkspace{Repository: repository, Ref: ref}
			workspace.Subpath = strings.TrimSpace(subpath)
			control, err := controlClient()
			if err != nil {
				return err
			}
			run, err := control.CreateRun(cmd.Context(), api.CreateRunRequest{
				ProjectID:          strings.TrimSpace(projectID),
				EnvironmentID:      strings.TrimSpace(environmentID),
				TaskID:             args[0],
				DeploymentID:       strings.TrimSpace(deploymentID),
				Version:            strings.TrimSpace(version),
				Payload:            payload,
				Secrets:            secrets,
				Workspace:          workspace,
				MaxDurationSeconds: maxDurationSeconds,
			})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), run.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&repository, "repo", "", "Workspace GitHub repository to check out as OWNER/REPO.")
	cmd.Flags().StringVar(&ref, "ref", "", "Workspace GitHub branch, tag, or commit SHA to check out.")
	cmd.Flags().StringVar(&subpath, "subpath", "", "Workspace repository subdirectory to mount for the run.")
	cmd.Flags().StringVar(&payloadFile, "payload-file", "", "Read payload JSON from a file.")
	cmd.Flags().StringVar(&payloadJSON, "payload-json", "", "Inline payload JSON literal.")
	cmd.Flags().StringArrayVarP(&payloadPairs, "payload", "p", nil, "Add a top-level string payload field as KEY=VALUE.")
	cmd.Flags().StringArrayVar(&secretPairs, "secret", nil, "Bind a declared task secret as NAME=vault:SECRET_NAME.")
	cmd.Flags().StringVar(&projectID, "project", "", "Project ID for this run.")
	cmd.Flags().StringVar(&environmentID, "environment", "", "Environment ID for this run.")
	cmd.Flags().StringVar(&deploymentID, "deployment", "", "Deployment ID to pin for this run.")
	cmd.Flags().StringVar(&version, "version", "", "Deployment version to pin for this run.")
	cmd.Flags().Int32Var(&maxDurationSeconds, "max-duration-seconds", 0, "Maximum run duration in seconds.")
	cmd.MarkFlagsMutuallyExclusive("deployment", "version")
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
			control, err := controlClient()
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
	cmd.Flags().StringVar(&projectID, "project", "", "Project ID to list.")
	cmd.Flags().StringVar(&environmentID, "environment", "", "Environment ID to list.")
	return cmd
}

func showCommand() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "show RUN",
		Short: "Show run details.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient()
			if err != nil {
				return err
			}
			run, err := control.GetRun(cmd.Context(), args[0])
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
	cmd := &cobra.Command{
		Use:   "logs RUN",
		Short: "Print the latest run logs.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient()
			if err != nil {
				return err
			}
			logs, err := control.GetRunLogs(cmd.Context(), args[0])
			if err != nil {
				return err
			}
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
		},
	}
	return cmd
}

func eventsCommand() *cobra.Command {
	var cursor int64
	var limit int32
	cmd := &cobra.Command{
		Use:   "events RUN",
		Short: "List run events.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient()
			if err != nil {
				return err
			}
			page, err := control.ListRunEvents(cmd.Context(), args[0], client.ListRunEventsOptions{Cursor: cursor, Limit: limit})
			if err != nil {
				return err
			}
			return format.JSONLines(cmd.OutOrStdout(), page.Events)
		},
	}
	cmd.Flags().Int64Var(&cursor, "cursor", 0, "Return events after this cursor.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum events to return.")
	return cmd
}

func parsePayload(file string, raw string, pairs []string) (json.RawMessage, error) {
	file = strings.TrimSpace(file)
	raw = strings.TrimSpace(raw)
	if file != "" && (raw != "" || len(pairs) > 0) {
		return nil, errors.New("--payload-file cannot be combined with --payload-json or -p/--payload")
	}
	if raw != "" && len(pairs) > 0 {
		return nil, errors.New("--payload-json cannot be combined with -p/--payload")
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

func parseSecrets(pairs []string) (api.SecretBindings, error) {
	if len(pairs) == 0 {
		return api.SecretBindings{}, nil
	}
	bindings := make(api.SecretBindings, len(pairs))
	for _, pair := range pairs {
		name, stored, err := splitKeyValue(pair, "secret")
		if err != nil {
			return nil, err
		}
		bindings[name] = stored
	}
	if err := secret.ValidateBindings(bindings); err != nil {
		return nil, err
	}
	return bindings, nil
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
