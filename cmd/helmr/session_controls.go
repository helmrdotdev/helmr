package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/spf13/cobra"
)

func sessionResumeCommand() *cobra.Command {
	var projectID, environmentID, key, holdID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "resume SESSION_ID --hold HOLD_ID",
		Short: "Resume queued work after an exact interruption hold has converged.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, scope, err := scopedSessionClient(cmd, projectID, environmentID)
			if err != nil {
				return err
			}
			receipt, err := controlPlane.ResumeSession(cmd.Context(), args[0], api.ResumeSessionRequest{
				HoldID: holdID, IdempotencyKey: strings.TrimSpace(key),
			}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), receipt)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "id: %s\nsession_id: %s\nhold_id: %s\nstatus: %s\n", receipt.ID, receipt.SessionID, receipt.HoldID, receipt.Status)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&holdID, "hold", "", "Exact hold ID from Session inspection.")
	_ = cmd.MarkFlagRequired("hold")
	cmd.Flags().StringVar(&key, "idempotency-key", "", "Idempotency key for this resume.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func sessionTurnCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "turn", Short: "Inspect or control an exact Session Turn."}
	cmd.AddCommand(sessionTurnAskCommand(), sessionTurnWaitCommand(), sessionTurnListCommand(), sessionTurnGetCommand(), sessionTurnSendCommand())
	return cmd
}

func sessionTurnGetCommand() *cobra.Command {
	var projectID, environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use: "get SESSION_ID TURN_ID", Short: "Show Turn outcome and message readiness.", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, scope, err := scopedSessionClient(cmd, projectID, environmentID)
			if err != nil {
				return err
			}
			turn, err := controlPlane.RetrieveSessionTurn(cmd.Context(), args[0], args[1], scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), turn)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "turn_id: %s\nsession_id: %s\nstatus: %s\nsequence: %d\n", turn.ID, turn.SessionID, turn.Status, turn.Sequence)
			if len(turn.Result) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "result: %s\n", turn.Result)
			}
			if turn.Error != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "error_code: %s\n", turn.Error.Code)
				if turn.Error.Message != nil && *turn.Error.Message != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "error_message: %s\n", *turn.Error.Message)
				}
			}
			if len(turn.Response) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "response: %s\n", turn.Response)
			}
			if turn.CompletionSaveID != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "completion_save_id: %s\n", *turn.CompletionSaveID)
			}
			if turn.PayloadExpiredAt != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "payload_expired_at: %s\n", turn.PayloadExpiredAt.Format(time.RFC3339Nano))
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func sessionTurnSendCommand() *cobra.Command {
	var projectID, environmentID, dataFile, dataJSON, key string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use: "send SESSION_ID TURN_ID", Short: "Send a message to exactly this Turn; never retarget it.", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := parseContentInput(cmd, dataFile, dataJSON, "--data")
			if err != nil {
				return err
			}
			controlPlane, scope, err := scopedSessionClient(cmd, projectID, environmentID)
			if err != nil {
				return err
			}
			receipt, err := controlPlane.SendSessionMessage(cmd.Context(), args[0], args[1], api.SessionDataRequest{Data: data, IdempotencyKey: strings.TrimSpace(key)}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), receipt)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "id: %s\nturn_id: %s\nmessage_id: %s\nstatus: %s\n", receipt.ID, receipt.TurnID, receipt.MessageID, receipt.Status)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&dataFile, "data-file", "", "Read text-part array JSON from a file.")
	cmd.Flags().StringVar(&dataJSON, "data-json", "", "Inline text-part array JSON literal.")
	cmd.Flags().StringVar(&key, "idempotency-key", "", "Idempotency key for this message.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.Flags().String("text", "", "Message text, preserving whitespace.")
	cmd.MarkFlagsMutuallyExclusive("text", "data-file", "data-json")
	return cmd
}
