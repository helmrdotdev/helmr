package main

import (
	"fmt"

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
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(cmd.Context(), controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := controlPlane.ListDeployments(cmd.Context(), scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), response)
			}
			for _, deployment := range response.Deployments {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", deployment.ID, deployment.Version, deployment.BundleDigest)
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
			controlPlane, err := controlPlaneClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(cmd.Context(), controlPlane, projectID, environmentID)
			if err != nil {
				return err
			}
			deployment, err := controlPlane.GetDeployment(cmd.Context(), args[0], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), deployment)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deployment: %s\n", deployment.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "Version:    %s\n", deployment.Version)
			fmt.Fprintf(cmd.OutOrStdout(), "Bundle:     %s\n", deployment.BundleDigest)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}
