package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

func sessionListCommand() *cobra.Command {
	var project, environment, agent, key, parent, requester, cursor string
	var statuses []string
	var limit int32
	var jsonOutput bool
	cmd := &cobra.Command{Use: "list", Short: "Read one page of retained Sessions.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("limit") && limit < 1 {
			return errors.New("--limit must be in [1,100]")
		}
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		opts := client.SessionListOptions{AgentID: agent, ParentSessionID: parent, RequesterSessionID: requester, Statuses: statuses, Cursor: cursor, Limit: limit, EnvironmentScopeOptions: scope}
		if cmd.Flags().Changed("key") {
			opts.Key = &key
		}
		page, err := cp.ListSessions(cmd.Context(), opts)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), page)
		}
		for _, session := range page.Sessions {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", session.ID, session.AgentID, session.Status, session.ComputerID)
		}
		if page.NextCursor != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", page.NextCursor)
		}
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&agent, "agent", "", "Filter by Agent definition UUID.")
	cmd.Flags().StringVar(&key, "key", "", "Filter by explicit Session key; requires --agent.")
	cmd.Flags().StringVar(&parent, "parent-session", "", "Filter by owning parent Session UUID.")
	cmd.Flags().StringVar(&requester, "requester-session", "", "Filter by requesting Session UUID.")
	cmd.Flags().StringSliceVar(&statuses, "status", nil, "Filter by open, closing, closed or cancelled status.")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continuation cursor from the previous page.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum Sessions (default 50, maximum 100).")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON page.")
	return cmd
}

func sessionTurnListCommand() *cobra.Command {
	var project, environment, cursor string
	var limit int32
	var jsonOutput bool
	cmd := &cobra.Command{Use: "list SESSION_ID", Short: "Read one page of Turns in admission order.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("limit") && limit < 1 {
			return errors.New("--limit must be in [1,100]")
		}
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		page, err := cp.ListSessionTurns(cmd.Context(), args[0], client.SessionTurnListOptions{Cursor: cursor, Limit: limit, EnvironmentScopeOptions: scope})
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), page)
		}
		for _, turn := range page.Turns {
			fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\t%s\n", turn.Sequence, turn.ID, turn.Status)
		}
		if page.NextCursor != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", page.NextCursor)
		}
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continuation cursor from the previous page.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum Turns (default 50, maximum 100).")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON page.")
	return cmd
}

func sessionInterruptCommand() *cobra.Command {
	var project, environment, key string
	var jsonOutput bool
	cmd := &cobra.Command{Use: "interrupt SESSION_ID", Short: "Interrupt active work and hold queued Turns.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		receipt, err := cp.InterruptSession(cmd.Context(), args[0], api.InterruptSessionRequest{IdempotencyKey: strings.TrimSpace(key)}, scope)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), receipt)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "id: %s\nsession_id: %s\nhold_id: %s\nstatus: %s\n", receipt.ID, receipt.SessionID, receipt.HoldID, receipt.Status)
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&key, "idempotency-key", "", "Idempotency key for this interruption.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON receipt.")
	return cmd
}
