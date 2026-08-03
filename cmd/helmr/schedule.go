package main

import (
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

func scheduleCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schedule",
		Short: "Inspect Schedules.",
	}
	cmd.AddCommand(scheduleListCommand(), scheduleGetCommand())
	return cmd
}

func scheduleListCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var cursor string
	var limit int32
	var jsonOutput bool
	var jsonLines bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Schedules.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("limit") && limit < 1 {
				return errors.New("--limit must be in [1,100]")
			}
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := controlPlane.ListSchedules(cmd.Context(), client.ListSchedulesOptions{
				Cursor: cursor, Limit: limit, EnvironmentScopeOptions: scope,
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), response)
			}
			if jsonLines {
				return writeJSONLines(cmd.OutOrStdout(), response.Schedules)
			}
			for _, schedule := range response.Schedules {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", schedule.ID, schedule.Task, schedule.Status)
			}
			if response.NextCursor != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", response.NextCursor)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continue a prior Schedule page.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum Schedules (default 50, maximum 100).")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.Flags().BoolVar(&jsonLines, "jsonl", false, "Emit one JSON Schedule per line.")
	cmd.MarkFlagsMutuallyExclusive("json", "jsonl")
	return cmd
}

func scheduleGetCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get SCHEDULE",
		Short: "Show Schedule status.",
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
			schedule, err := controlPlane.GetSchedule(cmd.Context(), args[0], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), schedule)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "schedule_id: %s\n", schedule.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "task: %s\n", schedule.Task)
			fmt.Fprintf(cmd.OutOrStdout(), "status: %s\n", schedule.Status)
			if schedule.NextFireAt != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "next_fire_at: %s\n", schedule.NextFireAt.UTC().Format("2006-01-02T15:04:05.999999999Z"))
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}
