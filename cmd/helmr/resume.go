package main

import (
	"errors"
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
		resumeApprovalCommand("approve", true),
		resumeApprovalCommand("deny", false),
		resumeMessageCommand(),
	)
	return cmd
}

func resumeApprovalCommand(name string, approve bool) *cobra.Command {
	var reason string
	short := "Deny an approval waitpoint."
	if approve {
		short = "Approve an approval waitpoint."
	}
	cmd := &cobra.Command{
		Use:   name + " RUN_ID WAITPOINT_ID",
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := controlClient()
			if err != nil {
				return err
			}
			request := api.ResumeApprovalRequest{Reason: reason}
			if approve {
				return client.ApproveWaitpoint(cmd.Context(), args[0], args[1], request)
			}
			return client.DenyWaitpoint(cmd.Context(), args[0], args[1], request)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Reason to store with the waitpoint resolution.")
	return cmd
}

func resumeMessageCommand() *cobra.Command {
	var text string
	cmd := &cobra.Command{
		Use:   "message RUN_ID WAITPOINT_ID",
		Short: "Reply to a message waitpoint.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(text) == "" {
				return errors.New("--text is required")
			}
			client, err := controlClient()
			if err != nil {
				return err
			}
			return client.MessageWaitpoint(cmd.Context(), args[0], args[1], api.ResumeMessageRequest{Text: text})
		},
	}
	cmd.Flags().StringVar(&text, "text", "", "Message text to send to the waiting run.")
	return cmd
}
