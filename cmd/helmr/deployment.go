package main

import (
	"fmt"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cli/format"
	"github.com/spf13/cobra"
)

func deploymentCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deployment",
		Short: "Work with deployments.",
	}
	cmd.AddCommand(deploymentListCommand(), deploymentGetCommand(), promoteCommand())
	return cmd
}

func deploymentListCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List deployments.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.ListDeployments(cmd.Context(), scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			for _, deployment := range response.Deployments {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", deployment.ID, deployment.Version, deployment.Status)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func deploymentGetCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get DEPLOYMENT",
		Short: "Show deployment details.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			deployment, err := control.GetDeployment(cmd.Context(), args[0], api.GetDeploymentRequest{
				ProjectID:     scope.ProjectID,
				EnvironmentID: scope.EnvironmentID,
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), deployment)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deployment: %s\n", deployment.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "Version:    %s\n", deployment.Version)
			fmt.Fprintf(cmd.OutOrStdout(), "Status:     %s\n", deployment.Status)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}
