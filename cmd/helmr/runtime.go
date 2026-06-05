package main

import (
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/spf13/cobra"
)

func runtimeCommand() *cobra.Command {
	runtime := &cobra.Command{Use: "runtime", Short: "Manage runtime releases."}
	runtime.AddCommand(runtimePromoteCommand())
	return runtime
}

func runtimePromoteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "promote RUNTIME_ID",
		Short: "Promote an observed runtime release to current.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runtimeID := strings.TrimSpace(args[0])
			if runtimeID == "" {
				return fmt.Errorf("runtime_id is required")
			}
			control, err := sessionControlClient()
			if err != nil {
				return err
			}
			promoted, err := control.PromoteRuntimeRelease(cmd.Context(), api.PromoteRuntimeReleaseRequest{RuntimeID: runtimeID})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", promoted.RuntimeID, promoted.SelectedAt.Format("2006-01-02T15:04:05Z07:00"))
			return nil
		},
	}
	return cmd
}
