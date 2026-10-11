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
	cmd := &cobra.Command{Use: "session", Short: "Work with Agent Sessions."}
	cmd.AddCommand(sessionListCommand(), sessionInterruptCommand(), sessionGetCommand(), sessionSendCommand(), sessionEnqueueCommand(), sessionResumeCommand(), sessionTurnCommand(), sessionEventsCommand(), sessionCloseCommand(), sessionCancelCommand())
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
			controlPlane, scope, err := scopedSessionClient(cmd, projectID, environmentID)
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
			fmt.Fprintf(cmd.OutOrStdout(), "agent_id: %s\ncomputer_id: %s\n", session.AgentID, session.ComputerID)
			for _, hold := range session.Holds {
				fmt.Fprintf(cmd.OutOrStdout(), "hold_id: %s\nhold_session_id: %s\nhold_scope: %s\n", hold.ID, hold.SessionID, hold.Scope)
				if hold.Reason != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "hold_reason: %s\n", hold.Reason)
				}
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func sessionSendCommand() *cobra.Command {
	var projectID string
	var environmentID string
	var dataFile string
	var dataJSON string
	var idempotencyKey string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "send SESSION_ID",
		Short: "Send data to active work or enqueue a new Turn.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			input, err := parseContentInput(cmd, dataFile, dataJSON, "--data")
			if err != nil {
				return err
			}
			controlPlane, scope, err := scopedSessionClient(cmd, projectID, environmentID)
			if err != nil {
				return err
			}
			response, err := controlPlane.SendSession(cmd.Context(), args[0], api.SessionDataRequest{Data: input, IdempotencyKey: strings.TrimSpace(idempotencyKey)}, scope)
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
	cmd.Flags().StringVar(&dataFile, "data-file", "", "Read text-part array JSON from a file.")
	cmd.Flags().StringVar(&dataJSON, "data-json", "", "Inline text-part array JSON literal.")
	cmd.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key for this send.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.Flags().String("text", "", "Message text, preserving whitespace.")
	cmd.MarkFlagsMutuallyExclusive("text", "data-file", "data-json")
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
			controlPlane, scope, err := scopedSessionClient(cmd, projectID, environmentID)
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
			controlPlane, scope, err := scopedSessionClient(cmd, projectID, environmentID)
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
			controlPlane, scope, err := scopedSessionClient(cmd, projectID, environmentID)
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

func scopedSessionClient(
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

func sessionEnqueueCommand() *cobra.Command {
	var project, environment, inputFile, inputJSON, key string
	var jsonOutput bool
	cmd := &cobra.Command{Use: "enqueue SESSION_ID", Short: "Enqueue a new Turn, including while another Turn is active.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		input, err := parseContentInput(cmd, inputFile, inputJSON, "--input")
		if err != nil {
			return err
		}
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		receipt, err := cp.EnqueueSession(cmd.Context(), args[0], api.EnqueueSessionRequest{Input: input, IdempotencyKey: strings.TrimSpace(key)}, scope)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), receipt)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "session_id: %s\nturn_id: %s\nsequence: %d\n", receipt.SessionID, receipt.TurnID, receipt.Sequence)
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&inputFile, "input-file", "", "Read Turn input JSON from a file.")
	cmd.Flags().StringVar(&inputJSON, "input-json", "", "Inline Turn input JSON literal.")
	cmd.Flags().StringVar(&key, "idempotency-key", "", "Idempotency key for this admission.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON admission receipt.")
	cmd.Flags().String("text", "", "Message text, preserving whitespace.")
	cmd.MarkFlagsMutuallyExclusive("text", "input-file", "input-json")
	return cmd
}
