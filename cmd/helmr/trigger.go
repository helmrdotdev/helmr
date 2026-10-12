package main

import (
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

func triggerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trigger",
		Short: "Inspect Triggers.",
	}
	cmd.AddCommand(triggerListCommand(), triggerGetCommand())
	return cmd
}

func triggerListCommand() *cobra.Command {
	var agent string
	var projectID string
	var environmentID string
	var cursor string
	var limit int32
	var jsonOutput bool
	var jsonLines bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Triggers.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("limit") && limit < 1 {
				return errors.New("--limit must be in [1,100]")
			}
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(cmd.Context(), controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := controlPlane.ListTriggers(cmd.Context(), client.ListTriggersOptions{
				Cursor: cursor, Limit: limit, Agent: agent, EnvironmentScopeOptions: scope,
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), response)
			}
			if jsonLines {
				return writeJSONLines(cmd.OutOrStdout(), response.Triggers)
			}
			for _, trigger := range response.Triggers {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", trigger.ID, trigger.Agent, trigger.TriggerKey)
			}
			if response.NextCursor != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", response.NextCursor)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continue a prior Trigger page.")
	cmd.Flags().StringVar(&agent, "agent", "", "Filter by the Agent identifier declared in code.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum Triggers (default 50, maximum 100).")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.Flags().BoolVar(&jsonLines, "jsonl", false, "Emit one JSON Trigger per line.")
	cmd.MarkFlagsMutuallyExclusive("json", "jsonl")
	return cmd
}

func triggerGetCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get SCHEDULE",
		Short: "Show Trigger status.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(cmd.Context(), controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			trigger, err := controlPlane.GetTrigger(cmd.Context(), args[0], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), trigger)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "trigger_id: %s\n", trigger.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "agent: %s\n", trigger.Agent)
			fmt.Fprintf(cmd.OutOrStdout(), "trigger: %s\n", trigger.TriggerKey)
			if trigger.NextFireAt != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "next_fire_at: %s\n", trigger.NextFireAt.UTC().Format("2006-01-02T15:04:05.999999999Z"))
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}
