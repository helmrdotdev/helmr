package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/spf13/cobra"
)

func agentCommand() *cobra.Command {
	command := &cobra.Command{Use: "agent", Short: "Inspect deployed Agents and start work."}
	command.AddCommand(agentListCommand(), agentGetCommand(), agentStartCommand())
	return command
}

func agentStartCommand() *cobra.Command {
	var project, environment, inputJSON, inputFile, computer, key, retry string
	var routes []string
	var jsonOutput bool
	start := &cobra.Command{Use: "start AGENT", Short: "Start a Session and its first Turn.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		input, err := parseContentInput(cmd, inputFile, inputJSON, "--input")
		if err != nil {
			return err
		}
		request := api.StartAgentRequest{Input: input, ComputerID: computer, IdempotencyKey: retry}
		if cmd.Flags().Changed("session-key") {
			request.SessionKey = &key
		}
		if cmd.Flags().Changed("slack-channel") {
			if len(routes) != 1 {
				return errors.New("--slack-channel must be supplied exactly once")
			}
			if !definition.ValidSlackChannelID(routes[0]) {
				return errors.New("--slack-channel must be a Slack channel ID")
			}
			request.Slack, err = json.Marshal(struct {
				ChannelID string `json:"channel_id"`
			}{routes[0]})
			if err != nil {
				return err
			}
		}
		controlPlane, err := controlPlaneClient(cmd)
		if err != nil {
			return err
		}
		scope, err := environmentScopeForClient(cmd.Context(), controlPlane, project, environment)
		if err != nil {
			return err
		}
		result, err := controlPlane.StartAgent(cmd.Context(), args[0], request, scope)
		if err != nil {
			return err
		}
		if jsonOutput {
			return writeJSON(cmd.OutOrStdout(), result)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "session_id: %s\nturn_id: %s\n", result.SessionID, result.TurnID)
		return nil
	}}
	addScopeFlags(start, &project, &environment)
	start.Flags().StringVar(&inputJSON, "input-json", "", "Input as a JSON array of text parts.")
	start.Flags().StringVar(&inputFile, "input-file", "", "Read input JSON from a file.")
	start.Flags().StringVar(&computer, "computer", "", "Existing Computer ID; omission prepares a new Computer.")
	start.Flags().StringVar(&key, "session-key", "", "Explicit Session identity key.")
	start.Flags().StringVar(&retry, "idempotency-key", "", "Retry identity for this admission.")
	start.Flags().StringArrayVar(&routes, "slack-channel", nil, "Slack channel ID, such as C0123456789.")
	start.Flags().BoolVar(&jsonOutput, "json", false, "Emit the admission receipt as JSON.")
	start.Flags().String("text", "", "Message text, preserving whitespace.")
	start.MarkFlagsMutuallyExclusive("text", "input-json", "input-file")
	return start
}
