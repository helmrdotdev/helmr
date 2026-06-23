package main

import (
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/cli/format"
	"github.com/spf13/cobra"
)

func sandboxCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Work with deployment sandboxes.",
	}
	cmd.AddCommand(sandboxListCommand(), sandboxGetCommand())
	return cmd
}

func sandboxListCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List sandboxes in the current deployment.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			control, err := controlClient(cmd)
			if err != nil {
				return err
			}
			scope, err := environmentScopeForClient(control, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := control.ListSandboxes(cmd.Context(), scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), response)
			}
			for _, sandbox := range response.Sandboxes {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", sandbox.SandboxID, sandbox.Fingerprint, sandbox.RuntimeABI, sandbox.WorkspaceMountPath)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func sandboxGetCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get SANDBOX",
		Short: "Show sandbox details.",
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
			sandbox, err := control.GetSandbox(cmd.Context(), args[0], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return format.JSON(cmd.OutOrStdout(), sandbox)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Sandbox:    %s\n", sandbox.SandboxID)
			fmt.Fprintf(cmd.OutOrStdout(), "Fingerprint:%s\n", strings.TrimSpace(sandbox.Fingerprint))
			fmt.Fprintf(cmd.OutOrStdout(), "Runtime ABI:%s\n", sandbox.RuntimeABI)
			fmt.Fprintf(cmd.OutOrStdout(), "Mount:      %s\n", sandbox.WorkspaceMountPath)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}
