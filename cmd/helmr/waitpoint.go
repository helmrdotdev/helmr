package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cli/format"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

type waitpointListEntry struct {
	WaitpointID string          `json:"waitpoint_id"`
	RunID       string          `json:"run_id"`
	TaskID      string          `json:"task_id"`
	Kind        string          `json:"kind"`
	DisplayText string          `json:"display_text,omitempty"`
	Request     json.RawMessage `json:"request,omitempty"`
	RequestedAt string          `json:"requested_at"`
	Policy      *string         `json:"policy,omitempty"`
}

func waitpointCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "waitpoint",
		Short: "List and respond to waitpoints.",
	}
	cmd.AddCommand(
		waitpointListCommand(),
		waitpointRespondCommand(),
	)
	return cmd
}

func waitpointListCommand() *cobra.Command {
	var jsonOutput bool
	var projectID string
	var environmentID string
	var limit int32
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List open waitpoints.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 1 || limit > 200 {
				return errors.New("--limit must be an integer between 1 and 200")
			}
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			response, err := control.ListRuns(cmd.Context(), client.ListRunsOptions{
				Status:        "waiting",
				Limit:         limit,
				ProjectID:     strings.TrimSpace(projectID),
				EnvironmentID: strings.TrimSpace(environmentID),
			})
			if err != nil {
				return err
			}
			entries := openWaitpointEntries(response.Runs)
			if jsonOutput {
				return format.JSONLines(cmd.OutOrStdout(), entries)
			}
			waitpointTable(cmd.OutOrStdout(), entries)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON waitpoint per line.")
	cmd.Flags().StringVarP(&projectID, "project", "p", "", "Project slug or ID to list.")
	cmd.Flags().StringVarP(&environmentID, "env", "e", "", "Environment slug or ID to list.")
	cmd.Flags().Int32Var(&limit, "limit", 100, "Maximum waiting runs to inspect.")
	return cmd
}

func waitpointRespondCommand() *cobra.Command {
	var value string
	var valueFile string
	var projectID string
	var environmentID string
	cmd := &cobra.Command{
		Use:   "respond WAITPOINT_ID",
		Short: "Respond to a human waitpoint.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedValue, err := waitpointResponseValue(cmd.InOrStdin(), value, valueFile)
			if err != nil {
				return err
			}
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := waitpointCommandScope(cmd.Context(), control, projectID, environmentID)
			if err != nil {
				return err
			}
			request := api.RespondWaitpointRequest{Value: resolvedValue}
			return control.RespondWaitpoint(cmd.Context(), args[0], request, scope)
		},
	}
	cmd.Flags().StringVar(&value, "value", "", "JSON value to return to the waiting run.")
	cmd.Flags().StringVar(&valueFile, "value-file", "", "Read response JSON value from a file, or '-' for stdin.")
	cmd.Flags().StringVarP(&projectID, "project", "p", "", "Project slug or ID.")
	cmd.Flags().StringVarP(&environmentID, "env", "e", "", "Environment slug or ID.")
	cmd.MarkFlagsMutuallyExclusive("value", "value-file")
	return cmd
}

func waitpointCommandScope(ctx context.Context, control *client.Client, projectID string, environmentID string) (client.RunScopeOptions, error) {
	scope, err := runScopeForClient(control, projectID, environmentID)
	if err != nil {
		return client.RunScopeOptions{}, err
	}
	if !control.UsesSessionScopedRoutes() {
		return scope, nil
	}
	project, environment, err := resolveProjectEnvironment(ctx, control, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		return client.RunScopeOptions{}, err
	}
	return client.RunScopeOptions{ProjectID: project.ID, EnvironmentID: environment.ID}, nil
}

func waitpointResponseValue(stdin io.Reader, raw string, file string) (json.RawMessage, error) {
	raw = strings.TrimSpace(raw)
	file = strings.TrimSpace(file)
	if raw != "" && file != "" {
		return nil, errors.New("--value cannot be combined with --value-file")
	}
	if file != "" {
		var contents []byte
		var err error
		if file == "-" {
			contents, err = io.ReadAll(stdin)
		} else {
			contents, err = os.ReadFile(file)
		}
		if err != nil {
			return nil, fmt.Errorf("read --value-file: %w", err)
		}
		value := json.RawMessage(strings.TrimSpace(string(contents)))
		if !json.Valid(value) {
			return nil, errors.New("--value-file must contain valid JSON")
		}
		return value, nil
	}
	if raw == "" {
		return nil, nil
	}
	value := json.RawMessage(raw)
	if !json.Valid(value) {
		return nil, errors.New("--value must be valid JSON")
	}
	return value, nil
}

func openWaitpointEntries(runs []api.RunResponse) []waitpointListEntry {
	entries := make([]waitpointListEntry, 0, len(runs))
	for _, run := range runs {
		if run.PendingWaitpoint == nil {
			continue
		}
		waitpoint := run.PendingWaitpoint
		entries = append(entries, waitpointListEntry{
			WaitpointID: waitpoint.WaitpointID,
			RunID:       run.ID,
			TaskID:      run.TaskID,
			Kind:        waitpoint.Kind,
			DisplayText: waitpoint.DisplayText,
			Request:     waitpoint.Request,
			RequestedAt: waitpoint.RequestedAt.Format("2006-01-02T15:04:05Z07:00"),
			Policy:      waitpoint.Policy,
		})
	}
	return entries
}

func waitpointTable(w io.Writer, entries []waitpointListEntry) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WAITPOINT ID\tRUN ID\tTASK\tKIND\tREQUESTED\tREQUEST")
	for _, entry := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			shortWaitpointID(entry.WaitpointID),
			shortWaitpointID(entry.RunID),
			entry.TaskID,
			entry.Kind,
			entry.RequestedAt,
			entry.DisplayText,
		)
	}
	_ = tw.Flush()
}

func shortWaitpointID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
