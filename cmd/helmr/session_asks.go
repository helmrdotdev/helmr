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
	cmd.AddCommand(sessionTurnAskListCommand(), sessionTurnAskGetCommand(), sessionTurnAskRespondCommand())
	return cmd
}

func sessionTurnAskListCommand() *cobra.Command {
	var project, environment, cursor string
	var limit int32
	var jsonOutput bool
	cmd := &cobra.Command{Use: "list SESSION_ID TURN_ID", Short: "Read one page of questions for a Turn.", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("limit") && limit < 1 {
			return errors.New("--limit must be in [1,100]")
		}
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		page, err := cp.ListTurnAsks(cmd.Context(), args[0], args[1], client.AskListOptions{Cursor: cursor, Limit: limit, EnvironmentScopeOptions: scope})
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), page)
		}
		for _, ask := range page.Asks {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", ask.ID, ask.Status, ask.Prompt)
		}
		if page.NextCursor != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", page.NextCursor)
		}
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&cursor, "cursor", "", "Continuation cursor from the previous page.")
	cmd.Flags().Int32Var(&limit, "limit", 0, "Maximum questions (default 50, maximum 100).")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON page.")
	return cmd
}

func sessionTurnAskGetCommand() *cobra.Command {
	var project, environment string
	var jsonOutput bool
	cmd := &cobra.Command{Use: "get SESSION_ID TURN_ID ASK_ID", Short: "Inspect a question and its answer control.", Args: cobra.ExactArgs(3), RunE: func(cmd *cobra.Command, args []string) error {
		cp, scope, err := scopedSessionClient(cmd, project, environment)
		if err != nil {
			return err
		}
		ask, err := cp.GetTurnAsk(cmd.Context(), args[0], args[1], args[2], scope)
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
		if len(ask.Prompt) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "prompt: %s\n", ask.Prompt)
		}
		if len(ask.AnswerControl) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "answer_control: %s\n", ask.AnswerControl)
		}
		if len(ask.Answer) > 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "answer: %s\n", ask.Answer)
		}
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON question.")
	return cmd
}

func sessionTurnAskRespondCommand() *cobra.Command {
	var project, environment, answerFile, answerJSON, responseID string
	var jsonOutput bool
	cmd := &cobra.Command{Use: "respond SESSION_ID TURN_ID ASK_ID", Short: "Answer exactly this question using current response authority.", Args: cobra.ExactArgs(3), RunE: func(cmd *cobra.Command, args []string) error {
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
		receipt, err := cp.RespondTurnAsk(cmd.Context(), args[0], args[1], args[2], api.RespondAgentAskRequest{Answer: answer, ResponseID: responseID}, scope)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), receipt)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "ask_id: %s\nstatus: %s\n", receipt.ID, receipt.Status)
		return nil
	}}
	addScopeFlags(cmd, &project, &environment)
	cmd.Flags().StringVar(&answerFile, "answer-file", "", "Read answer JSON from a file.")
	cmd.Flags().StringVar(&answerJSON, "answer-json", "", "Inline answer JSON literal.")
	cmd.Flags().StringVar(&responseID, "response-id", "", "Retry identity for this answer; reuse when reconnecting.")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Emit one JSON answer receipt.")
	cmd.MarkFlagsMutuallyExclusive("answer-file", "answer-json")
	return cmd
}
