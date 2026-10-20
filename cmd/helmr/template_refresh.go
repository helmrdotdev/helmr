package main

import (
	"fmt"
	"github.com/spf13/cobra"
)

func templateRefreshCommand() *cobra.Command {
	var project, environment, key string
	var async, jsonOutput bool
	cmd := &cobra.Command{Use: "refresh TEMPLATE", Short: "Prepare a fresh image for a current Template.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		result, err := cp.RefreshTemplate(cmd.Context(), args[0], key, scope)
		if err != nil {
			return err
		}
		if !async {
			fmt.Fprintf(cmd.ErrOrStderr(), "Waiting for preparation %s...\n", result.ImageBuildID)
			result, err = cp.WaitTemplateBuild(cmd.Context(), result, scope)
			if err != nil {
				return err
			}
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), result)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "template: %s\ndeployment_id: %s\nimage_spec_id: %s\nimage_build_id: %s\nstatus: %s\n", result.TemplateID, result.DeploymentID, result.ImageSpecID, result.ImageBuildID, result.Status)
		return err
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&key, "idempotency-key", "", "Reuse the original refresh request, including after deployment promotion.")
	cmd.Flags().BoolVar(&async, "async", false, "Return the build receipt without waiting for preparation.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit the build receipt as JSON.")
	return cmd
}
