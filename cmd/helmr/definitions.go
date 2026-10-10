package main

import (
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

func computerDefinitionCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "definition", Short: "Inspect deployed Computer definitions."}
	cmd.AddCommand(computerDefinitionListCommand(), computerDefinitionGetCommand())
	return cmd
}

func agentListCommand() *cobra.Command {
	var project, environment, deployment, cursor string
	var limit int32
	var jsonOutput bool
	cmd := &cobra.Command{Use: "list", Short: "Read one page of deployed Agents.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("limit") && limit < 1 {
			return errors.New("--limit must be in [1,100]")
		}
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		page, err := cp.ListAgents(cmd.Context(), client.DefinitionListOptions{DeploymentID: deployment, Cursor: cursor, Limit: limit, EnvironmentScopeOptions: scope})
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), page)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "deployment_id: %s\n", page.DeploymentID)
		for _, item := range page.Agents {
			fmt.Fprintln(cmd.OutOrStdout(), item.ID)
		}
		if page.NextCursor != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", page.NextCursor)
		}
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&deployment, "deployment", "", "Inspect this Deployment; omission uses the current Deployment.")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continuation cursor from the previous page.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum definitions (default 50, maximum 100).")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON page.")
	return cmd
}

func agentGetCommand() *cobra.Command {
	var project, environment, deployment string
	var jsonOutput bool
	cmd := &cobra.Command{Use: "get DEFINITION_ID", Short: "Inspect one deployed Agent.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		item, err := cp.GetAgent(cmd.Context(), args[0], client.DefinitionGetOptions{DeploymentID: deployment, EnvironmentScopeOptions: scope})
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), item)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "id: %s\ndeployment_id: %s\n", item.ID, item.DeploymentID)
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&deployment, "deployment", "", "Inspect this Deployment; omission uses the current Deployment.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON definition.")
	return cmd
}

func computerDefinitionListCommand() *cobra.Command {
	var project, environment, deployment, cursor string
	var limit int32
	var jsonOutput bool
	cmd := &cobra.Command{Use: "list", Short: "Read one page of deployed Computer definitions.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("limit") && limit < 1 {
			return errors.New("--limit must be in [1,100]")
		}
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		page, err := cp.ListComputerDefinitions(cmd.Context(), client.DefinitionListOptions{DeploymentID: deployment, Cursor: cursor, Limit: limit, EnvironmentScopeOptions: scope})
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), page)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "deployment_id: %s\n", page.DeploymentID)
		for _, item := range page.ComputerDefinitions {
			fmt.Fprintln(cmd.OutOrStdout(), item.ID)
		}
		if page.NextCursor != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", page.NextCursor)
		}
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&deployment, "deployment", "", "Inspect this Deployment; omission uses the current Deployment.")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continuation cursor from the previous page.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum definitions (default 50, maximum 100).")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON page.")
	return cmd
}

func computerDefinitionGetCommand() *cobra.Command {
	var project, environment, deployment string
	var jsonOutput bool
	cmd := &cobra.Command{Use: "get DEFINITION_ID", Short: "Inspect one deployed Computer definition.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		item, err := cp.GetComputerDefinition(cmd.Context(), args[0], client.DefinitionGetOptions{DeploymentID: deployment, EnvironmentScopeOptions: scope})
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), item)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "id: %s\ndeployment_id: %s\n", item.ID, item.DeploymentID)
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&deployment, "deployment", "", "Inspect this Deployment; omission uses the current Deployment.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON definition.")
	return cmd
}
