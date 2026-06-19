package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

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
	Params      json.RawMessage `json:"params,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	Tags        []string        `json:"tags,omitempty"`
	CreatedAt   string          `json:"created_at"`
}

func waitpointCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "waitpoint",
		Short: "List waitpoints and manage waitpoint tokens.",
	}
	cmd.AddCommand(
		waitpointListCommand(),
		waitpointTokenCommand(),
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
		Short: "List open run waitpoints.",
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
	cmd.Flags().Int32Var(&limit, "limit", 100, "Maximum waitpoints to inspect.")
	return cmd
}

func waitpointTokenCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Create, inspect, and complete waitpoint tokens.",
	}
	cmd.AddCommand(
		waitpointTokenCreateCommand(),
		waitpointTokenListCommand(),
		waitpointTokenGetCommand(),
		waitpointTokenCompleteCommand(),
	)
	return cmd
}

func waitpointTokenCreateCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var timeoutSeconds int32
	var timeoutAt string
	var metadata string
	var tags []string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a waitpoint token.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			request := api.CreateWaitpointTokenRequest{
				Tags: tags,
			}
			if timeoutSeconds > 0 {
				request.TimeoutInSeconds = &timeoutSeconds
			}
			if strings.TrimSpace(timeoutAt) != "" {
				parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(timeoutAt))
				if err != nil {
					return fmt.Errorf("--timeout-at must be RFC3339: %w", err)
				}
				request.TimeoutAt = &parsed
			}
			metadataJSON, err := optionalJSONObject(metadata, "--metadata")
			if err != nil {
				return err
			}
			request.Metadata = metadataJSON
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			token, err := control.CreateWaitpointToken(cmd.Context(), request, waitpointTokenScope(projectID, environmentID))
			if err != nil {
				return err
			}
			return format.JSON(cmd.OutOrStdout(), token)
		},
	}
	cmd.Flags().StringVarP(&projectID, "project", "p", "", "Project slug or ID.")
	cmd.Flags().StringVarP(&environmentID, "env", "e", "", "Environment slug or ID.")
	cmd.Flags().Int32Var(&timeoutSeconds, "timeout-seconds", 0, "Token timeout in seconds.")
	cmd.Flags().StringVar(&timeoutAt, "timeout-at", "", "Token timeout timestamp in RFC3339.")
	cmd.Flags().StringVar(&metadata, "metadata", "", "Optional JSON metadata object.")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "Token tag. May be passed multiple times.")
	cmd.MarkFlagsMutuallyExclusive("timeout-seconds", "timeout-at")
	return cmd
}

func waitpointTokenListCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var status string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List waitpoint tokens.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			status = strings.TrimSpace(status)
			if status != "" && status != "waiting" && status != "completed" && status != "timed_out" && status != "cancelled" {
				return errors.New("--status must be one of waiting, completed, timed_out, or cancelled")
			}
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope := waitpointTokenScope(projectID, environmentID)
			scope.Status = status
			response, err := control.ListWaitpointTokens(cmd.Context(), scope)
			if err != nil {
				return err
			}
			return format.JSONLines(cmd.OutOrStdout(), response.Tokens)
		},
	}
	cmd.Flags().StringVarP(&projectID, "project", "p", "", "Project slug or ID.")
	cmd.Flags().StringVarP(&environmentID, "env", "e", "", "Environment slug or ID.")
	cmd.Flags().StringVar(&status, "status", "", "Filter by token status: waiting, completed, timed_out, or cancelled.")
	return cmd
}

func waitpointTokenGetCommand() *cobra.Command {
	var projectID string
	var environmentID string
	cmd := &cobra.Command{
		Use:   "get TOKEN_ID",
		Short: "Get a waitpoint token.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			response, err := control.GetWaitpointToken(cmd.Context(), args[0], waitpointTokenScope(projectID, environmentID))
			if err != nil {
				return err
			}
			return format.JSON(cmd.OutOrStdout(), response)
		},
	}
	cmd.Flags().StringVarP(&projectID, "project", "p", "", "Project slug or ID.")
	cmd.Flags().StringVarP(&environmentID, "env", "e", "", "Environment slug or ID.")
	return cmd
}

func waitpointTokenCompleteCommand() *cobra.Command {
	var data string
	var dataFile string
	cmd := &cobra.Command{
		Use:   "complete TOKEN_ID",
		Short: "Complete a waitpoint token.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			completionData, err := jsonPayload(cmd.InOrStdin(), data, dataFile, "--data")
			if err != nil {
				return err
			}
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			return control.CompleteWaitpointToken(cmd.Context(), args[0], api.CompleteWaitpointTokenRequest{
				Data: completionData,
			})
		},
	}
	cmd.Flags().StringVar(&data, "data", "", "JSON data to complete the token with.")
	cmd.Flags().StringVar(&dataFile, "data-file", "", "Read JSON data from a file, or '-' for stdin.")
	cmd.MarkFlagsMutuallyExclusive("data", "data-file")
	return cmd
}

func waitpointTokenScope(projectID string, environmentID string) client.WaitpointTokenOptions {
	return client.WaitpointTokenOptions{
		ProjectID:     strings.TrimSpace(projectID),
		EnvironmentID: strings.TrimSpace(environmentID),
	}
}

func jsonPayload(stdin io.Reader, raw string, file string, flag string) (json.RawMessage, error) {
	raw = strings.TrimSpace(raw)
	file = strings.TrimSpace(file)
	if raw != "" && file != "" {
		return nil, fmt.Errorf("%s cannot be combined with %s-file", flag, flag)
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
			return nil, fmt.Errorf("read %s-file: %w", flag, err)
		}
		raw = strings.TrimSpace(string(contents))
	}
	if raw == "" {
		return nil, nil
	}
	payload := json.RawMessage(raw)
	if !json.Valid(payload) {
		return nil, fmt.Errorf("%s must be valid JSON", flag)
	}
	return payload, nil
}

func optionalJSONObject(raw string, flag string) (json.RawMessage, error) {
	payload := json.RawMessage(strings.TrimSpace(raw))
	if len(payload) == 0 {
		return nil, nil
	}
	if !json.Valid(payload) {
		return nil, fmt.Errorf("%s must be valid JSON", flag)
	}
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("%s must be valid JSON: %w", flag, err)
	}
	if _, ok := decoded.(map[string]any); !ok {
		return nil, fmt.Errorf("%s must be a JSON object", flag)
	}
	return payload, nil
}

func openWaitpointEntries(runs []api.RunResponse) []waitpointListEntry {
	entries := make([]waitpointListEntry, 0, len(runs))
	for _, run := range runs {
		if run.PendingWaitpoint == nil {
			continue
		}
		request := run.PendingWaitpoint
		entries = append(entries, waitpointListEntry{
			WaitpointID: request.ID,
			RunID:       run.ID,
			TaskID:      run.TaskID,
			Kind:        request.Kind,
			Params:      request.Params,
			Metadata:    request.Metadata,
			Tags:        request.Tags,
			CreatedAt:   request.CreatedAt.Format(time.RFC3339),
		})
	}
	return entries
}

func waitpointTable(w io.Writer, entries []waitpointListEntry) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WAITPOINT ID\tRUN ID\tTASK\tKIND\tCREATED\tPARAMS")
	for _, entry := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			shortWaitpointID(entry.WaitpointID),
			shortWaitpointID(entry.RunID),
			entry.TaskID,
			entry.Kind,
			entry.CreatedAt,
			waitpointDataSummary(entry.Params),
		)
	}
	_ = tw.Flush()
}

func waitpointDataSummary(data json.RawMessage) string {
	if len(data) == 0 {
		return "{}"
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return truncateTableCell(singleLine(string(data)), 80)
	}
	return truncateTableCell(singleLine(compact.String()), 80)
}

func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func truncateTableCell(value string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}

func shortWaitpointID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
