package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

func sessionCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "session", Short: "Work with Actor Sessions."}
	cmd.AddCommand(sessionGetCommand(), sessionSendCommand(false), sessionSendCommand(true), sessionResumeCommand(), sessionTurnCommand(), sessionEventsCommand(), sessionCloseCommand(), sessionCancelCommand())
	return cmd
}

func sessionGetCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "get SESSION_ID",
		Short: "Show Session status.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, scope, err := scopedActorClient(cmd, projectID, environmentID)
			if err != nil {
				return err
			}
			session, err := controlPlane.RetrieveSession(cmd.Context(), args[0], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), session)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "session_id: %s\n", session.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "session_status: %s\n", session.Status)
			fmt.Fprintf(cmd.OutOrStdout(), "dispatch: %s\n", session.Dispatch.State)
			if session.Dispatch.HoldID != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "hold_id: %s\n", *session.Dispatch.HoldID)
			}
			if session.Dispatch.Reason != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "hold_reason: %s\n", *session.Dispatch.Reason)
			}
			if session.ActiveTurnID != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "turn_id: %s\n", *session.ActiveTurnID)
			}
			if session.CurrentRunID != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "run_id: %s\n", *session.CurrentRunID)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func sessionSendCommand(enqueue bool) *cobra.Command {
	operation, description := "send", "Send data to active work or enqueue a new Turn."
	if enqueue {
		operation, description = "enqueue", "Enqueue a new Turn, including while another Turn is active."
	}
	var projectID string
	var environmentID string
	var dataFile string
	var dataJSON string
	var idempotencyKey string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   operation + " SESSION_ID",
		Short: description,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			input, err := parseOptionalJSON(dataFile, dataJSON, "--data")
			if err != nil {
				return err
			}
			if len(input) == 0 {
				return errors.New("--data-file or --data-json is required")
			}
			controlPlane, scope, err := scopedActorClient(cmd, projectID, environmentID)
			if err != nil {
				return err
			}
			send := controlPlane.SendSession
			if enqueue {
				send = controlPlane.EnqueueSession
			}
			response, err := send(cmd.Context(), args[0], api.SessionDataRequest{
				Data: input, IdempotencyKey: strings.TrimSpace(idempotencyKey),
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), response)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "id: %s\n", response.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "kind: %s\n", response.Kind)
			fmt.Fprintf(cmd.OutOrStdout(), "turn_id: %s\n", response.TurnID)
			if response.MessageID != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "message_id: %s\n", *response.MessageID)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&dataFile, "data-file", "", "Read application data JSON from a file.")
	cmd.Flags().StringVar(&dataJSON, "data-json", "", "Inline application data JSON literal.")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for this send.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.MarkFlagsMutuallyExclusive("data-file", "data-json")
	return cmd
}

func sessionEventsCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var after int64
	var limit int32
	var jsonOutput bool
	var jsonLines bool
	cmd := &cobra.Command{
		Use:   "events SESSION_ID",
		Short: "Read one finite Session event page.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("limit") && limit < 1 {
				return errors.New("--limit must be in [1,1000]")
			}
			controlPlane, scope, err := scopedActorClient(cmd, projectID, environmentID)
			if err != nil {
				return err
			}
			page, err := controlPlane.ReadSessionEvents(cmd.Context(), args[0], client.SessionEventReadOptions{
				After: after, Limit: limit, EnvironmentScopeOptions: scope,
			})
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), page)
			}
			if jsonLines {
				return writeJSONLines(cmd.OutOrStdout(), page.Records)
			}
			for _, record := range page.Records {
				fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\t%s\n", record.Sequence, record.Kind, record.Data)
			}
			if page.HasMore {
				fmt.Fprintf(cmd.OutOrStdout(), "next_after: %d\n", page.NextAfter)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().Int64Var(&after, "after", 0, "Return records after this durable sequence.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum events (default 100, maximum 1000).")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.Flags().BoolVar(&jsonLines, "jsonl", false, "Emit one JSON record per line.")
	cmd.MarkFlagsMutuallyExclusive("json", "jsonl")
	return cmd
}

func sessionCloseCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var idempotencyKey string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "close SESSION_ID",
		Short: "Close a Session.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, scope, err := scopedActorClient(cmd, projectID, environmentID)
			if err != nil {
				return err
			}
			receipt, err := controlPlane.CloseSession(cmd.Context(), args[0], api.CloseSessionRequest{
				IdempotencyKey: strings.TrimSpace(idempotencyKey),
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), receipt)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "session_id: %s\n", receipt.SessionID)
			fmt.Fprintf(cmd.OutOrStdout(), "id: %s\n", receipt.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "status: %s\n", receipt.Status)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for this close.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func sessionCancelCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var idempotencyKey string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "cancel SESSION_ID",
		Short: "Cancel a Session.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, scope, err := scopedActorClient(cmd, projectID, environmentID)
			if err != nil {
				return err
			}
			receipt, err := controlPlane.CancelSession(cmd.Context(), args[0], api.CancelSessionRequest{
				IdempotencyKey: strings.TrimSpace(idempotencyKey),
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), receipt)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "session_id: %s\n", receipt.SessionID)
			fmt.Fprintf(cmd.OutOrStdout(), "id: %s\n", receipt.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "status: %s\n", receipt.Status)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for this cancel.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func scopedActorClient(
	cmd *cobra.Command,
	projectID string,
	environmentID string,
) (*client.Client, client.EnvironmentScopeOptions, error) {
	controlPlane, err := controlPlaneClient(cmd)
	if err != nil {
		return nil, client.EnvironmentScopeOptions{}, err
	}
	scope, err := environmentScopeForClient(cmd.Context(), controlPlane, projectID, environmentID)
	return controlPlane, scope, err
}
