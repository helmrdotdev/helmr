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
		resumeRespondCommand(),
	)
	return cmd
}

func resumeRespondCommand() *cobra.Command {
	var value string
	cmd := &cobra.Command{
		Use:   "respond WAITPOINT_ID",
		Short: "Respond to a waitpoint.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := controlClient()
			if err != nil {
				return err
			}
			request := api.RespondWaitpointRequest{}
			if strings.TrimSpace(value) != "" {
				request.Value = json.RawMessage(value)
			}
			return client.RespondWaitpoint(cmd.Context(), args[0], request)
		},
	}
	cmd.Flags().StringVar(&value, "value", "", "JSON value to return to the waiting run.")
	return cmd
}
