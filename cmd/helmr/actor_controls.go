package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/spf13/cobra"
)

func actorResumeCommand() *cobra.Command {
	var projectID, environmentID, key, holdID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "resume SESSION_ID [--hold HOLD_ID]",
		Short: "Resume queued work after an exact interruption hold has converged.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, scope, err := scopedActorClient(cmd, projectID, environmentID)
			if err != nil {
				return err
			}
			targetHold := holdID
			if targetHold == "" {
				session, err := controlPlane.RetrieveSession(cmd.Context(), args[0], scope)
				if err != nil {
					return err
				}
				if session.Dispatch.HoldID == nil {
					return errors.New("session has no hold to resume")
				}
				if session.Dispatch.Reason != nil && *session.Dispatch.Reason == "recovery_required" {
					return errors.New("session requires reconciliation; use actor recover")
				}
				targetHold = *session.Dispatch.HoldID
			}
			receipt, err := controlPlane.ResumeSession(cmd.Context(), args[0], api.ResumeSessionRequest{
				HoldID: targetHold, IdempotencyKey: strings.TrimSpace(key),
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
	cmd.Flags().StringVar(&holdID, "hold", "", "Exact hold ID; defaults to the currently observed hold.")
	cmd.Flags().StringVar(&key, "idempotency-key", "", "Idempotency key for this resume; use --hold when retrying an uncertain request.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func actorRecoverCommand() *cobra.Command {
	var projectID, environmentID, key, turnID, workspaceVersion, reconciliation, disposition string
	var outsideTurn, jsonOutput bool
	cmd := &cobra.Command{
		Use:   "recover SESSION_ID HOLD_ID",
		Short: "Reconcile an uncertain execution using an exact hold and Workspace version.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (turnID != "") == outsideTurn {
				return errors.New("exactly one of --turn or --outside-turn is required")
			}
			if workspaceVersion == "" || strings.TrimSpace(reconciliation) == "" {
				return errors.New("--workspace-version and --reconciliation-ref are required")
			}
			input := api.RecoverSessionRequest{
				HoldID: args[1], WorkspaceVersionID: workspaceVersion,
				ReconciliationRef: reconciliation, Disposition: disposition, IdempotencyKey: strings.TrimSpace(key),
			}
			if !outsideTurn {
				input.TurnID = &turnID
			}
			if err := api.ValidateRecoverSessionRequest(input); err != nil {
				return err
			}
			controlPlane, scope, err := scopedActorClient(cmd, projectID, environmentID)
			if err != nil {
				return err
			}
			receipt, err := controlPlane.RecoverSession(cmd.Context(), args[0], input, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), receipt)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "id: %s\nsession_id: %s\nhold_id: %s\nstatus: %s\n", receipt.ID, receipt.SessionID, receipt.HoldID, receipt.Status)
			if receipt.TurnID != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "turn_id: %s\n", *receipt.TurnID)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&turnID, "turn", "", "Exact Turn being reconciled.")
	cmd.Flags().BoolVar(&outsideTurn, "outside-turn", false, "Explicitly reconcile execution outside a Turn (turn_id null).")
	cmd.Flags().StringVar(&workspaceVersion, "workspace-version", "", "Reconciled Workspace version ID (required).")
	cmd.Flags().StringVar(&reconciliation, "reconciliation-ref", "", "Reference to the external-effect reconciliation record (required).")
	cmd.Flags().StringVar(&disposition, "disposition", "", "Turn disposition: failed or interrupted; omit with --outside-turn.")
	cmd.Flags().StringVar(&key, "idempotency-key", "", "Idempotency key for this recovery.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.MarkFlagsMutuallyExclusive("turn", "outside-turn")
	return cmd
}

func actorTurnCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "turn", Short: "Inspect or control an exact Session Turn."}
	cmd.AddCommand(actorTurnGetCommand(), actorTurnSendCommand(), actorTurnInterruptCommand())
	return cmd
}

func actorTurnGetCommand() *cobra.Command {
	var projectID, environmentID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use: "get SESSION_ID TURN_ID", Short: "Show Turn outcome and message readiness.", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, scope, err := scopedActorClient(cmd, projectID, environmentID)
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
			fmt.Fprintf(cmd.OutOrStdout(), "turn_id: %s\nsession_id: %s\nstatus: %s\naccepts_messages: %t\ninterrupt_requested: %t\n", turn.ID, turn.SessionID, turn.Status, turn.AcceptsMessages, turn.InterruptRequested)
			if len(turn.Result) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "result: %s\n", turn.Result)
			}
			if len(turn.Error) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "error: %s\n", turn.Error)
			}
			if turn.TerminalEventID != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "terminal_event_id: %s\n", *turn.TerminalEventID)
			}
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}

func actorTurnSendCommand() *cobra.Command {
	var projectID, environmentID, dataFile, dataJSON, key string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use: "send SESSION_ID TURN_ID", Short: "Send a message to exactly this Turn; never retarget it.", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := parseOptionalJSON(dataFile, dataJSON, "--data")
			if err != nil {
				return err
			}
			if len(data) == 0 {
				return errors.New("--data-file or --data-json is required")
			}
			controlPlane, scope, err := scopedActorClient(cmd, projectID, environmentID)
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
	cmd.Flags().StringVar(&dataFile, "data-file", "", "Read application message JSON from a file.")
	cmd.Flags().StringVar(&dataJSON, "data-json", "", "Inline application message JSON literal.")
	cmd.Flags().StringVar(&key, "idempotency-key", "", "Idempotency key for this message.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	cmd.MarkFlagsMutuallyExclusive("data-file", "data-json")
	return cmd
}

func actorTurnInterruptCommand() *cobra.Command {
	var projectID, environmentID, key string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use: "interrupt SESSION_ID TURN_ID", Short: "Request interruption of exactly this Turn and retain queued work.", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			controlPlane, scope, err := scopedActorClient(cmd, projectID, environmentID)
			if err != nil {
				return err
			}
			receipt, err := controlPlane.InterruptSessionTurn(cmd.Context(), args[0], args[1], api.InterruptTurnRequest{IdempotencyKey: strings.TrimSpace(key)}, scope)
			if err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), receipt)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "id: %s\nsession_id: %s\nturn_id: %s\nhold_id: %s\nstatus: %s\n", receipt.ID, receipt.SessionID, receipt.TurnID, receipt.HoldID, receipt.Status)
			return nil
		},
	}
	addScopeFlags(cmd, &projectID, &environmentID)
	cmd.Flags().StringVar(&key, "idempotency-key", "", "Idempotency key for this interruption.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON object.")
	return cmd
}
