package main

import (
	"encoding/json"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/spf13/cobra"
)

func resumeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume",
		Short: "Resolve a waiting run.",
	}
	cmd.AddCommand(
		resumeCompleteCommand(),
	)
	return cmd
}

func resumeCompleteCommand() *cobra.Command {
	var value string
	cmd := &cobra.Command{
		Use:   "complete RUN_ID WAITPOINT_ID",
		Short: "Complete a waitpoint.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := controlClient()
			if err != nil {
				return err
			}
			request := api.CompleteWaitpointTokenRequest{}
			if strings.TrimSpace(value) != "" {
				request.Value = json.RawMessage(value)
			}
			return client.CompleteWaitpoint(cmd.Context(), args[0], args[1], request)
		},
	}
	cmd.Flags().StringVar(&value, "value", "", "JSON value to return to the waiting run.")
	return cmd
}
