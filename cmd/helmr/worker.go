package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func workerCommand() *cobra.Command {
	worker := &cobra.Command{Use: "worker", Short: "Manage workers."}
	worker.AddCommand(workerRevokeCommand())
	return worker
}

func workerRevokeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke WORKER_ID",
		Short: "Revoke active credentials for a worker.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			control, err := controlClient()
			if err != nil {
				return err
			}
			revoked, err := control.RevokeWorkerCredentials(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%d\n", revoked.Revoked)
			return nil
		},
	}
}
