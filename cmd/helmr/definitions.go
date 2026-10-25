package main

import (
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

func templateCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "template", Short: "Inspect and refresh deployed Templates."}
	cmd.AddCommand(templateDefinitionListCommand(), templateDefinitionGetCommand(), templateRefreshCommand())
	return cmd
}

func agentDefinitionListCommand() *cobra.Command {
	var project, environment, deployment, cursor string
	var limit int32
	var jsonOutput, all, deleted bool
	cmd := &cobra.Command{Use: "list", Short: "Read one page of deployed Agents.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("limit") && limit < 1 {
			return errors.New("--limit must be in [1,100]")
		}
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		if all || deleted {
			view := "all"
			if deleted {
				view = "deleted"
			}
			page, err := cp.ListAgentCatalog(cmd.Context(), scope, cursor, view, limit)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), page)
			}
			for _, item := range page.Agents {
				status := "not in current deployment"
				if item.DefinedInCurrent {
					status = "current"
				}
				if item.DeletedAt != nil {
					status = "deleted"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", item.Agent, status)
			}
			if page.NextCursor != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", page.NextCursor)
			}
			return nil
		}
		page, err := cp.ListAgentDefinitions(cmd.Context(), client.DefinitionListOptions{DeploymentID: deployment, Cursor: cursor, Limit: limit, EnvironmentScopeOptions: scope})
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), page)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "deployment_id: %s\n", page.DeploymentID)
		for _, item := range page.AgentDefinitions {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", item.ID, item.TemplateID)
		}
		if page.NextCursor != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", page.NextCursor)
		}
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&deployment, "deployment", "", "Inspect this Deployment; omission uses the current Deployment.")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continuation cursor from the previous page.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum items (default 50, maximum 100).")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON page.")
	cmd.Flags().BoolVar(&all, "all", false, "Include retained Agent identities.")
	cmd.Flags().BoolVar(&deleted, "deleted", false, "List deleted Agent identities.")
	cmd.MarkFlagsMutuallyExclusive("all", "deleted", "deployment")
	return cmd
}

func agentDefinitionGetCommand() *cobra.Command {
	var project, environment, deployment string
	var jsonOutput bool
	cmd := &cobra.Command{Use: "get AGENT", Short: "Inspect one deployed Agent.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		item, err := cp.GetAgentDefinition(cmd.Context(), args[0], client.DefinitionGetOptions{DeploymentID: deployment, EnvironmentScopeOptions: scope})
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), item)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "agent: %s\ndeployment_id: %s\n", item.ID, item.DeploymentID)
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&deployment, "deployment", "", "Inspect this Deployment; omission uses the current Deployment.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON resource.")
	return cmd
}

func templateDefinitionListCommand() *cobra.Command {
	var project, environment, deployment, cursor string
	var limit int32
	var jsonOutput bool
	cmd := &cobra.Command{Use: "list", Short: "Read one page of deployed Templates.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("limit") && limit < 1 {
			return errors.New("--limit must be in [1,100]")
		}
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		page, err := cp.ListTemplateDefinitions(cmd.Context(), client.DefinitionListOptions{DeploymentID: deployment, Cursor: cursor, Limit: limit, EnvironmentScopeOptions: scope})
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), page)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "deployment_id: %s\n", page.DeploymentID)
		for _, item := range page.TemplateDefinitions {
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
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum items (default 50, maximum 100).")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON page.")
	return cmd
}

func templateDefinitionGetCommand() *cobra.Command {
	var project, environment, deployment string
	var jsonOutput bool
	cmd := &cobra.Command{Use: "get TEMPLATE", Short: "Inspect one deployed Template.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		item, err := cp.GetTemplateDefinition(cmd.Context(), args[0], client.DefinitionGetOptions{DeploymentID: deployment, EnvironmentScopeOptions: scope})
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), item)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "template: %s\ndeployment_id: %s\n", item.ID, item.DeploymentID)
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&deployment, "deployment", "", "Inspect this Deployment; omission uses the current Deployment.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON resource.")
	return cmd
}
