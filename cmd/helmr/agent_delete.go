package main

import (
	"fmt"
	"github.com/spf13/cobra"
	"strconv"
)

func agentChangeCommand(restore bool) *cobra.Command {
	action := "delete"
	if restore {
		action = "restore"
	}
	var project, environment, revision string
	var dry, jsonOutput bool
	cmd := &cobra.Command{Use: action + " AGENT", Short: action + " an Agent using human management authority.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cp, err := sessionControlPlaneClient(cmd)
		if err != nil {
			return err
		}
		scope, err := environmentScopeForClient(cmd.Context(), cp, project, environment)
		if err != nil {
			return err
		}
		if !dry && revision == "" {
			status, err := cp.GetAgentStatus(cmd.Context(), args[0], scope)
			if err != nil {
				return err
			}
			revision = strconv.FormatInt(status.Revision, 10)
		}
		result, err := cp.ChangeAgent(cmd.Context(), args[0], scope, restore, dry, revision)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), result)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "agent: %s\nrevision: %d\ncan_apply: %t\nchanged: %t\n", result.Agent, result.Revision, result.CanApply, result.Changed)
		for _, b := range result.Blockers {
			fmt.Fprintf(cmd.OutOrStdout(), "blocked: %s\n", b.Code)
		}
		e := result.Effects
		fmt.Fprintf(cmd.OutOrStdout(), "sessions_cancelled: %d\nselected_agent_edges_removed: %d\nineffective_assignments_removed: %d\nmixed_assignments_preserved: %d\ntrigger_activations_sealed: %d\n", e.SessionsCancelled, e.SelectedAgentEdgesRemoved, e.IneffectiveAssignmentsRemoved, e.MixedAssignmentsPreserved, e.TriggerActivationsSealed)
		if e.SlackConnectionDisconnected != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "slack_connection_disconnected: %s\n", *e.SlackConnectionDisconnected)
		}
		for _, warning := range result.Warnings {
			fmt.Fprintf(cmd.OutOrStdout(), "warning: %s\n", warning)
		}
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().BoolVar(&dry, "dry-run", false, "Preview prerequisites and cleanup without changing state.")
	cmd.Flags().StringVar(&revision, "if-match", "", "Require this revision; omission reads the current revision.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON result.")
	return cmd
}
