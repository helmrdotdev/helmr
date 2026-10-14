package main

import (
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/spf13/cobra"
)

func sessionTurnAskCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ask", Short: "Inspect and answer exact Turn questions."}
	cmd.AddCommand(sessionTurnAskListCommand(), sessionTurnAskGetCommand(), sessionTurnAskAnswerCommand())
	return cmd
}

func sessionTurnAskListCommand() *cobra.Command {
	var sessionID, turnID string
	var project, environment, cursor string
	var limit int32
	var jsonOutput bool
	cmd := &cobra.Command{Use: "list --session SESSION_ID --turn TURN_ID", Short: "Read one page of questions for a Turn.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("limit") && limit < 1 {
			return errors.New("--limit must be in [1,100]")
		}
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		page, err := cp.ListTurnAsks(cmd.Context(), sessionID, turnID, client.AskListOptions{Cursor: cursor, Limit: limit, EnvironmentScopeOptions: scope})
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), page)
		}
		for _, ask := range page.Asks {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", ask.ID, ask.Status, ask.Question)
		}
		if page.NextCursor != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", page.NextCursor)
		}
		return nil
	}}
	cmd.Flags().StringVar(&sessionID, "session", "", "Parent Session ID.")
	_ = cmd.MarkFlagRequired("session")
	cmd.Flags().StringVar(&turnID, "turn", "", "Parent Turn ID.")
	_ = cmd.MarkFlagRequired("turn")
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continuation cursor from the previous page.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum questions (default 50, maximum 100).")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON page.")
	return cmd
}

func sessionTurnAskGetCommand() *cobra.Command {
	var sessionID, turnID string
	var project, environment string
	var jsonOutput bool
	cmd := &cobra.Command{Use: "get ASK_ID --session SESSION_ID --turn TURN_ID", Short: "Inspect a complete question and its answer.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		ask, err := cp.GetTurnAsk(cmd.Context(), sessionID, turnID, args[0], scope)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), ask)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "ask_id: %s\nstatus: %s\n", ask.ID, ask.Status)
		if ask.PayloadExpired {
			fmt.Fprintln(cmd.OutOrStdout(), "payload_expired: true")
		}
		if len(ask.Question) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "question: %s\n", ask.Question)
		}
		if len(ask.Answer) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "answer: %s\n", ask.Answer)
		}
		return nil
	}}
	cmd.Flags().StringVar(&sessionID, "session", "", "Parent Session ID.")
	_ = cmd.MarkFlagRequired("session")
	cmd.Flags().StringVar(&turnID, "turn", "", "Parent Turn ID.")
	_ = cmd.MarkFlagRequired("turn")
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON question.")
	return cmd
}

func sessionTurnAskAnswerCommand() *cobra.Command {
	var sessionID, turnID string
	var project, environment, answerFile, answerJSON, answerID string
	var jsonOutput bool
	cmd := &cobra.Command{Use: "answer ASK_ID --session SESSION_ID --turn TURN_ID", Short: "Answer exactly this question using current answer authority.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		answer, err := parseOptionalJSON(answerFile, answerJSON, "--answer")
		if err != nil {
			return err
		}
		if len(answer) == 0 {
			return errors.New("--answer-file or --answer-json is required")
		}
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		receipt, err := cp.AnswerTurnAsk(cmd.Context(), sessionID, turnID, args[0], api.AnswerAgentAskRequest{Answer: answer, AnswerID: answerID}, scope)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), receipt)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "ask_id: %s\nstatus: %s\n", receipt.ID, receipt.Status)
		return nil
	}}
	cmd.Flags().StringVar(&sessionID, "session", "", "Parent Session ID.")
	_ = cmd.MarkFlagRequired("session")
	cmd.Flags().StringVar(&turnID, "turn", "", "Parent Turn ID.")
	_ = cmd.MarkFlagRequired("turn")
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&answerFile, "answer-file", "", "Read answer JSON from a file.")
	cmd.Flags().StringVar(&answerJSON, "answer-json", "", "Inline answer JSON literal.")
	cmd.Flags().StringVar(&answerID, "answer-id", "", "Retry identity for this answer; reuse when reconnecting.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON answer receipt.")
	cmd.MarkFlagsMutuallyExclusive("answer-file", "answer-json")
	return cmd
}
